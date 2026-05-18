package condition

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestHTTPConditionDropsCustomHeadersOnCrossOriginRedirect(t *testing.T) {
	var redirectedHeader string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedHeader = r.Header.Get("X-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	cond := NewHTTP(source.URL)
	cond.Headers["X-Token"] = "secret"
	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if redirectedHeader != "" {
		t.Fatalf("redirected header = %q, want stripped", redirectedHeader)
	}
}

func TestHTTPConditionRejectsOversizedBodyForMatchers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("a", int(maxHTTPBodyBytes)+1)))
	}))
	defer server.Close()

	cond := NewHTTP(server.URL)
	cond.BodyContains = "a"
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied || result.Err == nil || !strings.Contains(result.Err.Error(), "too large") {
		t.Fatalf("result = %+v, want oversized body unsatisfied", result)
	}
}

func TestHTTPConditionInvalidDirectConfigFatalBeforeRequest(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*HTTPCondition)
	}{
		{"empty url", func(c *HTTPCondition) { c.URL = "" }},
		{"unsupported scheme", func(c *HTTPCondition) { c.URL = "ftp://example.test/file" }},
		{"missing host", func(c *HTTPCondition) { c.URL = "https:///ready" }},
		{"userinfo", func(c *HTTPCondition) { c.URL = "https://user@example.test/ready" }},
		{"fragment", func(c *HTTPCondition) { c.URL = "https://example.test/ready#frag" }},
		{"bad port", func(c *HTTPCondition) { c.URL = "https://example.test:bad/ready" }},
		{"invalid method", func(c *HTTPCondition) { c.Method = "BAD METHOD" }},
		{"large body", func(c *HTTPCondition) { c.RequestBody = []byte(strings.Repeat("x", maxHTTPRequestBodyBytes+1)) }},
		{"large matcher", func(c *HTTPCondition) { c.BodyContains = strings.Repeat("x", maxHTTPMatcherBytes+1) }},
		{"invalid expected status", func(c *HTTPCondition) { c.ExpectedStatus = 99 }},
		{"invalid matcher raw", func(c *HTTPCondition) { c.StatusMatcher = HTTPStatusMatcher{raw: "9xx"} }},
		{"invalid matcher exact", func(c *HTTPCondition) { c.StatusMatcher = HTTPStatusMatcher{exact: 99} }},
		{"invalid matcher class", func(c *HTTPCondition) { c.StatusMatcher = HTTPStatusMatcher{class: 9} }},
		{"ambiguous status matcher", func(c *HTTPCondition) {
			c.StatusMatcher = HTTPStatusMatcher{exact: http.StatusOK, class: 2}
		}},
		{"expected status with matcher", func(c *HTTPCondition) {
			c.ExpectedStatus = http.StatusOK
			c.StatusMatcher = HTTPStatusMatcher{class: 2}
		}},
		{"invalid header name", func(c *HTTPCondition) { c.Headers["Bad Header"] = "ok" }},
		{"invalid header value", func(c *HTTPCondition) { c.Headers["X-Test"] = "bad\nvalue" }},
		{"large header", func(c *HTTPCondition) { c.Headers["X-Test"] = strings.Repeat("x", maxHTTPHeaderBytes+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewHTTP("https://example.test/ready")
			cond.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("request should not be sent for invalid config")
				return nil, nil
			})}
			tt.setup(cond)

			result := cond.Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
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

func TestHTTPStatusOnlyDrainsLimitedBody(t *testing.T) {
	body := &countingReadCloser{remaining: maxHTTPDrainBytes + 1024}
	cond := NewHTTP("https://example.test/ready")
	cond.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header:     make(http.Header),
		}, nil
	})}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want %s; err = %v", result.Status, CheckSatisfied, result.Err)
	}
	if body.read != maxHTTPDrainBytes {
		t.Fatalf("read = %d, want capped drain %d", body.read, maxHTTPDrainBytes)
	}
	if !body.closed {
		t.Fatal("body was not closed")
	}
}

func TestHTTPStatusMismatchLongBody(t *testing.T) {
	secretBody := "secret-token-" + strings.Repeat("x", 250)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, secretBody, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	result := NewHTTP(server.URL).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("expected unsatisfied")
	}
	if strings.Contains(result.Detail, "secret-token") || strings.Contains(result.Err.Error(), "secret-token") {
		t.Fatalf("status mismatch leaked response body: detail=%q err=%q", result.Detail, result.Err)
	}
}

func TestHTTPMatcherDetailsDoNotExposeSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "secret-token")
	}))
	defer server.Close()

	contains := NewHTTP(server.URL)
	contains.BodyContains = "secret-token"
	result := contains.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("contains status = %s, err = %v", result.Status, result.Err)
	}
	if strings.Contains(result.Detail, "secret-token") {
		t.Fatalf("contains detail leaked secret: %q", result.Detail)
	}

	regex := NewHTTP(server.URL)
	regex.BodyRegex = regexp.MustCompile("secret-token")
	result = regex.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("regex status = %s, err = %v", result.Status, result.Err)
	}
	if strings.Contains(result.Detail, "secret-token") {
		t.Fatalf("regex detail leaked secret: %q", result.Detail)
	}
}

func TestHTTPJSONPathErrorDoesNotExposeExpressionValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"token":"actual"}`)
	}))
	defer server.Close()

	cond := NewHTTP(server.URL)
	cond.BodyJSONExpr = expr.MustCompile(".token == expected-secret")
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("Status = %s, want %s", result.Status, CheckUnsatisfied)
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want jsonpath unsatisfied error")
	}
	if strings.Contains(result.Err.Error(), "expected-secret") || strings.Contains(result.Detail, "expected-secret") {
		t.Fatalf("jsonpath output leaked expected value: detail=%q err=%q", result.Detail, result.Err)
	}
}

func TestHTTPInvalidJSONBodyUnsatisfied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "warming up")
	}))
	defer server.Close()

	cond := NewHTTP(server.URL)
	cond.BodyJSONExpr = expr.MustCompile(".ready == true")
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "parse json") {
		t.Fatalf("err = %v, want parse json error", result.Err)
	}
}

func TestHTTPErrorRedactsSensitiveURLParts(t *testing.T) {
	rawURL := "https://example.com/health?token=secret&ready=true" // #nosec G101 -- synthetic secret used to verify redaction.
	cond := NewHTTP(rawURL)
	cond.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: rawURL, Err: errors.New("dial " + rawURL)}
	})}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("Status = %s, want %s", result.Status, CheckUnsatisfied)
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want redacted URL error")
	}
	got := result.Err.Error()
	for _, leaked := range []string{"secret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("error = %q leaked %q", got, leaked)
		}
	}
	if strings.Contains(got, "ready=true") {
		t.Fatalf("error = %q leaked query value", got)
	}
}

func TestHTTPRedactUserinfoHelper(t *testing.T) {
	user := "user"
	password := "pa" + "ss"
	sensitiveURL := "https://" + user + ":" + password + "@example.com/health"
	err := redactHTTPError(&url.Error{
		Op:  "Get",
		URL: sensitiveURL,
		Err: errors.New("dial " + sensitiveURL + " failed"),
	})
	got := err.Error()
	if strings.Contains(got, user) || strings.Contains(got, password) {
		t.Fatalf("error = %q leaked userinfo", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type countingReadCloser struct {
	remaining int64
	read      int64
	closed    bool
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	for i := range p {
		p[i] = 'x'
	}
	n := len(p)
	r.remaining -= int64(n)
	r.read += int64(n)
	return n, nil
}

func (r *countingReadCloser) Close() error {
	r.closed = true
	return nil
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

func TestHTTPDescriptorRedactsSensitiveURLParts(t *testing.T) {
	cond := NewHTTP("https://user:pass@example.com/health?token=secret&ready=true&api_key=k")
	d := cond.Descriptor()
	if strings.Contains(d.Target, "user") || strings.Contains(d.Target, "pass") ||
		strings.Contains(d.Target, "secret") || strings.Contains(d.Target, "api_key=k") ||
		strings.Contains(d.Target, "ready=true") {
		t.Fatalf("Target = %q, want sensitive values redacted", d.Target)
	}
	if !strings.Contains(d.Target, "ready=REDACTED") {
		t.Fatalf("Target = %q, want query values redacted", d.Target)
	}
}
