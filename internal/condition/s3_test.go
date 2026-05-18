package condition

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestS3BucketExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/ready-bucket" {
			t.Fatalf("request = %s %s, want HEAD /ready-bucket", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket")
	cond.EndpointURL = server.URL

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if result.Detail != "bucket exists" {
		t.Fatalf("detail = %q, want bucket exists", result.Detail)
	}
}

func TestS3DescriptorAndAWSURL(t *testing.T) {
	cond := NewS3("s3://ready-bucket/path/ready.json")
	desc := cond.Descriptor()
	if desc.Backend != "s3" || desc.Target != "s3://ready-bucket/path/ready.json" {
		t.Fatalf("Descriptor() = %+v", desc)
	}
	got := cond.awsS3URL(S3Target{Bucket: "ready-bucket", Key: "path/ready.json"})
	want := "https://ready-bucket.s3.us-east-1.amazonaws.com/path/ready.json"
	if got != want {
		t.Fatalf("awsS3URL() = %q, want %q", got, want)
	}
}

func TestS3ObjectExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/ready-bucket/path/ready.json" {
			t.Fatalf("request = %s %s, want HEAD object", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = server.URL

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if result.Detail != "object exists" {
		t.Fatalf("detail = %q, want object exists", result.Detail)
	}
}

func TestS3MetadataAndContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		w.Header().Set("x-amz-meta-version", "42")
		_, _ = fmt.Fprint(w, `{"ready":true}`)
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = server.URL
	cond.Metadata = map[string]string{"version": "42"}
	cond.Contains = `"ready":true`

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v, detail = %q", result.Status, result.Err, result.Detail)
	}
	if result.Detail != "object contains required marker" {
		t.Fatalf("detail = %q, want content detail", result.Detail)
	}
}

func TestS3VirtualHostedEndpoint(t *testing.T) {
	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = "https://objects.example.test/base"
	cond.VirtualHostedStyle = true

	got, err := cond.s3RequestURL(S3Target{Bucket: "ready-bucket", Key: "path/ready.json"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://ready-bucket.objects.example.test/base/path/ready.json"
	if got != want {
		t.Fatalf("s3RequestURL() = %q, want %q", got, want)
	}
}

func TestS3RequestURLEscapesObjectKey(t *testing.T) {
	cond := NewS3("s3://ready-bucket/path/ready file.json")
	cond.EndpointURL = "http://127.0.0.1:9000/root"

	got, err := cond.s3RequestURL(S3Target{Bucket: "ready-bucket", Key: "path/ready file.json"})
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:9000/root/ready-bucket/path/ready%20file.json"
	if got != want {
		t.Fatalf("s3RequestURL() = %q, want %q", got, want)
	}
}

func TestS3RequestURLPreservesOpaqueObjectKey(t *testing.T) {
	cond := NewS3("s3://ready-bucket")
	cond.EndpointURL = "https://objects.example.test/root"

	got, err := cond.s3RequestURL(S3Target{Bucket: "ready-bucket", Key: "a//b/"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://objects.example.test/root/ready-bucket/a//b/"
	if got != want {
		t.Fatalf("s3RequestURL() = %q, want %q", got, want)
	}

	got, err = cond.s3RequestURL(S3Target{Bucket: "ready-bucket", Key: "/ready"})
	if err != nil {
		t.Fatal(err)
	}
	want = "https://objects.example.test/root/ready-bucket//ready"
	if got != want {
		t.Fatalf("s3RequestURL() = %q, want %q", got, want)
	}
}

func TestS3CephRGWEndpointWithBasePath(t *testing.T) {
	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = "https://ceph-rgw.example.test/s3"
	cond.Region = "default"

	got, err := cond.s3RequestURL(S3Target{Bucket: "ready-bucket", Key: "path/ready.json"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://ceph-rgw.example.test/s3/ready-bucket/path/ready.json"
	if got != want {
		t.Fatalf("s3RequestURL() = %q, want %q", got, want)
	}
}

func TestS3UnsignedStatusMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = server.URL

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "object does not exist" {
		t.Fatalf("detail = %q, want missing object", result.Detail)
	}
}

func TestS3BucketMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket")
	cond.EndpointURL = server.URL

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "bucket does not exist" {
		t.Fatalf("detail = %q, want missing bucket", result.Detail)
	}
}

func TestS3MetadataMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-meta-version", "41")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = server.URL
	cond.Metadata = map[string]string{"version": "42"}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err == nil || strings.Contains(result.Err.Error(), "41") || strings.Contains(result.Err.Error(), "42") {
		t.Fatalf("err = %v, want redacted metadata mismatch", result.Err)
	}
}

func TestS3ContentMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "warming")
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = server.URL
	cond.Contains = "ready"

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "object content marker not found" {
		t.Fatalf("detail = %q, want marker detail", result.Detail)
	}
}

func TestS3SignsRequestsWhenCredentialsProvided(t *testing.T) {
	var authorization string
	var token string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		token = r.Header.Get("X-Amz-Security-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cond := NewS3("s3://ready-bucket/path/ready.json")
	cond.EndpointURL = server.URL
	cond.Client = server.Client()
	cond.Region = "auto"
	cond.Now = func() time.Time { return time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC) }
	cond.Credentials = S3Credentials{
		AccessKeyID:     "AKIATest",
		SecretAccessKey: "test-secret",
		SessionToken:    "session-token",
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=AKIATest/20260509/auto/s3/aws4_request") {
		t.Fatalf("Authorization = %q, want SigV4 credential scope", authorization)
	}
	if token != "session-token" {
		t.Fatalf("token = %q, want session-token", token)
	}
}

func TestS3InvalidDirectConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*S3Condition)
	}{
		{"bad url", func(c *S3Condition) { c.URL = "https://example.test/object" }},
		{"blank region", func(c *S3Condition) { c.Region = " " }},
		{"bad endpoint", func(c *S3Condition) { c.EndpointURL = "ftp://example.test" }},
		{"endpoint userinfo", func(c *S3Condition) { c.EndpointURL = "https://user@example.test" }},
		{"contains without key", func(c *S3Condition) { c.URL = "s3://bucket"; c.Contains = "ready" }},
		{"metadata without key", func(c *S3Condition) { c.URL = "s3://bucket"; c.Metadata = map[string]string{"version": "1"} }},
		{"partial credentials", func(c *S3Condition) { c.Credentials.AccessKeyID = "AKIATest" }},
		{"plaintext credentials", func(c *S3Condition) {
			c.EndpointURL = "http://127.0.0.1:9000"
			c.Credentials = S3Credentials{AccessKeyID: "AKIATest", SecretAccessKey: "secret"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewS3("s3://bucket/key")
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

func TestParseS3URL(t *testing.T) {
	target, err := ParseS3URL("s3://bucket/path/ready%20file.json")
	if err != nil {
		t.Fatal(err)
	}
	if target.Bucket != "bucket" || target.Key != "path/ready file.json" {
		t.Fatalf("target = %+v", target)
	}
	target, err = ParseS3URL("s3://bucket/%2Fready")
	if err != nil {
		t.Fatal(err)
	}
	if target.Key != "/ready" {
		t.Fatalf("key = %q, want leading slash preserved", target.Key)
	}
}

func TestCanonicalS3Query(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://bucket.s3.us-east-1.amazonaws.com/key?z=last&a=first", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalS3Query(req.URL); got != "a=first&z=last" {
		t.Fatalf("canonicalS3Query() = %q", got)
	}
}
