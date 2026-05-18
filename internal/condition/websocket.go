package condition

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 requires SHA-1 for Sec-WebSocket-Accept.
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const maxWebSocketMessageBytes = 16 * 1024 * 1024
const maxWebSocketSendBytes = 1 * 1024 * 1024
const maxWebSocketHeaderCount = 32
const maxWebSocketHeaderBytes = 16 * 1024
const maxWebSocketHandshakeBytes = 32 * 1024

const (
	websocketOpcodeContinuation byte = 0
	websocketOpcodeText         byte = 1
	websocketOpcodeBinary       byte = 2
	websocketOpcodeClose        byte = 8
	websocketOpcodePing         byte = 9
	websocketOpcodePong         byte = 10
)

type WebSocketCondition struct {
	URL            string
	Send           string
	Contains       string
	Matches        *regexp.Regexp
	Headers        map[string]string
	AttemptTimeout time.Duration
}

type websocketFrame struct {
	fin     bool
	opcode  byte
	masked  bool
	payload []byte
}

type websocketConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *websocketConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func NewWebSocket(rawURL string) *WebSocketCondition {
	return &WebSocketCondition{URL: rawURL, Headers: map[string]string{}, AttemptTimeout: 2 * time.Second}
}

func (c *WebSocketCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "websocket", Target: redactHTTPURL(c.URL)}
}

func (c *WebSocketCondition) Check(ctx context.Context) Result {
	if err := validateWebSocketConfig(c); err != nil {
		return Fatal(err)
	}
	message, err := websocketRoundTrip(ctx, c)
	if err != nil {
		return Unsatisfied("", err)
	}
	if c.Contains != "" && !strings.Contains(message, c.Contains) {
		return Unsatisfied("websocket message did not contain substring", fmt.Errorf("websocket message mismatch"))
	}
	if c.Matches != nil && !c.Matches.MatchString(message) {
		return Unsatisfied("websocket message did not match regex", fmt.Errorf("websocket message mismatch"))
	}
	if c.Contains != "" || c.Matches != nil {
		return Satisfied("websocket message matched")
	}
	return Satisfied("websocket connected")
}

func validateWebSocketConfig(c *WebSocketCondition) error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("invalid websocket URL: %w", err)
	}
	if err := validateWebSocketURL(u); err != nil {
		return err
	}
	if len(c.Send) > maxWebSocketSendBytes {
		return fmt.Errorf("websocket send payload is too large")
	}
	if c.AttemptTimeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	return validateWebSocketHeaders(c.Headers)
}

func validateWebSocketURL(u *url.URL) error {
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("websocket URL must use ws or wss")
	}
	if u.Host == "" {
		return fmt.Errorf("websocket URL host is required")
	}
	if err := validateWebSocketAuthority(u); err != nil {
		return err
	}
	if u.User != nil {
		return fmt.Errorf("websocket URL userinfo is not allowed")
	}
	if u.Fragment != "" {
		return fmt.Errorf("websocket URL fragment is not allowed")
	}
	return nil
}

func validateWebSocketAuthority(u *url.URL) error {
	host := u.Hostname()
	if host == "" || containsSpaceOrControl(host) {
		return fmt.Errorf("invalid websocket URL host")
	}
	port := u.Port()
	if port == "" {
		if strings.Contains(u.Host, ":") && (!strings.HasPrefix(u.Host, "[") || !strings.HasSuffix(u.Host, "]")) {
			return fmt.Errorf("invalid websocket URL port")
		}
		return nil
	}
	if !validPortNumber(port) {
		return fmt.Errorf("invalid websocket URL port")
	}
	return nil
}

func validateWebSocketHeaders(headers map[string]string) error {
	if err := validateHTTPHeaders(headers); err != nil {
		return err
	}
	if len(headers) > maxWebSocketHeaderCount {
		return fmt.Errorf("too many websocket headers")
	}
	total := 0
	for name := range headers {
		if reservedWebSocketHeader(name) {
			return fmt.Errorf("websocket header %q is managed by waitfor", name)
		}
		total += len(name) + len(headers[name])
		if total > maxWebSocketHeaderBytes {
			return fmt.Errorf("websocket headers are too large")
		}
	}
	return nil
}

func reservedWebSocketHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Host", "Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Accept":
		return true
	default:
		return false
	}
}

func websocketRoundTrip(ctx context.Context, c *WebSocketCondition) (string, error) {
	u, _ := url.Parse(c.URL)
	timeout := c.AttemptTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	conn, err := websocketDial(ctx, u, c.Headers, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	stopCancelWatcher := closeOnContextDone(ctx, conn)
	defer stopCancelWatcher()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if c.Send != "" {
		if err := writeWebSocketText(conn, c.Send); err != nil {
			return "", err
		}
	}
	if c.Contains == "" && c.Matches == nil {
		return "", nil
	}
	return readWebSocketTextWithPong(conn, conn)
}

func websocketDial(ctx context.Context, u *url.URL, headers map[string]string, timeout time.Duration) (net.Conn, error) {
	address := websocketAddress(u)
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	stopCancelWatcher := closeOnContextDone(ctx, conn)
	defer stopCancelWatcher()
	if u.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	reader, err := websocketHandshake(conn, u, headers)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &websocketConn{Conn: conn, reader: reader}, nil
}

func websocketAddress(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if u.Scheme == "wss" {
		return net.JoinHostPort(host, "443")
	}
	return net.JoinHostPort(host, "80")
}

func closeOnContextDone(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func websocketHandshake(conn net.Conn, u *url.URL, headers map[string]string) (*bufio.Reader, error) {
	key, err := websocketKey()
	if err != nil {
		return nil, err
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := websocketHandshakeRequest(path, u.Host, key, headers)
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := readWebSocketHandshakeResponse(reader)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("websocket HTTP status %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("websocket upgrade header missing")
	}
	if !httpHeaderHasToken(resp.Header, "Connection", "upgrade") {
		return nil, fmt.Errorf("websocket connection upgrade token missing")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != websocketAccept(key) {
		return nil, fmt.Errorf("websocket accept mismatch")
	}
	return reader, nil
}

func readWebSocketHandshakeResponse(reader *bufio.Reader) (*http.Response, error) {
	total := 0
	statusLine, err := readWebSocketHandshakeLine(reader, &total)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return nil, fmt.Errorf("websocket malformed HTTP status")
	}
	statusCode, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("websocket malformed HTTP status")
	}
	header, err := readWebSocketHandshakeHeaders(reader, &total)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: statusCode,
		Proto:      fields[0],
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func readWebSocketHandshakeHeaders(reader *bufio.Reader, total *int) (http.Header, error) {
	header := http.Header{}
	count := 0
	for {
		line, err := readWebSocketHandshakeLine(reader, total)
		if err != nil {
			return nil, err
		}
		if line == "" {
			return header, nil
		}
		count++
		if count > maxWebSocketHeaderCount {
			return nil, fmt.Errorf("websocket handshake has too many headers")
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !validHTTPToken(name) || !validHTTPHeaderValue(strings.TrimSpace(value)) {
			return nil, fmt.Errorf("websocket malformed HTTP header")
		}
		header.Add(name, strings.TrimSpace(value))
	}
}

func readWebSocketHandshakeLine(reader *bufio.Reader, total *int) (string, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		*total += len(part)
		if *total > maxWebSocketHandshakeBytes {
			return "", fmt.Errorf("websocket handshake response too large")
		}
		line = append(line, part...)
		if err == nil {
			break
		}
		if err != bufio.ErrBufferFull {
			return "", err
		}
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func httpHeaderHasToken(header http.Header, name, want string) bool {
	for _, value := range header.Values(name) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

func websocketHandshakeRequest(path, host, key string, headers map[string]string) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", path, host, key)
	for name, value := range headers {
		_, _ = fmt.Fprintf(&b, "%s: %s\r\n", name, value)
	}
	b.WriteString("\r\n")
	return b.String()
}

func websocketKey() (string, error) {
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key[:]), nil
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID)) // #nosec G401 -- RFC 6455 requires SHA-1 for Sec-WebSocket-Accept.
	return base64.StdEncoding.EncodeToString(sum[:])
}

func writeWebSocketText(w io.Writer, message string) error {
	payload := []byte(message)
	header := []byte{0x81}
	header = appendWebSocketLength(header, len(payload), true)
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	frame := append(header, masked...)
	return writeAll(w, frame)
}

func writeWebSocketPong(w io.Writer, payload []byte) error {
	if w == nil {
		return nil
	}
	header := []byte{0x8a}
	header = appendWebSocketLength(header, len(payload), true)
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	return writeAll(w, append(header, masked...))
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func appendWebSocketLength(header []byte, length int, masked bool) []byte {
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	if length < 126 {
		return append(header, maskBit|byteWebSocketLength(length))
	}
	if length <= 0xffff {
		var buf [2]byte
		binary.BigEndian.PutUint16(buf[:], uint16(length))
		return append(append(header, maskBit|126), buf[:]...)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(length))
	return append(append(header, maskBit|127), buf[:]...)
}

func byteWebSocketLength(length int) byte {
	if length < 0 {
		return 0
	}
	if length > 125 {
		return 125
	}
	return byte(length)
}

func readWebSocketText(r io.Reader) (string, error) {
	return readWebSocketTextWithPong(r, nil)
}

func readWebSocketTextWithPong(r io.Reader, pong io.Writer) (string, error) {
	state := websocketMessageState{}
	for {
		frame, err := readWebSocketFrame(r)
		if err != nil {
			return "", err
		}
		done, message, err := state.handleFrame(frame, pong)
		if done || err != nil {
			return message, err
		}
	}
}

type websocketMessageState struct {
	payload    []byte
	fragmented bool
}

func (s *websocketMessageState) handleFrame(frame websocketFrame, pong io.Writer) (bool, string, error) {
	switch frame.opcode {
	case websocketOpcodeText:
		return s.startText(frame)
	case websocketOpcodeContinuation:
		return s.continueText(frame)
	case websocketOpcodePing:
		return false, "", writeWebSocketPong(pong, frame.payload)
	case websocketOpcodePong:
		return false, "", nil
	case websocketOpcodeClose:
		return false, "", fmt.Errorf("websocket close frame received")
	case websocketOpcodeBinary:
		return false, "", fmt.Errorf("websocket frame opcode %d, expected text", frame.opcode)
	default:
		return false, "", fmt.Errorf("websocket unsupported opcode %d", frame.opcode)
	}
}

func (s *websocketMessageState) startText(frame websocketFrame) (bool, string, error) {
	if s.fragmented {
		return false, "", fmt.Errorf("websocket text frame started before fragmented message completed")
	}
	if frame.fin {
		if !utf8.Valid(frame.payload) {
			return false, "", fmt.Errorf("websocket text is not valid UTF-8")
		}
		return true, string(frame.payload), nil
	}
	s.fragmented = true
	s.payload = append(s.payload[:0], frame.payload...)
	return false, "", nil
}

func (s *websocketMessageState) continueText(frame websocketFrame) (bool, string, error) {
	if !s.fragmented {
		return false, "", fmt.Errorf("websocket continuation without fragmented message")
	}
	s.payload = append(s.payload, frame.payload...)
	if len(s.payload) > maxWebSocketMessageBytes {
		return false, "", fmt.Errorf("websocket message too large")
	}
	if !frame.fin {
		return false, "", nil
	}
	s.fragmented = false
	if !utf8.Valid(s.payload) {
		return false, "", fmt.Errorf("websocket text is not valid UTF-8")
	}
	return true, string(s.payload), nil
}

func readWebSocketFrame(r io.Reader) (websocketFrame, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return websocketFrame{}, err
	}
	frame := websocketFrame{
		fin:    header[0]&0x80 != 0,
		opcode: header[0] & 0x0f,
	}
	if header[0]&0x70 != 0 {
		return websocketFrame{}, fmt.Errorf("websocket RSV bits set without negotiated extension")
	}
	if !validWebSocketOpcode(frame.opcode) {
		return websocketFrame{}, fmt.Errorf("websocket reserved opcode %d", frame.opcode)
	}
	length, masked, err := readWebSocketLength(r, header[1])
	if err != nil {
		return websocketFrame{}, err
	}
	frame.masked = masked
	if masked {
		return websocketFrame{}, fmt.Errorf("websocket server frame must not be masked")
	}
	if err := validateWebSocketFrameHeader(frame, length); err != nil {
		return websocketFrame{}, err
	}
	if length > maxWebSocketMessageBytes {
		return websocketFrame{}, fmt.Errorf("websocket frame too large")
	}
	frame.payload = make([]byte, length)
	if _, err := io.ReadFull(r, frame.payload); err != nil {
		return websocketFrame{}, err
	}
	return frame, nil
}

func validWebSocketOpcode(opcode byte) bool {
	switch opcode {
	case websocketOpcodeContinuation, websocketOpcodeText, websocketOpcodeBinary, websocketOpcodeClose, websocketOpcodePing, websocketOpcodePong:
		return true
	default:
		return false
	}
}

func validateWebSocketFrameHeader(frame websocketFrame, length int) error {
	if frame.opcode >= websocketOpcodeClose {
		if !frame.fin {
			return fmt.Errorf("websocket control frame fragmented")
		}
		if length > 125 {
			return fmt.Errorf("websocket control frame too large")
		}
	}
	return nil
}

func readWebSocketLength(r io.Reader, b byte) (int, bool, error) {
	masked := b&0x80 != 0
	length := int(b & 0x7f)
	if length == 126 {
		var buf [2]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, false, err
		}
		length = int(binary.BigEndian.Uint16(buf[:]))
		if length < 126 {
			return 0, false, fmt.Errorf("websocket non-minimal 16-bit length")
		}
	}
	if length == 127 {
		var buf [8]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, false, err
		}
		if buf[0]&0x80 != 0 {
			return 0, false, fmt.Errorf("websocket 64-bit length has high bit set")
		}
		size := binary.BigEndian.Uint64(buf[:])
		if size <= 0xffff {
			return 0, false, fmt.Errorf("websocket non-minimal 64-bit length")
		}
		if size > uint64(^uint(0)>>1) {
			return 0, false, fmt.Errorf("websocket frame too large")
		}
		length = int(size)
	}
	return length, masked, nil
}
