package cli

import (
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseExtraBackends(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want any
	}{
		{"launchd", []string{"launchd", "system/com.example.agent", "--loaded"}, condition.LaunchdLoaded},
		{"pidfile", []string{"pidfile", "/tmp/app.pid", "--stopped"}, condition.ProcessStopped},
		{"lockfile", []string{"lockfile", "/tmp/app.lock", "--older-than", "5s"}, condition.LockfilePresent},
		{"permission", []string{"permission", "/tmp/app", "--mode", "0640", "--type", "file"}, condition.NewPermission("/tmp/app")},
		{"checksum", []string{"checksum", "/tmp/app", "--equals", "sha512:abc"}, condition.ChecksumAuto},
		{"archive", []string{"archive", "/tmp/app.zip", "--matches", "bin/*", "--format", "zip"}, condition.ArchiveZip},
		{"cosign", []string{"cosign", "--blob", "artifact", "--signature", "artifact.sig", "--certificate-identity", "id", "--certificate-oidc-issuer", "issuer"}, condition.CosignBlob},
		{"ntp", []string{"ntp", "127.0.0.1:123", "--max-offset", "1s", "--timeout", "500ms"}, "ntp"},
		{"icmp", []string{"icmp", "127.0.0.1", "--count", "3", "--timeout", "2s"}, "icmp"},
		{"grpc", []string{"grpc", "127.0.0.1:50051", "--service", "svc", "--tls", "--timeout", "2s"}, "grpc"},
		{"websocket", []string{"websocket", "ws://127.0.0.1/ready", "--matches", "ready", "--header", "Authorization=Bearer token", "--timeout", "2s"}, "websocket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond, err := parseCondition(tt.args)
			if err != nil {
				t.Fatalf("parseCondition() error = %v", err)
			}
			assertParsedBackend(t, cond, tt.want)
		})
	}
}

func assertParsedBackend(t *testing.T, cond condition.Condition, want any) {
	t.Helper()
	switch expected := want.(type) {
	case condition.LaunchdState:
		got := cond.(*condition.LaunchdCondition)
		if got.State != expected {
			t.Fatalf("state = %s, want %s", got.State, expected)
		}
	case condition.ProcessState:
		got := cond.(*condition.PIDFileCondition)
		if got.State != expected {
			t.Fatalf("state = %s, want %s", got.State, expected)
		}
	case condition.LockfileState:
		got := cond.(*condition.LockfileCondition)
		if got.State != expected || got.OlderThan != 5*time.Second {
			t.Fatalf("lockfile = %+v", got)
		}
	case *condition.PermissionCondition:
		got := cond.(*condition.PermissionCondition)
		if got.Mode != 0o640 || got.Type != condition.PermissionFile {
			t.Fatalf("permission = %+v", got)
		}
	case condition.ChecksumAlgorithm:
		got := cond.(*condition.ChecksumCondition)
		if got.Algorithm != expected {
			t.Fatalf("algorithm = %s, want %s", got.Algorithm, expected)
		}
	case condition.ArchiveFormat:
		got := cond.(*condition.ArchiveCondition)
		if got.Format != expected || got.Matches != "bin/*" {
			t.Fatalf("archive = %+v", got)
		}
	case condition.CosignMode:
		got := cond.(*condition.CosignCondition)
		if got.Mode != expected || got.Signature != "artifact.sig" || got.Identity != "id" || got.OIDCIssuer != "issuer" {
			t.Fatalf("cosign = %+v", got)
		}
	case string:
		if cond.Descriptor().Backend != expected {
			t.Fatalf("backend = %s, want %s", cond.Descriptor().Backend, expected)
		}
		assertParsedBackendDetails(t, cond)
	default:
		t.Fatalf("unsupported expected type %T", want)
	}
}

func assertParsedBackendDetails(t *testing.T, cond condition.Condition) {
	t.Helper()
	switch got := cond.(type) {
	case *condition.NTPCondition:
		if got.AttemptTimeout != 500*time.Millisecond {
			t.Fatalf("ntp = %+v", got)
		}
	case *condition.ICMPCondition:
		if got.Count != 3 || got.AttemptTimeout != 2*time.Second {
			t.Fatalf("icmp = %+v", got)
		}
	case *condition.GRPCCondition:
		if !got.UseTLS || got.AttemptTimeout != 2*time.Second {
			t.Fatalf("grpc = %+v", got)
		}
	case *condition.WebSocketCondition:
		if got.Matches == nil || got.Headers["Authorization"] != "Bearer token" || got.AttemptTimeout != 2*time.Second {
			t.Fatalf("websocket = %+v", got)
		}
	}
}

func TestParseExtraBackendsInvalid(t *testing.T) {
	tests := [][]string{
		{"launchd", "svc", "--loaded", "--running"},
		{"pidfile", "/tmp/app.pid", "--running", "--stopped"},
		{"lockfile", "/tmp/app.lock", "--present", "--absent"},
		{"permission", "/tmp/app", "--uid", "1", "--user", "root"},
		{"ntp", "time.example", "--max-offset", "not-duration"},
		{"icmp", "127.0.0.1", "--timeout", "nope"},
		{"grpc", "127.0.0.1:1", "--timeout", "nope"},
		{"websocket", "ws://127.0.0.1", "--matches", "["},
	}
	for _, args := range tests {
		if _, err := parseCondition(args); err == nil {
			t.Fatalf("parseCondition(%v) succeeded, want error", args)
		}
	}
}

func TestPermissionNumericUserAndGroupOptions(t *testing.T) {
	cond := condition.NewPermission("/tmp/app")
	if err := applyOwnerOption(cond, -1, "1001"); err != nil {
		t.Fatalf("applyOwnerOption() error = %v", err)
	}
	if cond.UID == nil || *cond.UID != 1001 {
		t.Fatalf("uid = %v, want 1001", cond.UID)
	}
	if err := applyGroupOption(cond, -1, "1002"); err != nil {
		t.Fatalf("applyGroupOption() error = %v", err)
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
