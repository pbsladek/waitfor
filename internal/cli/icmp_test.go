package cli

import (
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseICMPCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"icmp", "127.0.0.1", "--count", "3", "--timeout", "2s"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.ICMPCondition)
	if got.Count != 3 || got.AttemptTimeout != 2*time.Second {
		t.Fatalf("icmp = %+v", got)
	}
	if _, err := parseCondition([]string{"icmp", "127.0.0.1", "--timeout", "nope"}); err == nil {
		t.Fatal("invalid ICMP duration succeeded")
	}
}
