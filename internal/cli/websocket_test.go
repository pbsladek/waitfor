package cli

import (
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseWebSocketCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"websocket", "ws://127.0.0.1/ready", "--matches", "ready", "--ping", "--expect-close-code", "1000", "--read-timeout", "1s", "--header", "Authorization=Bearer token", "--timeout", "2s"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.WebSocketCondition)
	if got.Matches == nil || !got.Ping || got.ExpectCloseCode != 1000 || got.ReadTimeout != time.Second || got.Headers["Authorization"] != "Bearer token" || got.AttemptTimeout != 2*time.Second {
		t.Fatalf("websocket = %+v", got)
	}
	if _, err := parseCondition([]string{"websocket", "ws://127.0.0.1", "--matches", "["}); err == nil {
		t.Fatal("invalid websocket regex succeeded")
	}
}
