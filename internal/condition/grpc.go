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

	"golang.org/x/net/http2"
)

const (
	maxGRPCResponseBytes    = 4 * 1024 * 1024
	maxGRPCServiceNameBytes = 1024
	grpcHealthPath          = "/grpc.health.v1.Health/Check"
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
	switch c.Status {
	case GRPCStatusServing, GRPCStatusNotServing, GRPCStatusUnknown, GRPCStatusServiceUnknown:
	default:
		return fmt.Errorf("unsupported grpc health status %q", c.Status)
	}
	if c.AttemptTimeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	if c.UseTLS && grpcAddressIsCleartext(c.Address) {
		return fmt.Errorf("grpc --tls cannot be used with a cleartext URL scheme")
	}
	if !utf8.ValidString(c.Service) {
		return fmt.Errorf("grpc service name must be valid UTF-8")
	}
	if len(c.Service) > maxGRPCServiceNameBytes {
		return fmt.Errorf("grpc service name is too long")
	}
	return nil
}

func (c *GRPCCondition) check(ctx context.Context) (GRPCServingStatus, error) {
	client := c.Client
	if client == nil {
		client = grpcHTTPClient(c.AttemptTimeout, grpcAddressUsesTLS(c.Address, c.UseTLS))
	}
	req, err := grpcHealthRequest(ctx, c.Address, c.Service, c.UseTLS)
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

func grpcHTTPClient(timeout time.Duration, useTLS bool) *http.Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			dialer := net.Dialer{Timeout: timeout}
			if !useTLS {
				return dialer.DialContext(ctx, network, addr)
			}
			if cfg == nil {
				cfg = &tls.Config{MinVersion: tls.VersionTLS12}
			}
			tlsDialer := tls.Dialer{NetDialer: &dialer, Config: cfg}
			return tlsDialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func grpcAddressUsesTLS(address string, useTLS bool) bool {
	return useTLS || strings.HasPrefix(address, "grpcs://") || strings.HasPrefix(address, "https://")
}

func grpcAddressIsCleartext(address string) bool {
	return strings.HasPrefix(address, "grpc://") || strings.HasPrefix(address, "http://")
}

func grpcHealthRequest(ctx context.Context, address, service string, useTLS bool) (*http.Request, error) {
	endpoint, err := grpcHealthURL(address, useTLS)
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

func grpcHealthURL(address string, useTLS bool) (string, error) {
	if strings.HasPrefix(address, "grpc://") {
		return grpcSchemeURL(address, "grpc", "http")
	}
	if strings.HasPrefix(address, "grpcs://") {
		return grpcSchemeURL(address, "grpcs", "https")
	}
	if urlHasHTTPScheme(address) {
		return grpcHTTPURL(address)
	}
	if strings.Contains(address, "://") {
		return "", fmt.Errorf("invalid grpc address %q", address)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return "", fmt.Errorf("invalid grpc address %q: %w", address, err)
	}
	if useTLS {
		return "https://" + address + grpcHealthPath, nil
	}
	return "http://" + address + grpcHealthPath, nil
}

func urlHasHTTPScheme(address string) bool {
	parsed, err := url.ParseRequestURI(address)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func grpcHTTPURL(address string) (string, error) {
	parsed, err := url.ParseRequestURI(address)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid grpc address %q", address)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("grpc address cannot include userinfo, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + grpcHealthPath
	return parsed.String(), nil
}

func grpcSchemeURL(address, sourceScheme, targetScheme string) (string, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != sourceScheme || parsed.Host == "" {
		return "", fmt.Errorf("invalid grpc address %q", address)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("grpc address cannot include userinfo, query, or fragment")
	}
	parsed.Scheme = targetScheme
	parsed.Path = strings.TrimRight(parsed.Path, "/") + grpcHealthPath
	return parsed.String(), nil
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

func parseGRPCHealthResponse(resp *http.Response) (GRPCServingStatus, error) {
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("grpc health HTTP status %d", resp.StatusCode)
	}
	if !validGRPCContentType(resp.Header.Get("Content-Type")) {
		return "", fmt.Errorf("grpc response content-type is not application/grpc")
	}
	if resp.Body == nil {
		return "", fmt.Errorf("grpc response body is missing")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGRPCResponseBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxGRPCResponseBytes {
		return "", fmt.Errorf("grpc response too large")
	}
	status := grpcStatus(resp)
	if status == "" {
		return "", fmt.Errorf("grpc status missing")
	}
	if status != "0" {
		return "", fmt.Errorf("grpc status %s", status)
	}
	payload, err := parseGRPCFrame(body)
	if err != nil {
		return "", err
	}
	return decodeGRPCHealthStatus(payload)
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
