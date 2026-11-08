package cli

import (
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseLockfileCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"lockfile", "/tmp/app.lock", "--older-than", "5s"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.LockfileCondition)
	if got.State != condition.LockfilePresent || got.OlderThan != 5*time.Second {
		t.Fatalf("lockfile = %+v", got)
	}
	if _, err := parseCondition([]string{"lockfile", "/tmp/app.lock", "--present", "--absent"}); err == nil {
		t.Fatal("conflicting lockfile states succeeded")
	}
}
