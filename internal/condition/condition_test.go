package condition

import (
	"context"
	"errors"
	"testing"
)

func TestDescriptorDisplayName(t *testing.T) {
	tests := []struct {
		d    Descriptor
		want string
	}{
		{Descriptor{Name: "custom"}, "custom"},
		{Descriptor{Backend: "tcp", Target: "localhost:5432"}, "tcp localhost:5432"},
		{Descriptor{Backend: "http"}, "http"},
		{Descriptor{Target: "localhost:5432"}, "localhost:5432"},
		{Descriptor{}, ""},
	}
	for _, tt := range tests {
		if got := tt.d.DisplayName(); got != tt.want {
			t.Fatalf("DisplayName() = %q, want %q (input %+v)", got, tt.want, tt.d)
		}
	}
}

func TestResultHelpers(t *testing.T) {
	s := Satisfied("ok")
	if s.Status != CheckSatisfied || s.Detail != "ok" {
		t.Fatalf("Satisfied() = %+v", s)
	}

	u := Unsatisfied("not yet", nil)
	if u.Status != CheckUnsatisfied || u.Detail != "not yet" {
		t.Fatalf("Unsatisfied() = %+v", u)
	}

	f := Fatal(nil)
	if f.Status != CheckFatal {
		t.Fatalf("Fatal() = %+v", f)
	}

	fd := FatalDetail("detail msg", nil)
	if fd.Status != CheckFatal || fd.Detail != "detail msg" {
		t.Fatalf("FatalDetail() = %+v", fd)
	}
}

func TestGuardCondition(t *testing.T) {
	guard := NewGuard(staticCondition{
		desc:   Descriptor{Backend: "log", Target: "app.log"},
		result: Satisfied("matched: panic"),
	})
	if guard.ConditionRole() != RoleGuard {
		t.Fatalf("role = %s, want guard", guard.ConditionRole())
	}
	if got := guard.Descriptor().DisplayName(); got != "guard log app.log" {
		t.Fatalf("display = %q, want guard log app.log", got)
	}
	result := guard.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

func TestGuardConditionPassesThroughUnsatisfied(t *testing.T) {
	guard := NewGuard(staticCondition{
		result: Unsatisfied("clear", errors.New("clear")),
	})
	result := guard.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestNilGuardCondition(t *testing.T) {
	guard := NewGuard(nil)
	if got := guard.Descriptor().DisplayName(); got != "guard <nil>" {
		t.Fatalf("display = %q, want guard <nil>", got)
	}
	result := guard.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

type staticCondition struct {
	desc   Descriptor
	result Result
}

func (c staticCondition) Descriptor() Descriptor {
	return c.desc
}

func (c staticCondition) Check(context.Context) Result {
	return c.result
}
