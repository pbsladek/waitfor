package cli

import (
	"testing"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseHTTPAuthHelpers(t *testing.T) {
	parsed, err := parseHTTPCondition([]string{"http", "https://example.test/ready", "--bearer-token", "token"})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.(*condition.HTTPCondition).Headers["Authorization"]; got != "Bearer token" {
		t.Fatalf("Authorization = %q", got)
	}
	parsed, err = parseHTTPCondition([]string{"http", "https://example.test/ready", "--basic-user", "u", "--basic-password", "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.(*condition.HTTPCondition).Headers["Authorization"]; got != "Basic dTpw" {
		t.Fatalf("Authorization = %q", got)
	}
	if _, err := parseHTTPCondition([]string{"http", "https://example.test/ready", "--header", "Authorization=raw", "--bearer-token", "token"}); err == nil {
		t.Fatal("authorization helper with explicit header succeeded")
	}
	if _, err := parseHTTPCondition([]string{"http", "https://example.test/ready", "--client-cert", "cert.pem"}); err == nil {
		t.Fatal("client certificate without key succeeded")
	}
}
