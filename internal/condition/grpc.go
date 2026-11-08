package condition

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxGRPCResponseBytes    = 4 * 1024 * 1024
	maxGRPCServiceNameBytes = 1024
	grpcHealthPath          = "/grpc.health.v1.Health/Check"
	grpcReflectionPath      = "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
)

type GRPCServingStatus string

const (
	GRPCStatusServing        GRPCServingStatus = "SERVING"
	GRPCStatusNotServing     GRPCServingStatus = "NOT_SERVING"
	GRPCStatusUnknown        GRPCServingStatus = "UNKNOWN"
	GRPCStatusServiceUnknown GRPCServingStatus = "SERVICE_UNKNOWN"
)

type GRPCCondition struct {
	Address        string
	Service        string
	Method         string
	Reflect        bool
	Status         GRPCServingStatus
	UseTLS         bool
	AttemptTimeout time.Duration
	Client         *http.Client
}

func NewGRPC(address string) *GRPCCondition {
	return &GRPCCondition{Address: address, Status: GRPCStatusServing, AttemptTimeout: 2 * time.Second}
}

func (c *GRPCCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "grpc", Target: c.Address}
}

func (c *GRPCCondition) Check(ctx context.Context) Result {
	if err := validateGRPCConfig(c); err != nil {
		return Fatal(err)
	}
	status, err := c.check(ctx)
	if err != nil {
		return Unsatisfied("", err)
	}
	if status != c.Status {
		detail := fmt.Sprintf("grpc health status %s, expected %s", status, c.Status)
		return Unsatisfied(detail, fmt.Errorf("%s", detail))
	}
	return Satisfied("grpc health status " + string(status))
}

func validateGRPCConfig(c *GRPCCondition) error {
	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("grpc address is required")
	}
	if err := validateGRPCStatus(c.Status); err != nil {
		return err
	}
	if err := validateGRPCTiming(c.AttemptTimeout); err != nil {
		return err
	}
	if err := validateGRPCTransport(c.Address, c.UseTLS); err != nil {
		return err
	}
	if err := validateGRPCService(c.Service); err != nil {
		return err
	}
	return validateGRPCMethod(c.Method)
}

func validateGRPCStatus(status GRPCServingStatus) error {
	switch status {
	case GRPCStatusServing, GRPCStatusNotServing, GRPCStatusUnknown, GRPCStatusServiceUnknown:
		return nil
	default:
		return fmt.Errorf("unsupported grpc health status %q", status)
	}
}

func validateGRPCTiming(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	return nil
}

func validateGRPCTransport(address string, useTLS bool) error {
	if useTLS && grpcAddressIsCleartext(address) {
		return fmt.Errorf("grpc --tls cannot be used with a cleartext URL scheme")
	}
	return nil
}

func validateGRPCService(service string) error {
	if !utf8.ValidString(service) {
		return fmt.Errorf("grpc service name must be valid UTF-8")
	}
	if len(service) > maxGRPCServiceNameBytes {
		return fmt.Errorf("grpc service name is too long")
	}
	return nil
}

func validateGRPCMethod(method string) error {
	if method == "" {
		return nil
	}
	if !strings.HasPrefix(method, "/") || strings.ContainsAny(method, "?#") {
		return fmt.Errorf("grpc method must be an absolute /Service/Method path")
	}
	return nil
}

func (c *GRPCCondition) check(ctx context.Context) (GRPCServingStatus, error) {
	client := c.Client
	if client == nil {
		client = grpcHTTPClient(c.AttemptTimeout, grpcAddressUsesTLS(c.Address, c.UseTLS))
	}
	if c.Reflect {
		if err := c.checkReflection(ctx, client); err != nil {
			return "", err
		}
	}
	req, err := grpcHealthRequest(ctx, c.Address, c.Service, c.Method, c.UseTLS)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return parseGRPCHealthResponse(resp)
}

func (c *GRPCCondition) checkReflection(ctx context.Context, client *http.Client) error {
	symbol := grpcReflectionSymbol(c)
	req, err := grpcReflectionRequest(ctx, c.Address, symbol, c.UseTLS)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return parseGRPCReflectionResponse(resp, symbol)
}

func grpcReflectionSymbol(c *GRPCCondition) string {
	if c.Method != "" {
		return grpcServiceFromMethod(c.Method)
	}
	if c.Service != "" {
		return c.Service
	}
	return "grpc.health.v1.Health"
}

func grpcServiceFromMethod(method string) string {
	method = strings.TrimPrefix(method, "/")
	service, _, ok := strings.Cut(method, "/")
	if !ok {
		return method
	}
	return service
}

func grpcHTTPClient(timeout time.Duration, useTLS bool) *http.Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	protocols := &http.Protocols{}
	protocols.SetHTTP2(useTLS)
	protocols.SetUnencryptedHTTP2(!useTLS)
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Protocols:           protocols,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: timeout,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func grpcAddressUsesTLS(address string, useTLS bool) bool {
	return useTLS || strings.HasPrefix(address, "grpcs://") || strings.HasPrefix(address, "https://")
}

func grpcAddressIsCleartext(address string) bool {
	return strings.HasPrefix(address, "grpc://") || strings.HasPrefix(address, "http://")
}

func grpcHealthRequest(ctx context.Context, address, service, method string, useTLS bool) (*http.Request, error) {
	endpoint, err := grpcHealthURL(address, method, useTLS)
	if err != nil {
		return nil, err
	}
	body := grpcFrame(encodeGRPCHealthRequest(service))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	return req, nil
}

func grpcReflectionRequest(ctx context.Context, address, symbol string, useTLS bool) (*http.Request, error) {
	endpoint, err := grpcHealthURL(address, grpcReflectionPath, useTLS)
	if err != nil {
		return nil, err
	}
	body := grpcFrame(encodeGRPCReflectionRequest(symbol))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	return req, nil
}

func grpcHealthURL(address string, method string, useTLS bool) (string, error) {
	if strings.HasPrefix(address, "grpc://") {
		return grpcSchemeURL(address, "grpc", "http", method)
	}
	if strings.HasPrefix(address, "grpcs://") {
		return grpcSchemeURL(address, "grpcs", "https", method)
	}
	if urlHasHTTPScheme(address) {
		return grpcHTTPURL(address, method)
	}
	if strings.Contains(address, "://") {
		return "", fmt.Errorf("invalid grpc address %q", address)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return "", fmt.Errorf("invalid grpc address %q: %w", address, err)
	}
	if useTLS {
		return "https://" + address + grpcMethodPath(method), nil
	}
	return "http://" + address + grpcMethodPath(method), nil
}

func urlHasHTTPScheme(address string) bool {
	parsed, err := url.ParseRequestURI(address)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func grpcHTTPURL(address string, method string) (string, error) {
	parsed, err := url.ParseRequestURI(address)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid grpc address %q", address)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("grpc address cannot include userinfo, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + grpcMethodPath(method)
	return parsed.String(), nil
}

func grpcSchemeURL(address, sourceScheme, targetScheme string, method string) (string, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != sourceScheme || parsed.Host == "" {
		return "", fmt.Errorf("invalid grpc address %q", address)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("grpc address cannot include userinfo, query, or fragment")
	}
	parsed.Scheme = targetScheme
	parsed.Path = strings.TrimRight(parsed.Path, "/") + grpcMethodPath(method)
	return parsed.String(), nil
}

func grpcMethodPath(method string) string {
	if method == "" {
		return grpcHealthPath
	}
	return method
}

func encodeGRPCHealthRequest(service string) []byte {
	if service == "" {
		return nil
	}
	payload := []byte(service)
	out := []byte{0x0a}
	out = appendVarint(out, uint64(len(payload)))
	return append(out, payload...)
}

func encodeGRPCReflectionRequest(symbol string) []byte {
	out := appendProtoString(nil, 4, symbol)
	return out
}

func appendProtoString(out []byte, fieldNumber uint64, value string) []byte {
	out = appendVarint(out, fieldNumber<<3|2)
	out = appendVarint(out, uint64(len(value)))
	return append(out, value...)
}

func grpcFrame(payload []byte) []byte {
	frame := make([]byte, 5, len(payload)+5)
	binary.BigEndian.PutUint32(frame[1:5], uint32Length(len(payload)))
	return append(frame, payload...)
}

func uint32Length(length int) uint32 {
	if length < 0 {
		return 0
	}
	if length > int(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(length)
}

func parseGRPCReflectionResponse(resp *http.Response, symbol string) error {
	payload, err := parseGRPCPayloadResponse(resp)
	if err != nil {
		return err
	}
	ok, reflectionErr, err := decodeGRPCReflectionResponse(payload)
	if err != nil {
		return err
	}
	if reflectionErr != "" {
		return fmt.Errorf("grpc reflection %s: %s", symbol, reflectionErr)
	}
	if !ok {
		return fmt.Errorf("grpc reflection response missing descriptor for %s", symbol)
	}
	return nil
}

func parseGRPCHealthResponse(resp *http.Response) (GRPCServingStatus, error) {
	payload, err := parseGRPCPayloadResponse(resp)
	if err != nil {
		return "", err
	}
	return decodeGRPCHealthStatus(payload)
}

func parseGRPCPayloadResponse(resp *http.Response) ([]byte, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grpc HTTP status %d", resp.StatusCode)
	}
	if !validGRPCContentType(resp.Header.Get("Content-Type")) {
		return nil, fmt.Errorf("grpc response content-type is not application/grpc")
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("grpc response body is missing")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGRPCResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxGRPCResponseBytes {
		return nil, fmt.Errorf("grpc response too large")
	}
	status := grpcStatus(resp)
	if status == "" {
		return nil, fmt.Errorf("grpc status missing")
	}
	if status != "0" {
		return nil, fmt.Errorf("grpc status %s", status)
	}
	return parseGRPCFrame(body)
}

func validGRPCContentType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	return value == "application/grpc" || strings.HasPrefix(value, "application/grpc+")
}

func grpcStatus(resp *http.Response) string {
	if status := resp.Trailer.Get("Grpc-Status"); status != "" {
		return status
	}
	return resp.Header.Get("Grpc-Status")
}

func parseGRPCFrame(body []byte) ([]byte, error) {
	if len(body) < 5 {
		return nil, fmt.Errorf("short grpc response")
	}
	if body[0] != 0 {
		return nil, fmt.Errorf("compressed grpc responses are not supported")
	}
	size := binary.BigEndian.Uint32(body[1:5])
	if int(size) > len(body)-5 {
		return nil, fmt.Errorf("truncated grpc response")
	}
	if int(size) != len(body)-5 {
		return nil, fmt.Errorf("unexpected extra grpc response bytes")
	}
	return body[5 : 5+size], nil
}

func decodeGRPCHealthStatus(payload []byte) (GRPCServingStatus, error) {
	value, ok, err := findGRPCHealthStatus(payload)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("grpc health response missing status")
	}
	switch value {
	case 0:
		return GRPCStatusUnknown, nil
	case 1:
		return GRPCStatusServing, nil
	case 2:
		return GRPCStatusNotServing, nil
	case 3:
		return GRPCStatusServiceUnknown, nil
	default:
		return "", fmt.Errorf("unsupported grpc health status code %d", value)
	}
}

func decodeGRPCReflectionResponse(payload []byte) (bool, string, error) {
	for len(payload) > 0 {
		key, n := readVarint(payload)
		if n == 0 {
			return false, "", fmt.Errorf("malformed grpc reflection field key")
		}
		payload = payload[n:]
		fieldNumber := key >> 3
		wireType := key & 0x7
		if fieldNumber == 4 && wireType == 2 {
			return true, "", nil
		}
		if fieldNumber == 7 && wireType == 2 {
			message, _, err := grpcReflectionError(payload)
			return false, message, err
		}
		rest, err := skipProtoField(payload, wireType)
		if err != nil {
			return false, "", err
		}
		payload = rest
	}
	return false, "", nil
}

func grpcReflectionError(payload []byte) (string, []byte, error) {
	messagePayload, rest, err := protoMessagePayload(payload)
	if err != nil {
		return "", nil, err
	}
	message, err := grpcReflectionErrorMessage(messagePayload)
	return message, rest, err
}

func protoMessagePayload(payload []byte) ([]byte, []byte, error) {
	size, n := readVarint(payload)
	if n == 0 || protoLengthExceedsAvailable(size, len(payload)-n) {
		return nil, nil, fmt.Errorf("malformed grpc reflection error")
	}
	end := n + checkedProtoLength(size)
	return payload[n:end], payload[end:], nil
}

func grpcReflectionErrorMessage(payload []byte) (string, error) {
	for len(payload) > 0 {
		key, n := readVarint(payload)
		if n == 0 {
			return "", fmt.Errorf("malformed grpc reflection error field")
		}
		payload = payload[n:]
		fieldNumber := key >> 3
		wireType := key & 0x7
		if fieldNumber == 2 && wireType == 2 {
			message, _, err := protoStringField(payload)
			if err != nil {
				return "", err
			}
			return message, nil
		}
		rest, err := skipProtoField(payload, wireType)
		if err != nil {
			return "", err
		}
		payload = rest
	}
	return "symbol not found", nil
}

func protoStringField(payload []byte) (string, []byte, error) {
	size, n := readVarint(payload)
	if n == 0 || protoLengthExceedsAvailable(size, len(payload)-n) {
		return "", nil, fmt.Errorf("malformed grpc reflection string")
	}
	end := n + checkedProtoLength(size)
	return string(payload[n:end]), payload[end:], nil
}

func findGRPCHealthStatus(payload []byte) (uint64, bool, error) {
	for len(payload) > 0 {
		key, n := readVarint(payload)
		if n == 0 {
			return 0, false, fmt.Errorf("malformed grpc protobuf field key")
		}
		payload = payload[n:]
		fieldNumber := key >> 3
		wireType := key & 0x7
		if fieldNumber == 1 && wireType == 0 {
			value, n := readVarint(payload)
			if n == 0 {
				return 0, false, fmt.Errorf("malformed grpc health status")
			}
			return value, true, nil
		}
		rest, err := skipProtoField(payload, wireType)
		if err != nil {
			return 0, false, err
		}
		payload = rest
	}
	return 0, false, nil
}

func skipProtoField(payload []byte, wireType uint64) ([]byte, error) {
	switch wireType {
	case 0:
		_, n := readVarint(payload)
		if n == 0 {
			return nil, fmt.Errorf("malformed grpc protobuf varint")
		}
		return payload[n:], nil
	case 1:
		return skipFixed(payload, 8)
	case 2:
		size, n := readVarint(payload)
		if n == 0 || protoLengthExceedsAvailable(size, len(payload)-n) {
			return nil, fmt.Errorf("malformed grpc protobuf length")
		}
		return payload[n+checkedProtoLength(size):], nil
	case 5:
		return skipFixed(payload, 4)
	default:
		return nil, fmt.Errorf("unsupported grpc protobuf wire type %d", wireType)
	}
}

func protoLengthExceedsAvailable(size uint64, available int) bool {
	return size > uint64(available) // #nosec G115 -- available comes from len() and is non-negative.
}

func checkedProtoLength(size uint64) int {
	return int(size) // #nosec G115 -- caller first checks size against the available slice length.
}

func skipFixed(payload []byte, size int) ([]byte, error) {
	if len(payload) < size {
		return nil, fmt.Errorf("truncated grpc protobuf field")
	}
	return payload[size:], nil
}

func appendVarint(out []byte, value uint64) []byte {
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func readVarint(in []byte) (uint64, int) {
	var value uint64
	for i, b := range in {
		if i == 10 || (i == 9 && b > 1) {
			return 0, 0
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1
		}
	}
	return value, 0
}
