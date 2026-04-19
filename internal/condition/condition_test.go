package condition

import "testing"

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
