package autopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
)

// +kubebuilder:rbac:groups=core,resources=nodes/proxy,verbs=get
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=get;create

// The manager's cached client cannot proxy to kubelets, exec into pods or read aggregated
// APIs, so this package keeps an uncached clientset for those calls.
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

// kubeletSummary is the subset of the kubelet /stats/summary response the operator reads.
type kubeletSummary struct {
	Pods []struct {
		Volume []struct {
			Name   string `json:"name"`
			PVCRef *struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
			CapacityBytes *uint64 `json:"capacityBytes"`
			UsedBytes     *uint64 `json:"usedBytes"`
		} `json:"volume"`
	} `json:"pods"`
}

// volumeStat is a kubelet-reported PVC usage.
type volumeStat struct {
	capacity, used int64
}

// nodeVolumeStats returns kubelet volume stats of the PVCs in namespace mounted on node.
func nodeVolumeStats(ctx context.Context, node, namespace string) (map[string]volumeStat, error) {
	cs, _, err := kube()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := cs.CoreV1().RESTClient().Get().Resource("nodes").Name(node).SubResource("proxy").Suffix("stats/summary").DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	var sum kubeletSummary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return nil, err
	}
	out := map[string]volumeStat{}
	for _, p := range sum.Pods {
		for _, v := range p.Volume {
			if v.PVCRef == nil || v.PVCRef.Namespace != namespace || v.CapacityBytes == nil || v.UsedBytes == nil {
				continue
			}
			out[v.PVCRef.Name] = volumeStat{capacity: int64(*v.CapacityBytes), used: int64(*v.UsedBytes)}
		}
	}
	return out, nil
}

var nodeCount struct {
	sync.Mutex
	at    time.Time
	count int
}

// schedulableNodes counts Ready, schedulable nodes (cached for 10 minutes).
func schedulableNodes(ctx context.Context) (int, error) {
	nodeCount.Lock()
	defer nodeCount.Unlock()
	if !nodeCount.at.IsZero() && time.Since(nodeCount.at) < 10*time.Minute {
		return nodeCount.count, nil
	}
	cs, _, err := kube()
	if err != nil {
		return 0, err
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, node := range list.Items {
		if node.Spec.Unschedulable {
			continue
		}
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				n++
			}
		}
	}
	nodeCount.at, nodeCount.count = time.Now(), n
	return n, nil
}
