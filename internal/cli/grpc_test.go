package cli

import (
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseGRPCCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"grpc", "127.0.0.1:50051", "--service", "svc", "--method", "/custom.Service/Ready", "--reflect", "--tls", "--timeout", "2s"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.GRPCCondition)
	if !got.UseTLS || !got.Reflect || got.Method != "/custom.Service/Ready" || got.AttemptTimeout != 2*time.Second {
		t.Fatalf("grpc = %+v", got)
	}
	if _, err := parseCondition([]string{"grpc", "127.0.0.1:1", "--timeout", "nope"}); err == nil {
		t.Fatal("invalid gRPC duration succeeded")
	}
}
