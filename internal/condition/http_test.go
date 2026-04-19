package condition

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/pbsladek/wait-for/internal/expr"
)

func TestHTTPConditionSatisfied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Test"); got != "yes" {
			t.Fatalf("header = %q, want yes", got)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"ready":true,"message":"ok"}`)
	}))
	defer server.Close()

	cond := NewHTTP(server.URL)
	cond.ExpectedStatus = http.StatusAccepted
	cond.BodyContains = "ok"
	cond.BodyJSONExpr = expr.MustCompile(".ready == true")
	cond.Headers["X-Test"] = "yes"

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v, detail = %q", result.Err, result.Detail)
	}
}

func TestHTTPConditionStatusRangeRequestBodyAndRegex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "ping" {
			t.Fatalf("body = %q, want ping", string(body))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, "service ready")
	}))
	defer server.Close()

	status, err := ParseHTTPStatusMatcher("2xx")
	if err != nil {
		t.Fatal(err)
	}
	cond := NewHTTP(server.URL)
	cond.Method = http.MethodPost
	cond.StatusMatcher = status
	cond.RequestBody = []byte("ping")
	cond.BodyRegex = regexp.MustCompile(`ready$`)

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v, detail = %q", result.Err, result.Detail)
	}
}

func TestHTTPConditionNoRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ready", http.StatusFound)
	}))
	defer server.Close()

	status, err := ParseHTTPStatusMatcher("3xx")
	if err != nil {
		t.Fatal(err)
	}
	cond := NewHTTP(server.URL)
	cond.StatusMatcher = status
	cond.NoRedirects = true

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v, detail = %q", result.Err, result.Detail)
	}
}

func TestParseHTTPStatusMatcher(t *testing.T) {
	tests := []struct {
		raw  string
		code int
		want bool
	}{
		{raw: "200", code: 200, want: true},
		{raw: "2xx", code: 201, want: true},
		{raw: "2xx", code: 404, want: false},
	}

	for _, tt := range tests {
		matcher, err := ParseHTTPStatusMatcher(tt.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := matcher.Match(tt.code); got != tt.want {
			t.Fatalf("%s.Match(%d) = %v, want %v", tt.raw, tt.code, got, tt.want)
		}
	}
}

func TestHTTPConditionStatusMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	result := NewHTTP(server.URL).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("Satisfied = true, want false")
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want status error")
	}
}

func TestHTTPStatusMismatchLongBody(t *testing.T) {
	long := strings.Repeat("x", 250)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, long, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	result := NewHTTP(server.URL).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("expected unsatisfied")
	}
	// firstLine truncates to 200 chars
	if len(result.Detail) > 300 {
		t.Fatalf("detail too long (%d chars), expected truncation", len(result.Detail))
	}
}

func TestParseHTTPStatusMatcherInvalid(t *testing.T) {
	tests := []string{"999", "abc", "0", "6xx", ""}
	for _, raw := range tests {
		m, err := ParseHTTPStatusMatcher(raw)
		switch raw {
		case "": // empty becomes "200" — valid
			if err != nil {
				t.Fatalf("ParseHTTPStatusMatcher(%q) err = %v, want nil for empty (defaults to 200)", raw, err)
			}
		default:
			if err == nil {
				t.Fatalf("ParseHTTPStatusMatcher(%q) expected error, got matcher %+v", raw, m)
			}
		}
	}
}

func TestHTTPStatusMatcherStringBranches(t *testing.T) {
	// Branch 1: raw is set (normal case)
	m1, _ := ParseHTTPStatusMatcher("2xx")
	if m1.String() != "2xx" {
		t.Fatalf("String() = %q, want 2xx", m1.String())
	}

	// Branch 2: zero value → default "200"
	var zero HTTPStatusMatcher
	if zero.String() != "200" {
		t.Fatalf("zero.String() = %q, want 200", zero.String())
	}

	// Branch 3: exact set but raw empty — construct directly
	withExact := HTTPStatusMatcher{exact: 404}
	if withExact.String() != "404" {
		t.Fatalf("withExact.String() = %q, want 404", withExact.String())
	}
}

func TestHTTPConditionFatalBadURL(t *testing.T) {
	cond := NewHTTP("://bad-url")
	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("expected Fatal for bad URL, got %s", result.Status)
	}
}

func TestHTTPDescriptor(t *testing.T) {
	cond := NewHTTP("http://example.com")
	d := cond.Descriptor()
	if d.Backend != "http" {
		t.Fatalf("Backend = %q, want http", d.Backend)
	}
	if d.Target != "http://example.com" {
		t.Fatalf("Target = %q, want http://example.com", d.Target)
	}
}
