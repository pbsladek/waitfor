package cli

import (
	"testing"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParsePermissionCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"permission", "/tmp/app", "--mode", "0640", "--type", "file"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.PermissionCondition)
	if got.Mode != 0o640 || got.Type != condition.PermissionFile {
		t.Fatalf("permission = %+v", got)
	}
	if _, err := parseCondition([]string{"permission", "/tmp/app", "--uid", "1", "--user", "root"}); err == nil {
		t.Fatal("conflicting permission owner options succeeded")
	}
}

func TestPermissionNumericUserAndGroupOptions(t *testing.T) {
	cond := condition.NewPermission("/tmp/app")
	if err := applyOwnerOption(cond, -1, "1001"); err != nil {
		t.Fatal(err)
	}
	if cond.UID == nil || *cond.UID != 1001 {
		t.Fatalf("uid = %v, want 1001", cond.UID)
	}
	if err := applyGroupOption(cond, -1, "1002"); err != nil {
		t.Fatal(err)
	}
	if cond.GID == nil || *cond.GID != 1002 {
		t.Fatalf("gid = %v, want 1002", cond.GID)
	}
	if err := applyOwnerOption(condition.NewPermission("/tmp/app"), -1, "root"); err == nil {
		t.Fatal("non-numeric --user succeeded")
	}
	if err := applyGroupOption(condition.NewPermission("/tmp/app"), -1, "staff"); err == nil {
		t.Fatal("non-numeric --group succeeded")
	}
}
