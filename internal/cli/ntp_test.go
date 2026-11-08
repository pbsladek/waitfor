package cli

import (
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseNTPCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"ntp", "127.0.0.1:123", "--max-offset", "1s", "--timeout", "500ms"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.NTPCondition)
	if got.AttemptTimeout != 500*time.Millisecond {
		t.Fatalf("ntp = %+v", got)
	}
	if _, err := parseCondition([]string{"ntp", "time.example", "--max-offset", "not-duration"}); err == nil {
		t.Fatal("invalid NTP duration succeeded")
	}
}
