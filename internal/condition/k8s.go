package condition

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pbsladek/wait-for/internal/expr"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type KubernetesGetter interface {
	Get(ctx context.Context, resource string, namespace string) (map[string]any, error)
}

type KubernetesCondition struct {
	Resource   string
	Namespace  string
	Condition  string
	JSONExpr   *expr.Expression // pre-compiled; use JSONExpr.String() for display
	Kubeconfig string
	Getter     KubernetesGetter
	getterOnce sync.Once
	getter     KubernetesGetter
	getterErr  error
}

func NewKubernetes(resource string) *KubernetesCondition {
	return &KubernetesCondition{Resource: resource, Namespace: "default"}
}

func (c *KubernetesCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "k8s", Target: c.Resource}
}

func (c *KubernetesCondition) Check(ctx context.Context) Result {
	if err := validateK8sResource(c.Resource); err != nil {
		return Fatal(err)
	}
	getter, err := c.resolveGetter()
	if err != nil {
		return Fatal(err)
	}
	obj, err := getter.Get(ctx, c.Resource, c.Namespace)
	if err != nil {
		return Unsatisfied("", err)
	}
	if c.JSONExpr != nil {
		return checkK8sJSONExpr(obj, c.JSONExpr)
	}
	conditionName := c.Condition
	if conditionName == "" {
		conditionName = "Ready"
	}
	return checkK8sNamedCondition(obj, conditionName)
}

func validateK8sResource(resource string) error {
	kind, _, err := splitKubernetesResource(resource)
	if err != nil {
		return err
	}
	if _, _, err := gvrForKind(kind); err != nil {
		return err
	}
	return nil
}

func (c *KubernetesCondition) resolveGetter() (KubernetesGetter, error) {
	if c.Getter != nil {
		return c.Getter, nil
	}
	c.getterOnce.Do(func() {
		c.getter, c.getterErr = NewDynamicKubernetesGetter(c.Kubeconfig)
	})
	return c.getter, c.getterErr
}

func checkK8sJSONExpr(obj map[string]any, jsonExpr *expr.Expression) Result {
	ok, detail, err := jsonExpr.Evaluate(obj)
	if err != nil {
		return Fatal(err)
	}
	if !ok {
		return Unsatisfied(detail, fmt.Errorf("jsonpath condition not satisfied: %s", jsonExpr))
	}
	return Satisfied(detail)
}

func checkK8sNamedCondition(obj map[string]any, conditionName string) Result {
	conditions, ok, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil {
		return Fatal(err)
	}
	if !ok {
		return Unsatisfied("status.conditions not found", fmt.Errorf("status.conditions not found"))
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(cond, "type")
		status, _, _ := unstructured.NestedString(cond, "status")
		if strings.EqualFold(t, conditionName) {
			if status == "True" {
				return Satisfied(fmt.Sprintf("condition %s=True", conditionName))
			}
			detail := fmt.Sprintf("condition %s=%s", conditionName, status)
			return Unsatisfied(detail, errors.New(detail))
		}
	}
	detail := fmt.Sprintf("condition %s not found", conditionName)
	return Unsatisfied(detail, errors.New(detail))
}

type DynamicKubernetesGetter struct {
	client dynamic.Interface
}

func NewDynamicKubernetesGetter(kubeconfig string) (*DynamicKubernetesGetter, error) {
	cfg, err := buildKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &DynamicKubernetesGetter{client: client}, nil
}

func NewDynamicKubernetesGetterWithClient(client dynamic.Interface) *DynamicKubernetesGetter {
	return &DynamicKubernetesGetter{client: client}
}

func (g *DynamicKubernetesGetter) Get(ctx context.Context, resource string, namespace string) (map[string]any, error) {
	kind, name, err := splitKubernetesResource(resource)
	if err != nil {
		return nil, err
	}
	gvr, namespaced, err := gvrForKind(kind)
	if err != nil {
		return nil, err
	}
	var obj *unstructured.Unstructured
	if namespaced {
		obj, err = g.client.Resource(gvr).Namespace(namespace).Get(ctx, name, v1.GetOptions{})
	} else {
		obj, err = g.client.Resource(gvr).Get(ctx, name, v1.GetOptions{})
	}
	if err != nil {
		return nil, err
	}
	return obj.Object, nil
}

func buildKubeConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	if home := homedir.HomeDir(); home != "" {
		path := filepath.Join(home, ".kube", "config")
		if cfg, err := clientcmd.BuildConfigFromFlags("", path); err == nil {
			return cfg, nil
		}
	}
	return nil, fmt.Errorf("kubernetes config not found; set --kubeconfig or run in cluster")
}

func splitKubernetesResource(resource string) (string, string, error) {
	kind, name, ok := strings.Cut(resource, "/")
	if !ok || kind == "" || name == "" {
		return "", "", fmt.Errorf("resource must use kind/name syntax")
	}
	return strings.ToLower(kind), name, nil
}

func gvrForKind(kind string) (schema.GroupVersionResource, bool, error) {
	switch kind {
	case "pod", "pods", "po":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}, true, nil
	case "service", "services", "svc":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}, true, nil
	case "deployment", "deployments", "deploy":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true, nil
	case "replicaset", "replicasets", "rs":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, true, nil
	case "statefulset", "statefulsets", "sts":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, true, nil
	case "daemonset", "daemonsets", "ds":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, true, nil
	case "job", "jobs":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, true, nil
	case "namespace", "namespaces", "ns":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}, false, nil
	default:
		return schema.GroupVersionResource{}, false, fmt.Errorf("unsupported kubernetes resource kind %q", kind)
	}
}
