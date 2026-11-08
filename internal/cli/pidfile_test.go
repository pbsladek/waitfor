package cli

import (
	"testing"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParsePIDFileCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"pidfile", "/tmp/app.pid", "--stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.(*condition.PIDFileCondition); got.State != condition.ProcessStopped {
		t.Fatalf("pidfile = %+v", got)
	}
	if _, err := parseCondition([]string{"pidfile", "/tmp/app.pid", "--running", "--stopped"}); err == nil {
		t.Fatal("conflicting pidfile states succeeded")
	}
}
