package condition

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pbsladek/wait-for/internal/expr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	List(ctx context.Context, resource string, namespace string, selector string) ([]map[string]any, error)
}

type KubernetesCondition struct {
	Resource   string
	Namespace  string
	Condition  string
	WaitFor    string
	Selector   string
	All        bool
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
	if err := validateK8sResource(c.Resource, c.Selector); err != nil {
		return Fatal(err)
	}
	getter, err := c.resolveGetter()
	if err != nil {
		return Fatal(err)
	}
	if c.Selector != "" {
		return c.checkSelected(ctx, getter)
	}
	obj, err := getter.Get(ctx, c.Resource, c.Namespace)
	if err != nil {
		return classifyK8sGetterError(err)
	}
	if c.WaitFor != "" {
		return checkK8sWaitFor(obj, c.WaitFor)
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

func validateK8sResource(resource string, selector string) error {
	if selector != "" {
		kind, err := splitKubernetesKind(resource)
		if err != nil {
			return err
		}
		_, _, err = gvrForKind(kind)
		return err
	}
	kind, _, err := splitKubernetesResource(resource)
	if err != nil {
		return err
	}
	if _, _, err := gvrForKind(kind); err != nil {
		return err
	}
	return nil
}

func (c *KubernetesCondition) checkSelected(ctx context.Context, getter KubernetesGetter) Result {
	items, err := getter.List(ctx, c.Resource, c.Namespace, c.Selector)
	if err != nil {
		return classifyK8sGetterError(err)
	}
	if len(items) == 0 {
		return Unsatisfied("no resources matched selector", errors.New("no resources matched selector"))
	}
	return checkK8sSelected(items, c.WaitFor, c.All)
}

func classifyK8sGetterError(err error) Result {
	if permanentK8sError(err) {
		return Fatal(err)
	}
	return Unsatisfied("", err)
}

func permanentK8sError(err error) bool {
	return apierrors.IsUnauthorized(err) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsBadRequest(err) ||
		apierrors.IsInvalid(err)
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
		return Unsatisfied(detail, fmt.Errorf("jsonpath condition not satisfied"))
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

func checkK8sWaitFor(obj map[string]any, waitFor string) Result {
	switch waitFor {
	case "ready":
		return checkK8sReady(obj)
	case "rollout":
		return checkK8sRollout(obj)
	case "complete":
		return checkK8sJobComplete(obj)
	default:
		return Fatal(fmt.Errorf("unsupported kubernetes wait type %q", waitFor))
	}
}

func checkK8sSelected(items []map[string]any, waitFor string, all bool) Result {
	firstSatisfied, lastUnsatisfied, fatal := scanK8sSelected(items, waitFor)
	if fatal != nil {
		return *fatal
	}
	if !all && firstSatisfied.Status == CheckSatisfied {
		return Satisfied(fmt.Sprintf("1 of %d resources satisfied: %s", len(items), firstSatisfied.Detail))
	}
	if all && lastUnsatisfied.Status == "" {
		return Satisfied(fmt.Sprintf("%d resources satisfied", len(items)))
	}
	if lastUnsatisfied.Detail == "" {
		lastUnsatisfied.Detail = "no selected resources satisfied"
	}
	return Unsatisfied(lastUnsatisfied.Detail, lastUnsatisfied.Err)
}

func scanK8sSelected(items []map[string]any, waitFor string) (Result, Result, *Result) {
	var firstSatisfied Result
	var lastUnsatisfied Result
	for _, item := range items {
		result := checkK8sWaitFor(item, waitFor)
		if result.Status == CheckFatal {
			return Result{}, Result{}, &result
		}
		if result.Status == CheckSatisfied {
			if firstSatisfied.Status == "" {
				firstSatisfied = result
			}
			continue
		}
		lastUnsatisfied = result
	}
	return firstSatisfied, lastUnsatisfied, nil
}

func checkK8sReady(obj map[string]any) Result {
	if !k8sKindIs(obj, "pod") {
		return Fatal(fmt.Errorf("--for ready requires pod"))
	}
	phase, _, _ := unstructured.NestedString(obj, "status", "phase")
	if phase == "Failed" {
		return Fatal(fmt.Errorf("pod failed"))
	}
	return checkK8sNamedCondition(obj, "Ready")
}

func checkK8sJobComplete(obj map[string]any) Result {
	if !k8sKindIs(obj, "job") {
		return Fatal(fmt.Errorf("--for complete requires job"))
	}
	if hasK8sConditionStatus(obj, "Failed", "True") {
		return Fatal(fmt.Errorf("job failed"))
	}
	if hasK8sConditionStatus(obj, "Complete", "True") {
		return Satisfied("condition Complete=True")
	}
	return Unsatisfied("condition Complete not true", errors.New("condition Complete not true"))
}

func hasK8sConditionStatus(obj map[string]any, conditionName, want string) bool {
	conditions, ok, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !ok {
		return false
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(cond, "type")
		status, _, _ := unstructured.NestedString(cond, "status")
		if strings.EqualFold(t, conditionName) && status == want {
			return true
		}
	}
	return false
}

func k8sKindIs(obj map[string]any, want string) bool {
	kind, _, _ := unstructured.NestedString(obj, "kind")
	return strings.EqualFold(kind, want)
}

func checkK8sRollout(obj map[string]any) Result {
	kind, _, _ := unstructured.NestedString(obj, "kind")
	switch strings.ToLower(kind) {
	case "deployment":
		return checkDeploymentRollout(obj)
	case "statefulset":
		return checkStatefulSetRollout(obj)
	case "daemonset":
		return checkDaemonSetRollout(obj)
	default:
		return Fatal(fmt.Errorf("--for rollout requires deployment, statefulset, or daemonset"))
	}
}

func checkDeploymentRollout(obj map[string]any) Result {
	desired := k8sInt64Default(obj, 1, "spec", "replicas")
	if result := checkObservedGeneration(obj); result != nil {
		return *result
	}
	updated := k8sInt64(obj, "status", "updatedReplicas")
	available := k8sInt64(obj, "status", "availableReplicas")
	total := k8sInt64(obj, "status", "replicas")
	if updated < desired {
		return k8sRolloutUnsatisfied("updated replicas", updated, desired)
	}
	if total > updated {
		return k8sRolloutUnsatisfied("total replicas after old replicas terminate", total, updated)
	}
	if available < desired {
		return k8sRolloutUnsatisfied("available replicas", available, desired)
	}
	return Satisfied(fmt.Sprintf("rollout complete: %d replicas available", available))
}

func checkStatefulSetRollout(obj map[string]any) Result {
	desired := k8sInt64Default(obj, 1, "spec", "replicas")
	if result := checkObservedGeneration(obj); result != nil {
		return *result
	}
	ready := k8sInt64(obj, "status", "readyReplicas")
	updated := k8sInt64(obj, "status", "updatedReplicas")
	if updated < desired {
		return k8sRolloutUnsatisfied("updated replicas", updated, desired)
	}
	if ready < desired {
		return k8sRolloutUnsatisfied("ready replicas", ready, desired)
	}
	return Satisfied(fmt.Sprintf("rollout complete: %d replicas ready", ready))
}

func checkDaemonSetRollout(obj map[string]any) Result {
	if result := checkObservedGeneration(obj); result != nil {
		return *result
	}
	desired := k8sInt64(obj, "status", "desiredNumberScheduled")
	updated := k8sInt64(obj, "status", "updatedNumberScheduled")
	ready := k8sInt64(obj, "status", "numberReady")
	unavailable := k8sInt64(obj, "status", "numberUnavailable")
	if updated < desired {
		return k8sRolloutUnsatisfied("updated pods", updated, desired)
	}
	if ready < desired {
		return k8sRolloutUnsatisfied("ready pods", ready, desired)
	}
	if unavailable > 0 {
		return Unsatisfied("pods unavailable", errors.New("pods unavailable"))
	}
	return Satisfied(fmt.Sprintf("rollout complete: %d pods ready", ready))
}

func checkObservedGeneration(obj map[string]any) *Result {
	generation := k8sInt64(obj, "metadata", "generation")
	if generation == 0 {
		return nil
	}
	observed := k8sInt64(obj, "status", "observedGeneration")
	if observed >= generation {
		return nil
	}
	detail := fmt.Sprintf("observed generation %d, expected %d", observed, generation)
	result := Unsatisfied(detail, errors.New(detail))
	return &result
}

func k8sRolloutUnsatisfied(label string, got, want int64) Result {
	detail := fmt.Sprintf("%s %d, expected %d", label, got, want)
	return Unsatisfied(detail, errors.New(detail))
}

func k8sInt64(obj map[string]any, fields ...string) int64 {
	value, ok, _ := unstructured.NestedInt64(obj, fields...)
	if ok {
		return value
	}
	return 0
}

func k8sInt64Default(obj map[string]any, fallback int64, fields ...string) int64 {
	value, ok, _ := unstructured.NestedInt64(obj, fields...)
	if ok {
		return value
	}
	return fallback
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

func (g *DynamicKubernetesGetter) List(ctx context.Context, resource string, namespace string, selector string) ([]map[string]any, error) {
	kind, err := splitKubernetesKind(resource)
	if err != nil {
		return nil, err
	}
	gvr, namespaced, err := gvrForKind(kind)
	if err != nil {
		return nil, err
	}
	opts := v1.ListOptions{LabelSelector: selector}
	var list *unstructured.UnstructuredList
	if namespaced {
		list, err = g.client.Resource(gvr).Namespace(namespace).List(ctx, opts)
	} else {
		list, err = g.client.Resource(gvr).List(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, item.Object)
	}
	return items, nil
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

func splitKubernetesKind(resource string) (string, error) {
	if resource == "" || strings.Contains(resource, "/") {
		return "", fmt.Errorf("selector waits require a resource kind without /name syntax")
	}
	return strings.ToLower(resource), nil
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
