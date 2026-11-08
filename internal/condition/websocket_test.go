package condition

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestWebSocketConditionCheckAgainstLocalServer(t *testing.T) {
	headerC := make(chan string, 1)
	payloadC := make(chan string, 1)
	errC := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			errC <- err
			return
		}
		defer func() { _ = conn.Close() }()
		headerC <- r.Header.Get("Authorization")
		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		if _, err := fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
			errC <- err
			return
		}
		if err := rw.Flush(); err != nil {
			errC <- err
			return
		}
		opcode, payload, err := readWebSocketClientFrame(rw)
		if err != nil {
			errC <- err
			return
		}
		if opcode != websocketOpcodeText {
			errC <- fmt.Errorf("opcode = %d", opcode)
			return
		}
		payloadC <- string(payload)
		if _, err := rw.Write(websocketServerFrame(0x81, []byte("ready-42"))); err != nil {
			errC <- err
			return
		}
		errC <- rw.Flush()
	}))
	t.Cleanup(server.Close)

	cond := NewWebSocket("ws://" + strings.TrimPrefix(server.URL, "http://") + "/events")
	cond.Send = "hello"
	cond.Contains = "ready"
	cond.Headers["Authorization"] = "Bearer token"
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("websocket status = %s, err = %v", result.Status, result.Err)
	}
	if got := receiveString(t, headerC); got != "Bearer token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := receiveString(t, payloadC); got != "hello" {
		t.Fatalf("payload = %q", got)
	}
	if err := receiveError(t, errC); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketPingAgainstLocalServer(t *testing.T) {
	errC := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			errC <- err
			return
		}
		defer func() { _ = conn.Close() }()
		if err := writeWebSocketSwitchingProtocols(rw, r); err != nil {
			errC <- err
			return
		}
		opcode, payload, err := readWebSocketClientFrame(rw)
		if err != nil {
			errC <- err
			return
		}
		if opcode != websocketOpcodePing {
			errC <- fmt.Errorf("opcode = %d", opcode)
			return
		}
		if _, err := rw.Write(websocketServerFrame(0x8a, payload)); err != nil {
			errC <- err
			return
		}
		errC <- rw.Flush()
	}))
	t.Cleanup(server.Close)

	cond := NewWebSocket("ws://" + strings.TrimPrefix(server.URL, "http://") + "/events")
	cond.Ping = true
	cond.ReadTimeout = time.Second
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("websocket status = %s, err = %v", result.Status, result.Err)
	}
	if err := receiveError(t, errC); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketExpectedCloseAgainstLocalServer(t *testing.T) {
	errC := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			errC <- err
			return
		}
		defer func() { _ = conn.Close() }()
		if err := writeWebSocketSwitchingProtocols(rw, r); err != nil {
			errC <- err
			return
		}
		payload := []byte{0x03, 0xe8}
		if _, err := rw.Write(websocketServerFrame(0x88, payload)); err != nil {
			errC <- err
			return
		}
		errC <- rw.Flush()
	}))
	t.Cleanup(server.Close)

	cond := NewWebSocket("ws://" + strings.TrimPrefix(server.URL, "http://") + "/events")
	cond.ExpectCloseCode = 1000
	cond.ReadTimeout = time.Second
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("websocket status = %s, err = %v", result.Status, result.Err)
	}
	if err := receiveError(t, errC); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketCheckMessageMismatchBranches(t *testing.T) {
	server := newWebSocketMessageServer(t, "cold")

	contains := NewWebSocket("ws://" + strings.TrimPrefix(server.URL, "http://") + "/events")
	contains.Contains = "ready"
	if result := contains.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("contains mismatch status = %s, want unsatisfied", result.Status)
	}

	matches := NewWebSocket("ws://" + strings.TrimPrefix(server.URL, "http://") + "/events")
	matches.Matches = regexp.MustCompile(`ready-\d+`)
	if result := matches.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("regex mismatch status = %s, want unsatisfied", result.Status)
	}

	invalid := NewWebSocket("http://example.com/events")
	if result := invalid.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("invalid config status = %s, want fatal", result.Status)
	}
}

func TestWebSocketAcceptMatchesRFC6455Example(t *testing.T) {
	got := websocketAccept("dGhlIHNhbXBsZSBub25jZQ==")
	if got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept = %q", got)
	}
}

func TestWebSocketFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(websocketServerFrame(0x81, []byte("ready")))
	frame, err := readWebSocketFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.opcode != 1 || string(frame.payload) != "ready" {
		t.Fatalf("opcode=%d payload=%q", frame.opcode, frame.payload)
	}
	var client bytes.Buffer
	if err := writeWebSocketText(&client, "ready"); err != nil {
		t.Fatal(err)
	}
	raw := client.Bytes()
	if len(raw) < 6 || raw[1]&0x80 == 0 {
		t.Fatalf("client frame was not masked: %v", raw[:minInt(len(raw), 6)])
	}
}

func TestWebSocketHelpers(t *testing.T) {
	if got := websocketAddress(mustURL(t, "ws://example.com/ready")); got != "example.com:80" {
		t.Fatalf("ws address = %q", got)
	}
	if got := websocketAddress(mustURL(t, "wss://example.com/ready")); got != "example.com:443" {
		t.Fatalf("wss address = %q", got)
	}
	if got := websocketAddress(mustURL(t, "ws://example.com:8080/ready")); got != "example.com:8080" {
		t.Fatalf("explicit address = %q", got)
	}
	if got := websocketAttemptTimeout(0); got != 2*time.Second {
		t.Fatalf("default attempt timeout = %s", got)
	}
	if got := websocketAttemptTimeout(100 * time.Millisecond); got != 100*time.Millisecond {
		t.Fatalf("explicit attempt timeout = %s", got)
	}
	if err := validateWebSocketConfig(NewWebSocket("http://example.com")); err == nil {
		t.Fatal("invalid websocket scheme succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws:///ready")); err == nil {
		t.Fatal("missing websocket host succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws://user@example.com/ready")); err == nil {
		t.Fatal("websocket userinfo succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws://example.com/ready#fragment")); err == nil {
		t.Fatal("websocket fragment succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws://example.com:bad/ready")); err == nil {
		t.Fatal("websocket bad port succeeded")
	}
	if err := validateWebSocketURL(&url.URL{Scheme: "ws", Host: "bad host"}); err == nil {
		t.Fatal("websocket host with space succeeded")
	}
	if err := validateWebSocketURL(&url.URL{Scheme: "ws", Host: "example.com:bad"}); err == nil {
		t.Fatal("websocket raw bad port succeeded")
	}
	negativeAttemptTimeout := NewWebSocket("ws://example.com/ready")
	negativeAttemptTimeout.AttemptTimeout = -time.Second
	if err := validateWebSocketConfig(negativeAttemptTimeout); err == nil {
		t.Fatal("negative websocket attempt timeout succeeded")
	}
	dup := NewWebSocket("ws://example.com/ready")
	dup.Headers["Connection"] = "keep-alive"
	if err := validateWebSocketConfig(dup); err == nil {
		t.Fatal("reserved websocket header succeeded")
	}
	badHeader := NewWebSocket("ws://example.com/ready")
	badHeader.Headers["Bad Header"] = "value"
	if err := validateWebSocketConfig(badHeader); err == nil {
		t.Fatal("invalid websocket header succeeded")
	}
	tooManyHeaders := NewWebSocket("ws://example.com/ready")
	for i := 0; i <= maxWebSocketHeaderCount; i++ {
		tooManyHeaders.Headers[fmt.Sprintf("X-Test-%d", i)] = "value"
	}
	if err := validateWebSocketConfig(tooManyHeaders); err == nil {
		t.Fatal("too many websocket headers succeeded")
	}
	largeSend := NewWebSocket("ws://example.com/ready")
	largeSend.Send = strings.Repeat("x", maxWebSocketSendBytes+1)
	if err := validateWebSocketConfig(largeSend); err == nil {
		t.Fatal("oversized websocket send succeeded")
	}
	largeHeaders := NewWebSocket("ws://example.com/ready")
	largeHeaders.Headers["X-Big"] = strings.Repeat("x", maxWebSocketHeaderBytes+1)
	if err := validateWebSocketConfig(largeHeaders); err == nil {
		t.Fatal("oversized websocket headers succeeded")
	}
	negativeReadTimeout := NewWebSocket("ws://example.com/ready")
	negativeReadTimeout.ReadTimeout = -time.Second
	if err := validateWebSocketConfig(negativeReadTimeout); err == nil {
		t.Fatal("negative websocket read timeout succeeded")
	}
	badCloseCode := NewWebSocket("ws://example.com/ready")
	badCloseCode.ExpectCloseCode = 70000
	if err := validateWebSocketConfig(badCloseCode); err == nil {
		t.Fatal("oversized websocket close code succeeded")
	}
	if desc := NewWebSocket("wss://example.com/events?token=secret").Descriptor(); strings.Contains(desc.Target, "secret") {
		t.Fatalf("descriptor leaked query secret: %+v", desc)
	}
	var medium bytes.Buffer
	header := appendWebSocketLength([]byte{0x81}, 126, false)
	medium.Write(header)
	medium.Write(bytes.Repeat([]byte("x"), 126))
	if frame, err := readWebSocketFrame(&medium); err != nil || frame.opcode != 1 || len(frame.payload) != 126 {
		t.Fatalf("medium frame opcode=%d len=%d err=%v", frame.opcode, len(frame.payload), err)
	}
	var large bytes.Buffer
	header = appendWebSocketLength([]byte{0x81}, 66000, false)
	large.Write(header)
	large.Write(bytes.Repeat([]byte("x"), 66000))
	if frame, err := readWebSocketFrame(&large); err != nil || len(frame.payload) != 66000 {
		t.Fatalf("large frame len=%d err=%v", len(frame.payload), err)
	}
	if byteWebSocketLength(200) != 125 {
		t.Fatal("byteWebSocketLength did not clamp")
	}
	if !httpHeaderHasToken(http.Header{"Connection": []string{"keep-alive, Upgrade"}}, "Connection", "upgrade") {
		t.Fatal("Connection token was not detected")
	}
}

func TestWebSocketFrameValidation(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "masked server frame", data: append([]byte{0x81, 0x80, 0, 0, 0, 0}, nil...)},
		{name: "rsv set", data: []byte{0xc1, 0}},
		{name: "reserved opcode", data: []byte{0x83, 0}},
		{name: "fragmented control", data: []byte{0x09, 0}},
		{name: "non-minimal 16-bit", data: []byte{0x81, 126, 0, 1, 'x'}},
		{name: "non-minimal 64-bit", data: []byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 126}},
		{name: "64-bit high bit", data: []byte{0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := readWebSocketFrame(bytes.NewReader(tt.data)); err == nil {
				t.Fatal("invalid websocket frame succeeded")
			}
		})
	}
	controlTooLarge := appendWebSocketLength([]byte{0x89}, 126, false)
	controlTooLarge = append(controlTooLarge, bytes.Repeat([]byte("x"), 126)...)
	if _, err := readWebSocketFrame(bytes.NewReader(controlTooLarge)); err == nil {
		t.Fatal("oversized control frame succeeded")
	}
}

func TestWebSocketFragmentedTextAndControls(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(websocketServerFrame(0x01, []byte("rea")))
	buf.Write(websocketServerFrame(0x89, []byte("ping")))
	buf.Write(websocketServerFrame(0x80, []byte("dy")))
	var pong bytes.Buffer
	got, err := readWebSocketTextWithPong(&buf, &pong)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ready" {
		t.Fatalf("message = %q", got)
	}
	if pong.Len() == 0 {
		t.Fatal("ping did not produce pong")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x88, nil))); err == nil {
		t.Fatal("close frame succeeded")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x80, []byte("orphan")))); err == nil {
		t.Fatal("orphan continuation succeeded")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x82, []byte("binary")))); err == nil {
		t.Fatal("binary frame succeeded")
	}
	if _, _, err := (&websocketMessageState{}).handleFrame(websocketFrame{opcode: 0x0b}, nil); err == nil {
		t.Fatal("unsupported opcode succeeded")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x81, []byte{0xff}))); err == nil {
		t.Fatal("invalid UTF-8 websocket text succeeded")
	}
	if err := writeAll(shortWriter{}, []byte("x")); err == nil {
		t.Fatal("short write succeeded")
	}
}

func TestWebSocketControlFrameBranches(t *testing.T) {
	var pong bytes.Buffer
	pong.Write(websocketServerFrame(0x89, []byte("server-ping")))
	pong.Write(websocketServerFrame(0x8a, []byte("client-pong")))
	if err := readWebSocketPong(&pong); err != nil {
		t.Fatalf("readWebSocketPong() error = %v", err)
	}
	if err := readWebSocketPong(bytes.NewReader(websocketServerFrame(0x88, nil))); err == nil {
		t.Fatal("close frame was accepted as pong")
	}
	var closeBuf bytes.Buffer
	closeBuf.Write(websocketServerFrame(0x89, []byte("server-ping")))
	closeBuf.Write(websocketServerFrame(0x88, []byte{0x03, 0xe9}))
	code, err := readWebSocketCloseCode(&closeBuf)
	if err != nil || code != 1001 {
		t.Fatalf("close code = %d, err = %v; want 1001, nil", code, err)
	}
	code, err = readWebSocketCloseCode(bytes.NewReader(websocketServerFrame(0x88, nil)))
	if err != nil || code != 1005 {
		t.Fatalf("empty close code = %d, err = %v; want 1005, nil", code, err)
	}
	if err := checkWebSocketClose(readOnlyConn{Reader: bytes.NewReader(websocketServerFrame(0x88, []byte{0x03, 0xe9}))}, 1000, 0); err == nil {
		t.Fatal("mismatched close code succeeded")
	}
	if err := writeWebSocketPing(io.Discard, bytes.Repeat([]byte("x"), 126)); err == nil {
		t.Fatal("oversized ping succeeded")
	}
	if err := writeWebSocketPong(nil, []byte("ignored")); err != nil {
		t.Fatalf("nil pong writer error = %v", err)
	}
}

func TestWebSocketFragmentStateErrorBranches(t *testing.T) {
	state := websocketMessageState{fragmented: true}
	if _, _, err := state.handleFrame(websocketFrame{fin: true, opcode: websocketOpcodeText, payload: []byte("again")}, nil); err == nil {
		t.Fatal("text start during fragmented message succeeded")
	}
	state = websocketMessageState{}
	if done, _, err := state.handleFrame(websocketFrame{fin: false, opcode: websocketOpcodeText, payload: []byte("rea")}, nil); done || err != nil {
		t.Fatalf("fragment start done=%v err=%v, want false nil", done, err)
	}
	if done, _, err := state.handleFrame(websocketFrame{fin: false, opcode: websocketOpcodeContinuation, payload: []byte("d")}, nil); done || err != nil {
		t.Fatalf("fragment continuation done=%v err=%v, want false nil", done, err)
	}
	if done, got, err := state.handleFrame(websocketFrame{fin: true, opcode: websocketOpcodeContinuation, payload: []byte("y")}, nil); !done || err != nil || got != "ready" {
		t.Fatalf("fragment finish done=%v got=%q err=%v", done, got, err)
	}
	state = websocketMessageState{fragmented: true, payload: bytes.Repeat([]byte("x"), maxWebSocketMessageBytes)}
	if _, _, err := state.handleFrame(websocketFrame{fin: false, opcode: websocketOpcodeContinuation, payload: []byte("x")}, nil); err == nil {
		t.Fatal("oversized fragmented message succeeded")
	}
	state = websocketMessageState{fragmented: true}
	if _, _, err := state.handleFrame(websocketFrame{fin: true, opcode: websocketOpcodeContinuation, payload: []byte{0xff}}, nil); err == nil {
		t.Fatal("invalid UTF-8 fragmented message succeeded")
	}
}

func TestWebSocketHandshakePreservesBufferedFrameBytes(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			done <- err
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: keep-alive, Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + websocketAccept(key) + "\r\n\r\n"
		_, err = server.Write(append([]byte(response), websocketServerFrame(0x81, []byte("ready"))...))
		done <- err
	}()
	reader, err := websocketHandshake(client, mustURL(t, "ws://example.com/events"), nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := readWebSocketText(&websocketConn{Conn: client, reader: reader})
	if err != nil {
		t.Fatal(err)
	}
	if message != "ready" {
		t.Fatalf("message = %q", message)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketHandshakeRejectsOversizedHeaders(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("HTTP/1.1 101 Switching Protocols\r\nX-Big: " + strings.Repeat("x", maxWebSocketHandshakeBytes) + "\r\n\r\n"))
	if _, err := readWebSocketHandshakeResponse(reader); err == nil {
		t.Fatal("oversized websocket handshake succeeded")
	}
}

func TestWebSocketHandshakeErrorBranches(t *testing.T) {
	tests := []struct {
		name string
		resp string
	}{
		{"malformed status", "HTTP/1.1\r\n\r\n"},
		{"bad status code", "HTTP/1.1 ok Switching Protocols\r\n\r\n"},
		{"not switching", "HTTP/1.1 200 OK\r\n\r\n"},
		{"missing upgrade", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: accept\r\n\r\n"},
		{"missing connection", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: accept\r\n\r\n"},
		{"accept mismatch", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n"},
		{"malformed header", "HTTP/1.1 101 Switching Protocols\r\nBad Header\r\n\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := websocketHandshakeWithResponse(tt.resp); err == nil {
				t.Fatal("handshake succeeded")
			}
		})
	}
	var tooMany strings.Builder
	tooMany.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	for i := 0; i <= maxWebSocketHeaderCount; i++ {
		_, _ = fmt.Fprintf(&tooMany, "X-Test-%d: value\r\n", i)
	}
	tooMany.WriteString("\r\n")
	if _, err := readWebSocketHandshakeResponse(bufio.NewReader(strings.NewReader(tooMany.String()))); err == nil {
		t.Fatal("handshake with too many headers succeeded")
	}
	if _, err := readWebSocketHandshakeResponse(bufio.NewReader(strings.NewReader("HTTP/1.1 101 Switching Protocols"))); err == nil {
		t.Fatal("unterminated handshake succeeded")
	}
}

func TestWebSocketRegexAndHeaderRequest(t *testing.T) {
	cond := NewWebSocket("ws://example.com/events")
	cond.Matches = regexp.MustCompile(`ready-\d+`)
	if !cond.Matches.MatchString("ready-42") {
		t.Fatal("regex did not match")
	}
	req := websocketHandshakeRequest("/events", "example.com", "key", map[string]string{"Authorization": "Bearer token"})
	if !strings.Contains(req, "Authorization: Bearer token\r\n") {
		t.Fatalf("request = %q", req)
	}
}

func TestDecodeWebSocketAcceptUsesBase64(t *testing.T) {
	if _, err := base64.StdEncoding.DecodeString(websocketAccept("test")); err != nil {
		t.Fatal(err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestWebSocketReadLengthExtended(t *testing.T) {
	var buf bytes.Buffer
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], 66000)
	buf.Write(raw[:])
	length, masked, err := readWebSocketLength(&buf, 127)
	if err != nil {
		t.Fatal(err)
	}
	if length != 66000 || masked {
		t.Fatalf("length=%d masked=%v", length, masked)
	}
}

func receiveString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for string")
		return ""
	}
}

func receiveError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error")
		return nil
	}
}

func newWebSocketMessageServer(t *testing.T, message string) *httptest.Server {
	t.Helper()
	errC := make(chan error, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			errC <- err
			return
		}
		defer func() { _ = conn.Close() }()
		if err := writeWebSocketSwitchingProtocols(rw, r); err != nil {
			errC <- err
			return
		}
		if _, err := rw.Write(websocketServerFrame(0x81, []byte(message))); err != nil {
			errC <- err
			return
		}
		errC <- rw.Flush()
	}))
	t.Cleanup(func() {
		server.Close()
		for {
			select {
			case err := <-errC:
				if err != nil {
					t.Fatal(err)
				}
			default:
				return
			}
		}
	})
	return server
}

func websocketHandshakeWithResponse(response string) error {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
			done <- err
			return
		}
		_, err := io.WriteString(server, response)
		done <- err
	}()
	_, err := websocketHandshake(client, mustURLNoTest("ws://example.com/events"), nil)
	if waitErr := <-done; waitErr != nil {
		return waitErr
	}
	return err
}

func writeWebSocketSwitchingProtocols(rw *bufio.ReadWriter, r *http.Request) error {
	accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
	if _, err := fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
		return err
	}
	return rw.Flush()
}

func readWebSocketClientFrame(r io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var raw [2]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(raw[:]))
	case 127:
		var raw [8]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return 0, nil, err
		}
		size := binary.BigEndian.Uint64(raw[:])
		if size > uint64(^uint(0)>>1) {
			return 0, nil, fmt.Errorf("client frame too large")
		}
		length = int(size)
	}
	if header[1]&0x80 == 0 {
		return 0, nil, fmt.Errorf("client frame was not masked")
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return header[0] & 0x0f, payload, nil
}

func websocketServerFrame(first byte, payload []byte) []byte {
	header := appendWebSocketLength([]byte{first}, len(payload), false)
	return append(header, payload...)
}

func mustURLNoTest(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) {
	return 0, nil
}

type readOnlyConn struct {
	io.Reader
}

func (readOnlyConn) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func (readOnlyConn) Close() error {
	return nil
}

func (readOnlyConn) LocalAddr() net.Addr {
	return nil
}

func (readOnlyConn) RemoteAddr() net.Addr {
	return nil
}

func (readOnlyConn) SetDeadline(time.Time) error {
	return nil
}

func (readOnlyConn) SetReadDeadline(time.Time) error {
	return nil
}

func (readOnlyConn) SetWriteDeadline(time.Time) error {
	return nil
}
