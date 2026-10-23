package autopilot

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
	ctrl "sigs.k8s.io/controller-runtime"
)

// pods/exec is used only to measure volume usage (df, du) in containers that mount an
// instance's claims. Kubelet stats (nodes/proxy) are deliberately not used: that permission
// opens the whole kubelet API of every node, including exec into any pod.
//
// It is deliberately not a +kubebuilder:rbac marker, so manager-role does not carry it: it is
// ClusterRole volume-usage (config/rbac/volume_usage_role.yaml), bound only where volume
// measurement is wanted (cluster-wide by the kustomize component config/components/volume-usage,
// or per namespace; see docs/permissions.md). Without it every measurement is refused and
// StorageHealthy is Unknown/MeasurementUnavailable.

// The manager's cached client cannot exec into pods or read aggregated APIs, so this package
// keeps an uncached clientset for those calls.
var (
	kubeMu     sync.Mutex
	kubeConfig *rest.Config
	kubeClient kubernetes.Interface
)

// SetRESTConfig provides the manager's rest.Config. Without it the package loads the same
// configuration (kubeconfig flag/env or in-cluster) on first use.
func SetRESTConfig(cfg *rest.Config) {
	kubeMu.Lock()
	defer kubeMu.Unlock()
	kubeConfig, kubeClient = rest.CopyConfig(cfg), nil
}

func kube() (kubernetes.Interface, *rest.Config, error) {
	kubeMu.Lock()
	defer kubeMu.Unlock()
	if kubeClient != nil {
		return kubeClient, kubeConfig, nil
	}
	if kubeConfig == nil {
		cfg, err := ctrl.GetConfig()
		if err != nil {
			return nil, nil, err
		}
		kubeConfig = cfg
	}
	cs, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, nil, err
	}
	kubeClient = cs
	return kubeClient, kubeConfig, nil
}

// execFunc runs a command in a container and returns its stdout. Tests replace execInPod.
type execFunc func(ctx context.Context, namespace, pod, container string, command []string) (string, error)

var execFn execFunc = execInPod

// execInPod runs a command in a container and returns its stdout. A non-zero exit status is
// returned as an error together with whatever was written to stdout.
func execInPod(ctx context.Context, namespace, pod, container string, command []string) (string, error) {
	cs, cfg, err := kube()
	if err != nil {
		return "", err
	}
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(cfg, "GET", req.URL().String())
	if err != nil {
		return "", err
	}
	spdy, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), err
}

// execForbidden reports whether an exec failed because the operator may not exec into the
// pod (pods/exec not granted in that namespace).
func execForbidden(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsForbidden(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "forbidden")
}

var nodeInfo struct {
	sync.Mutex
	at     time.Time
	count  int
	maxMem int64 // largest allocatable memory of a Ready, schedulable node (bytes)
}

// schedulableNodes counts Ready, schedulable nodes (cached for 10 minutes).
func schedulableNodes(ctx context.Context) (int, error) {
	n, _, err := nodeCapacity(ctx)
	return n, err
}

// nodeCapacityFn is nodeCapacity; tests replace it.
var nodeCapacityFn = nodeCapacity

// nodeCapacity returns the number of Ready, schedulable nodes and the largest allocatable
// memory among them, cached for 10 minutes.
func nodeCapacity(ctx context.Context) (int, int64, error) {
	nodeInfo.Lock()
	defer nodeInfo.Unlock()
	if !nodeInfo.at.IsZero() && time.Since(nodeInfo.at) < 10*time.Minute {
		return nodeInfo.count, nodeInfo.maxMem, nil
	}
	cs, _, err := kube()
	if err != nil {
		return 0, 0, err
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, err
	}
	n, maxMem := 0, int64(0)
	for _, node := range list.Items {
		if node.Spec.Unschedulable {
			continue
		}
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				n++
				maxMem = max(maxMem, node.Status.Allocatable.Memory().Value())
			}
		}
	}
	nodeInfo.at, nodeInfo.count, nodeInfo.maxMem = time.Now(), n, maxMem
	return n, maxMem, nil
}
