package cli

import (
	"testing"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseLaunchdCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"launchd", "system/com.example.agent", "--loaded"})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.(*condition.LaunchdCondition); got.State != condition.LaunchdLoaded {
		t.Fatalf("launchd = %+v", got)
	}
	if _, err := parseCondition([]string{"launchd", "svc", "--loaded", "--running"}); err == nil {
		t.Fatal("conflicting launchd states succeeded")
	}
}
