package core

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ReconcileNetwork ensures the HTTP/SSH Services and the Ingress.
func ReconcileNetwork(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if err := reconcileService(ctx, rc, forgejo.HTTPServiceName(f), f.Spec.Server.HTTP, "http", forgejo.HTTPServicePort(f)); err != nil {
		return forgejo.StepResult{}, err
	}

	if forgejo.SSHEnabled(f) {
		if err := reconcileService(ctx, rc, forgejo.SSHServiceName(f), f.Spec.Server.SSH.Service, "ssh", forgejo.SSHServicePort(f)); err != nil {
			return forgejo.StepResult{}, err
		}
	} else if err := deleteOwned(ctx, rc, &corev1.Service{}, forgejo.SSHServiceName(f)); err != nil {
		return forgejo.StepResult{}, err
	}

	if ingressEnabled(f) {
		if err := reconcileIngress(ctx, rc); err != nil {
			return forgejo.StepResult{}, err
		}
	} else if err := deleteOwned(ctx, rc, &networkingv1.Ingress{}, forgejo.IngressName(f)); err != nil {
		return forgejo.StepResult{}, err
	}
	return forgejo.StepResult{}, nil
}

func reconcileService(ctx context.Context, rc *forgejo.ReconcileContext, name string, spec forgejov1.ServiceSpec, portName string, port int32) error {
	f := rc.Instance
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace}}
	err := apply(ctx, rc, svc, true, func() error {
		svc.Labels = mergeMaps(svc.Labels, forgejo.Labels(f, forgejo.ComponentServer))
		svc.Annotations = mergeMaps(svc.Annotations, spec.Annotations)

		svcType := spec.Type
		if svcType == "" {
			svcType = corev1.ServiceTypeClusterIP
		}
		var nodePort int32
		if svcType != corev1.ServiceTypeClusterIP {
			nodePort = spec.NodePort
			// Keep the allocated port; zero would make the API server pick a new one.
			if nodePort == 0 && len(svc.Spec.Ports) > 0 {
				nodePort = svc.Spec.Ports[0].NodePort
			}
		}
		svc.Spec.Type = svcType
		svc.Spec.Selector = forgejo.SelectorLabels(f)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       portName,
			Port:       port,
			TargetPort: intstr.FromString(portName),
			Protocol:   corev1.ProtocolTCP,
			NodePort:   nodePort,
		}}
		if svcType == corev1.ServiceTypeLoadBalancer {
			// Immutable once set.
			if svc.Spec.LoadBalancerClass == nil {
				svc.Spec.LoadBalancerClass = spec.LoadBalancerClass
			}
		} else {
			svc.Spec.LoadBalancerClass = nil
			svc.Spec.ExternalTrafficPolicy = ""
			svc.Spec.AllocateLoadBalancerNodePorts = nil
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("service %s: %w", name, err)
	}
	return nil
}

func reconcileIngress(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	spec := f.Spec.Ingress
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: forgejo.IngressName(f), Namespace: f.Namespace}}
	err := apply(ctx, rc, ing, true, func() error {
		ing.Labels = mergeMaps(ing.Labels, forgejo.Labels(f, forgejo.ComponentServer))
		ing.Annotations = mergeMaps(ing.Annotations, spec.Annotations)
		pathType := networkingv1.PathTypePrefix
		// The DefaultIngressClass admission plugin fills in the cluster default class on
		// create only. Keep it (or one set by hand) when className is unset, or the next
		// update would strip it and controllers that ignore class-less Ingresses drop the route.
		class := ing.Spec.IngressClassName
		if spec.ClassName != nil {
			class = spec.ClassName
		}
		ing.Spec = networkingv1.IngressSpec{
			IngressClassName: class,
			Rules: []networkingv1.IngressRule{{
				Host: spec.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     ingressPath(f),
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: forgejo.HTTPServiceName(f),
							Port: networkingv1.ServiceBackendPort{Name: portNameHTTP},
						}},
					}},
				}},
			}},
		}
		if secret := ingressTLSSecret(f); secret != "" {
			tls := networkingv1.IngressTLS{SecretName: secret}
			if spec.Host != "" {
				tls.Hosts = []string{spec.Host}
			}
			ing.Spec.TLS = []networkingv1.IngressTLS{tls}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ingress: %w", err)
	}
	return nil
}

// deleteOwned deletes the named object if it exists and is controlled by the instance.
func deleteOwned(ctx context.Context, rc *forgejo.ReconcileContext, obj client.Object, name string) error {
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: rc.Instance.Namespace, Name: name}, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if owner := metav1.GetControllerOf(obj); owner == nil || owner.UID != rc.Instance.UID {
		return nil
	}
	if err := rc.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting %s: %w", name, err)
	}
	return nil
}
