package condition

import (
	"context"
	"fmt"
	"testing"

	"github.com/pbsladek/wait-for/internal/expr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestKubernetesConditionNamedCondition(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObject("True"))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.Condition = "Ready"

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v, detail = %q", result.Err, result.Detail)
	}
}

func TestKubernetesConditionJSONPath(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObject("False"))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.JSONExpr = expr.MustCompile("{.status.phase}=Running")

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v, detail = %q", result.Err, result.Detail)
	}
}

func TestKubernetesConditionJSONPathNotSatisfied(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObject("False"))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.JSONExpr = expr.MustCompile("{.status.phase}=Pending")

	result := cond.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("expected not satisfied when phase != Pending")
	}
}

func TestKubernetesConditionNotReady(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObject("False"))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.Condition = "Ready"

	result := cond.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("Satisfied = true, want false")
	}
	if result.Detail != "condition Ready=False" {
		t.Fatalf("detail = %q, want condition Ready=False", result.Detail)
	}
}

func TestKubernetesConditionUnknownUnsatisfied(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObject("Unknown"))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.Condition = "Ready"

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "condition Ready=Unknown" {
		t.Fatalf("detail = %q, want condition Ready=Unknown", result.Detail)
	}
}

func TestKubernetesConditionNestedJSONPathListIndex(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObjectWithContainerReady(true))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.JSONExpr = expr.MustCompile(".status.containerStatuses[0].ready == true")

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v, detail = %q", result.Status, result.Err, result.Detail)
	}
}

func TestKubernetesConditionDefaultConditionReady(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObject("True"))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	// No Condition set — defaults to "Ready"
	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("default Ready condition: Satisfied = false, err = %v", result.Err)
	}
}

func TestKubernetesConditionNotFound(t *testing.T) {
	// condition type exists but name doesn't match
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), podObject("True"))
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.Condition = "PodScheduled"

	result := cond.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("expected unsatisfied when condition type not found")
	}
}

func TestKubernetesConditionNoStatusConditions(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "myapp", "namespace": "default"},
		"status":     map[string]any{},
	}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"})

	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	cond := NewKubernetes("pod/myapp")
	cond.Getter = NewDynamicKubernetesGetterWithClient(client)
	cond.Condition = "Ready"

	result := cond.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("expected unsatisfied when status.conditions missing")
	}
	if result.Detail != "status.conditions not found" {
		t.Fatalf("detail = %q, want status.conditions not found", result.Detail)
	}
}

func TestKubernetesConditionUnsupportedKindFatal(t *testing.T) {
	cond := NewKubernetes("widget/myapp")
	cond.Getter = &stubGetter{}

	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("expected Fatal for unsupported kind, got %s", result.Status)
	}
}

func TestKubernetesConditionInvalidResourceFormat(t *testing.T) {
	cond := NewKubernetes("noSlashHere")
	cond.Getter = &stubGetter{}

	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("expected Fatal for invalid resource format, got %s", result.Status)
	}
}

func TestKubernetesConditionGetterError(t *testing.T) {
	cond := NewKubernetes("pod/missing")
	cond.Getter = &errorGetter{fmt.Errorf("not found")}

	result := cond.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("expected unsatisfied when getter returns error")
	}
}

func TestKubernetesDescriptor(t *testing.T) {
	cond := NewKubernetes("pod/myapp")
	d := cond.Descriptor()
	if d.Backend != "k8s" {
		t.Fatalf("Backend = %q, want k8s", d.Backend)
	}
	if d.Target != "pod/myapp" {
		t.Fatalf("Target = %q, want pod/myapp", d.Target)
	}
}

func TestGVRForKindAllResources(t *testing.T) {
	tests := []struct {
		kind string
	}{
		{"pod"}, {"pods"}, {"po"},
		{"service"}, {"services"}, {"svc"},
		{"deployment"}, {"deployments"}, {"deploy"},
		{"replicaset"}, {"replicasets"}, {"rs"},
		{"statefulset"}, {"statefulsets"}, {"sts"},
		{"daemonset"}, {"daemonsets"}, {"ds"},
		{"job"}, {"jobs"},
		{"namespace"}, {"namespaces"}, {"ns"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			gvr, _, err := gvrForKind(tt.kind)
			if err != nil {
				t.Fatalf("gvrForKind(%q) error = %v", tt.kind, err)
			}
			if gvr.Resource == "" {
				t.Fatalf("gvrForKind(%q) returned empty resource", tt.kind)
			}
		})
	}
}

func TestGVRForKindUnsupported(t *testing.T) {
	_, _, err := gvrForKind("widget")
	if err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}

func TestSplitKubernetesResource(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"pod/myapp", false},
		{"noSlash", true},
		{"/noKind", true},
		{"pod/", true},
	}
	for _, tt := range tests {
		_, _, err := splitKubernetesResource(tt.input)
		if (err != nil) != tt.wantErr {
			t.Fatalf("splitKubernetesResource(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
		}
	}
}

func TestDynamicGetterNamespaced(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), deployObject())
	getter := NewDynamicKubernetesGetterWithClient(client)
	obj, err := getter.Get(t.Context(), "deployment/myapp", "default")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if obj == nil {
		t.Fatal("Get() returned nil")
	}
}

func TestDynamicGetterClusterScoped(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), nsObject())
	getter := NewDynamicKubernetesGetterWithClient(client)
	obj, err := getter.Get(t.Context(), "namespace/mynamespace", "")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if obj == nil {
		t.Fatal("Get() returned nil")
	}
}

func TestBuildKubeConfigBadPath(t *testing.T) {
	_, err := buildKubeConfig("/nonexistent/kubeconfig.yaml")
	if err == nil {
		t.Fatal("buildKubeConfig('/nonexistent/kubeconfig.yaml') expected error, got nil")
	}
}

func TestNewDynamicKubernetesGetterBadPath(t *testing.T) {
	_, err := NewDynamicKubernetesGetter("/nonexistent/kubeconfig.yaml")
	if err == nil {
		t.Fatal("NewDynamicKubernetesGetter with bad path expected error, got nil")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

type stubGetter struct{}

func (g *stubGetter) Get(_ context.Context, _ string, _ string) (map[string]any, error) {
	return map[string]any{}, nil
}

type errorGetter struct{ err error }

func (g *errorGetter) Get(_ context.Context, _ string, _ string) (map[string]any, error) {
	return nil, g.err
}

func podObject(ready string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "myapp",
			"namespace": "default",
		},
		"status": map[string]any{
			"phase": "Running",
			"conditions": []any{
				map[string]any{"type": "Ready", "status": ready},
			},
		},
	}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"})
	return obj
}

func podObjectWithContainerReady(ready bool) *unstructured.Unstructured {
	obj := podObject("True")
	status := obj.Object["status"].(map[string]any)
	status["containerStatuses"] = []any{
		map[string]any{"name": "app", "ready": ready},
	}
	return obj
}

func deployObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      "myapp",
			"namespace": "default",
		},
	}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	return obj
}

func nsObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": "mynamespace",
		},
	}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"})
	return obj
}
