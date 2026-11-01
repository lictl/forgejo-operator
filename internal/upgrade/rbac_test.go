package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

// managerRole reads config/rbac/role.yaml as "group/resource" -> verbs. make test regenerates
// it from the +kubebuilder:rbac markers first.
func managerRole(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, r := range role.Rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				out[g+"/"+res] = append(out[g+"/"+res], r.Verbs...)
			}
		}
	}
	return out
}

var errApplyUnchecked = errors.New("rbacGuard does not check server-side apply")

// rbacGuard wraps live so that every call manager-role does not grant fails as Forbidden, as the
// API server refuses it. With cached (the manager's client), typed reads also need list and
// watch, which its informers use; unstructured reads go to the API server.
func rbacGuard(t *testing.T, live client.WithWatch, cached bool) client.WithWatch {
	t.Helper()
	role := managerRole(t)
	check := func(c client.Client, obj runtime.Object, subResource string, verbs ...string) error {
		gvk, err := apiutil.GVKForObject(obj, c.Scheme())
		if err != nil {
			return err
		}
		gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		resource := gvr.Resource
		if subResource != "" {
			resource += "/" + subResource
		}
		for _, v := range verbs {
			if !slices.Contains(role[gvr.Group+"/"+resource], v) {
				name := ""
				if o, ok := obj.(client.Object); ok {
					name = o.GetName()
				}
				return apierrors.NewForbidden(schema.GroupResource{Group: gvr.Group, Resource: resource}, name,
					fmt.Errorf("manager-role does not grant %s", v))
			}
		}
		return nil
	}
	read := func(obj runtime.Object, verb string) []string {
		switch obj.(type) {
		case *unstructured.Unstructured, *unstructured.UnstructuredList:
			return []string{verb}
		}
		if cached {
			return []string{verb, "list", "watch"}
		}
		return []string{verb}
	}
	return interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := check(c, obj, "", read(obj, "get")...); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := check(c, list, "", read(list, "list")...); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := check(c, obj, "", "create"); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := check(c, obj, "", "update"); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := check(c, obj, "", "patch"); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := check(c, obj, "", "delete"); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			if err := check(c, obj, "", "deletecollection"); err != nil {
				return err
			}
			return c.DeleteAllOf(ctx, obj, opts...)
		},
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errApplyUnchecked
		},
		SubResourceGet: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object, opts ...client.SubResourceGetOption) error {
			if err := check(c, obj, sub, "get"); err != nil {
				return err
			}
			return c.SubResource(sub).Get(ctx, obj, subObj, opts...)
		},
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object,
			opts ...client.SubResourceCreateOption) error {
			if err := check(c, obj, sub, "create"); err != nil {
				return err
			}
			return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := check(c, obj, sub, "update"); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if err := check(c, obj, sub, "patch"); err != nil {
				return err
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		SubResourceApply: func(context.Context, client.Client, string, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
			return errApplyUnchecked
		},
	})
}
