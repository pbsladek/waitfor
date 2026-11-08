package condition

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGRPCHTTP2Transport(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", useTLS), func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Errorf("HTTP version = %s, want HTTP/2", r.Proto)
				}
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("Trailer", "Grpc-Status")
				_, _ = w.Write(grpcFrame([]byte{0x08, 0x01}))
				w.Header().Set("Grpc-Status", "0")
			}))
			server.EnableHTTP2 = true
			server.Config.Protocols = &http.Protocols{}
			server.Config.Protocols.SetHTTP2(useTLS)
			server.Config.Protocols.SetUnencryptedHTTP2(!useTLS)
			if useTLS {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)

			cond := NewGRPC(server.URL)
			if useTLS {
				if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
					t.Fatalf("untrusted TLS result = %+v, want unsatisfied", result)
				}
			}
			client := grpcHTTPClient(0, useTLS)
			t.Cleanup(client.CloseIdleConnections)
			transport := client.Transport.(*http.Transport)
			roots := x509.NewCertPool()
			if useTLS {
				roots.AddCert(server.Certificate())
			}
			transport.TLSClientConfig.RootCAs = roots
			cond.Client = client
			if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
				t.Fatalf("HTTP/2 health result = %+v, want satisfied", result)
			}
		})
	}
}

func TestGRPCConditionCheckUsesHealthRequest(t *testing.T) {
	var seenPath, seenContentType string
	var seenBody []byte
	cond := NewGRPC("127.0.0.1:50051")
	cond.Service = "svc"
	cond.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seenPath = req.URL.Path
		seenContentType = req.Header.Get("Content-Type")
		var err error
		seenBody, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		return grpcTestResponse(1), nil
	})}

	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("grpc status = %s, err = %v", result.Status, result.Err)
	}
	if seenPath != grpcHealthPath || seenContentType != "application/grpc" {
		t.Fatalf("request path/content-type = %q/%q", seenPath, seenContentType)
	}
	if !bytes.Equal(seenBody, grpcFrame(encodeGRPCHealthRequest("svc"))) {
		t.Fatalf("grpc body = %v", seenBody)
	}
	cond.Status = GRPCStatusNotServing
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("grpc mismatch status = %s, want unsatisfied", result.Status)
	}
	failing := NewGRPC("127.0.0.1:50051")
	failing.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	if result := failing.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("grpc transport error status = %s, want unsatisfied", result.Status)
	}
}

func TestGRPCDescriptor(t *testing.T) {
	desc := NewGRPC("127.0.0.1:50051").Descriptor()
	if desc.Backend != "grpc" || desc.Target != "127.0.0.1:50051" {
		t.Fatalf("descriptor = %+v", desc)
	}
}

func TestGRPCConditionReflectionCheck(t *testing.T) {
	var paths []string
	var reflectionBody []byte
	cond := NewGRPC("127.0.0.1:50051")
	cond.Method = "/custom.Service/Ready"
	cond.Reflect = true
	cond.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if req.URL.Path == grpcReflectionPath {
			reflectionBody = body
			return grpcTestPayloadResponse(appendProtoString(nil, 4, "descriptor")), nil
		}
		return grpcTestResponse(1), nil
	})}

	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("grpc status = %s, err = %v", result.Status, result.Err)
	}
	if strings.Join(paths, ",") != grpcReflectionPath+",/custom.Service/Ready" {
		t.Fatalf("paths = %v", paths)
	}
	if !bytes.Equal(reflectionBody, grpcFrame(encodeGRPCReflectionRequest("custom.Service"))) {
		t.Fatalf("reflection body = %v", reflectionBody)
	}
}

func TestGRPCHealthEncodingAndDecoding(t *testing.T) {
	payload := encodeGRPCHealthRequest("svc")
	if !bytes.Equal(payload, []byte{0x0a, 0x03, 's', 'v', 'c'}) {
		t.Fatalf("payload = %v", payload)
	}
	defaultSymbol := grpcReflectionSymbol(NewGRPC("127.0.0.1:1"))
	if defaultSymbol != "grpc.health.v1.Health" {
		t.Fatalf("default reflection symbol = %q", defaultSymbol)
	}
	serviceSymbol := NewGRPC("127.0.0.1:1")
	serviceSymbol.Service = "custom.Service"
	if got := grpcReflectionSymbol(serviceSymbol); got != "custom.Service" {
		t.Fatalf("service reflection symbol = %q", got)
	}
	if got := grpcServiceFromMethod("/custom.Service/Ready"); got != "custom.Service" {
		t.Fatalf("method service = %q", got)
	}
	body := grpcFrame([]byte{0x08, 0x01})
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(body)},
		Header:     http.Header{"Content-Type": []string{"application/grpc"}},
		Trailer:    http.Header{"Grpc-Status": []string{"0"}},
	}
	status, err := parseGRPCHealthResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if status != GRPCStatusServing {
		t.Fatalf("status = %s, want SERVING", status)
	}
}

func TestGRPCHealthURLAndErrorBranches(t *testing.T) {
	tests := map[string]string{
		"grpc://127.0.0.1:1":  "http://127.0.0.1:1/grpc.health.v1.Health/Check",
		"grpcs://example:443": "https://example:443/grpc.health.v1.Health/Check",
		"127.0.0.1:50051":     "http://127.0.0.1:50051/grpc.health.v1.Health/Check",
	}
	for input, want := range tests {
		got, err := grpcHealthURL(input, "", false)
		if err != nil {
			t.Fatalf("grpcHealthURL(%q) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("grpcHealthURL(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := grpcHealthURL("missing-port", "", false); err == nil {
		t.Fatal("missing grpc port succeeded")
	}
	if got, err := grpcHealthURL("example.com:443", "", true); err != nil || got != "https://example.com:443/grpc.health.v1.Health/Check" {
		t.Fatalf("forced TLS url = %q err=%v", got, err)
	}
	if _, err := parseGRPCFrame([]byte{0}); err == nil {
		t.Fatal("short grpc frame succeeded")
	}
	if _, err := parseGRPCFrame([]byte{1, 0, 0, 0, 0}); err == nil {
		t.Fatal("compressed grpc frame succeeded")
	}
	if status, err := decodeGRPCHealthStatus([]byte{0x08, 0x02}); err != nil || status != GRPCStatusNotServing {
		t.Fatalf("decode status = %s, err = %v", status, err)
	}
	if status, err := decodeGRPCHealthStatus([]byte{0x08, 0x03}); err != nil || status != GRPCStatusServiceUnknown {
		t.Fatalf("decode service unknown = %s, err = %v", status, err)
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x08, 0x7f}); err == nil {
		t.Fatal("unsupported grpc status succeeded")
	}
	nonOK := &http.Response{StatusCode: http.StatusServiceUnavailable, Body: ioNopCloser{bytes.NewReader(nil)}}
	if _, err := parseGRPCHealthResponse(nonOK); err == nil {
		t.Fatal("non-200 grpc response succeeded")
	}
	trailerErr := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header:     http.Header{"Content-Type": []string{"application/grpc+proto"}},
		Trailer:    http.Header{"Grpc-Status": []string{"14"}},
	}
	if _, err := parseGRPCHealthResponse(trailerErr); err == nil {
		t.Fatal("grpc trailer error succeeded")
	}
	badContentType := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	if _, err := parseGRPCHealthResponse(badContentType); err == nil {
		t.Fatal("grpc non-grpc content-type succeeded")
	}
	if _, err := grpcHealthURL("httpx://example.com", "", false); err == nil {
		t.Fatal("invalid grpc URL scheme succeeded")
	}
	if _, err := grpcHealthURL("https://example.com/base?debug=true#frag", "", false); err == nil {
		t.Fatal("grpc query/fragment URL succeeded")
	}
	if _, err := grpcHealthURL("https://user@example.com/base", "", false); err == nil {
		t.Fatal("grpc userinfo URL succeeded")
	}
	if got, err := grpcHealthURL("https://example.com/base", "", false); err != nil || got != "https://example.com/base/grpc.health.v1.Health/Check" {
		t.Fatalf("grpc HTTP URL = %q err=%v", got, err)
	}
	if got, err := grpcHealthURL("https://example.com/base", "/custom.Service/Ready", false); err != nil || got != "https://example.com/base/custom.Service/Ready" {
		t.Fatalf("grpc custom method URL = %q err=%v", got, err)
	}
	headerStatus := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header: http.Header{
			"Content-Type": []string{"application/grpc"},
			"Grpc-Status":  []string{"7"},
		},
	}
	if _, err := parseGRPCHealthResponse(headerStatus); err == nil {
		t.Fatal("grpc header status error succeeded")
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x12, 0x01, 'x', 0x08, 0x01}); err != nil {
		t.Fatalf("decode with unknown field failed: %v", err)
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x08, 0x80}); err == nil {
		t.Fatal("malformed grpc status varint succeeded")
	}
	if _, err := decodeGRPCHealthStatus(append([]byte{0x08}, bytes.Repeat([]byte{0x81}, 11)...)); err == nil {
		t.Fatal("overlong grpc status varint succeeded")
	}
	missingStatus := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header:     http.Header{"Content-Type": []string{"application/grpc"}},
	}
	if _, err := parseGRPCHealthResponse(missingStatus); err == nil {
		t.Fatal("grpc missing status succeeded")
	}
	nilBody := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/grpc"}, "Grpc-Status": []string{"0"}}}
	if _, err := parseGRPCHealthResponse(nilBody); err == nil {
		t.Fatal("grpc nil body succeeded")
	}
}

func TestGRPCValidationAndProtoSkipBranches(t *testing.T) {
	if validateGRPCConfig(&GRPCCondition{}) == nil {
		t.Fatal("missing grpc address succeeded")
	}
	badStatus := NewGRPC("127.0.0.1:1")
	badStatus.Status = "READY"
	if validateGRPCConfig(badStatus) == nil {
		t.Fatal("bad grpc status succeeded")
	}
	badService := NewGRPC("127.0.0.1:1")
	badService.Service = string([]byte{0xff})
	if utf8.ValidString(badService.Service) || validateGRPCConfig(badService) == nil {
		t.Fatal("invalid UTF-8 grpc service succeeded")
	}
	longService := NewGRPC("127.0.0.1:1")
	longService.Service = strings.Repeat("x", maxGRPCServiceNameBytes+1)
	if validateGRPCConfig(longService) == nil {
		t.Fatal("oversized grpc service succeeded")
	}
	tlsCleartext := NewGRPC("grpc://127.0.0.1:1")
	tlsCleartext.UseTLS = true
	if validateGRPCConfig(tlsCleartext) == nil {
		t.Fatal("grpc --tls with cleartext scheme succeeded")
	}
	badTimeout := NewGRPC("127.0.0.1:1")
	badTimeout.AttemptTimeout = -time.Second
	if validateGRPCConfig(badTimeout) == nil {
		t.Fatal("negative grpc timeout succeeded")
	}
	badMethod := NewGRPC("127.0.0.1:1")
	badMethod.Method = "Service/Check"
	if validateGRPCConfig(badMethod) == nil {
		t.Fatal("relative grpc method succeeded")
	}
	queryMethod := NewGRPC("127.0.0.1:1")
	queryMethod.Method = "/Service/Check?debug=true"
	if validateGRPCConfig(queryMethod) == nil {
		t.Fatal("grpc method with query succeeded")
	}

	payload := []byte{
		0x08, 0x05,
	}
	if _, err := decodeGRPCHealthStatus(payload); err == nil {
		t.Fatal("unsupported grpc health status succeeded")
	}
	withUnknowns := []byte{
		0x11, 1, 2, 3, 4, 5, 6, 7, 8,
		0x1a, 0x01, 'x',
		0x25, 1, 2, 3, 4,
		0x08, 0x01,
	}
	if status, err := decodeGRPCHealthStatus(withUnknowns); err != nil || status != GRPCStatusServing {
		t.Fatalf("decode with skipped fields status=%s err=%v", status, err)
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x0b}); err == nil {
		t.Fatal("unsupported protobuf wire type succeeded")
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x11, 1}); err == nil {
		t.Fatal("truncated fixed64 field succeeded")
	}
	if err := parseGRPCReflectionResponse(grpcTestPayloadResponse(appendProtoString(nil, 4, "descriptor")), "svc"); err != nil {
		t.Fatalf("reflection descriptor failed: %v", err)
	}
	reflectionErr := appendProtoString(nil, 2, "missing")
	if err := parseGRPCReflectionResponse(grpcTestPayloadResponse(appendProtoString(nil, 7, string(reflectionErr))), "svc"); err == nil {
		t.Fatal("reflection error response succeeded")
	}
	if err := parseGRPCReflectionResponse(grpcTestPayloadResponse(nil), "svc"); err == nil {
		t.Fatal("reflection response without descriptor succeeded")
	}
	if _, _, err := decodeGRPCReflectionResponse([]byte{0x80}); err == nil {
		t.Fatal("malformed reflection field key succeeded")
	}
	if _, _, err := decodeGRPCReflectionResponse(appendProtoString(nil, 7, string([]byte{0x80}))); err == nil {
		t.Fatal("malformed reflection error field succeeded")
	}
	if message, err := grpcReflectionErrorMessage(appendProtoString(nil, 1, "ignored")); err != nil || message != "symbol not found" {
		t.Fatalf("reflection fallback message = %q err=%v", message, err)
	}
}

func TestGRPCAdditionalBranches(t *testing.T) {
	badConfigs := []*GRPCCondition{
		NewGRPC(" "),
		NewGRPC("127.0.0.1:50051"),
		NewGRPC("127.0.0.1:50051"),
		NewGRPC("grpc://127.0.0.1:50051"),
		NewGRPC("127.0.0.1:50051"),
		NewGRPC("127.0.0.1:50051"),
		NewGRPC("127.0.0.1:50051"),
	}
	badConfigs[1].Status = "BROKEN"
	badConfigs[2].AttemptTimeout = -time.Second
	badConfigs[3].UseTLS = true
	badConfigs[4].Service = string([]byte{0xff})
	badConfigs[5].Service = strings.Repeat("x", maxGRPCServiceNameBytes+1)
	badConfigs[6].Method = "Service/Method"
	for i, cond := range badConfigs {
		if err := validateGRPCConfig(cond); err == nil {
			t.Fatalf("bad grpc config %d succeeded", i)
		}
	}
	if got := grpcServiceFromMethod("/pkg.Service"); got != "pkg.Service" {
		t.Fatalf("service from method = %q", got)
	}
	if got := grpcMethodPath(""); got != grpcHealthPath {
		t.Fatalf("default grpc method = %q", got)
	}
	if got, err := grpcHealthURL("127.0.0.1:50051", "/svc.Method/Check", true); err != nil || got != "https://127.0.0.1:50051/svc.Method/Check" {
		t.Fatalf("grpcHealthURL tls = %q, %v", got, err)
	}
	for _, address := range []string{"ftp://example.test", "http://example.test/path?query=1", "grpc://user@example.test:443"} {
		if _, err := grpcHealthURL(address, "", false); err == nil {
			t.Fatalf("grpcHealthURL(%q) succeeded", address)
		}
	}
	if _, err := grpcHealthRequest(t.Context(), "bad-address", "", "", false); err == nil {
		t.Fatal("grpc health request with bad address succeeded")
	}
	if _, err := grpcReflectionRequest(t.Context(), "bad-address", "svc", false); err == nil {
		t.Fatal("grpc reflection request with bad address succeeded")
	}
}

func TestGRPCPayloadAndProtoErrorBranches(t *testing.T) {
	errBody := &errorReadCloser{}
	if _, err := parseGRPCPayloadResponse(&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/grpc"}, "Grpc-Status": []string{"0"}}, Body: errBody}); err == nil {
		t.Fatal("grpc read error succeeded")
	}
	tooLarge := io.NopCloser(strings.NewReader(strings.Repeat("x", maxGRPCResponseBytes+1)))
	if _, err := parseGRPCPayloadResponse(&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/grpc"}, "Grpc-Status": []string{"0"}}, Body: tooLarge}); err == nil {
		t.Fatal("oversized grpc response succeeded")
	}
	if _, err := parseGRPCFrame(append(grpcFrame([]byte("ok")), 'x')); err == nil {
		t.Fatal("grpc frame with extra bytes succeeded")
	}
	if _, err := parseGRPCFrame([]byte{0, 0, 0, 0, 3, 'o'}); err == nil {
		t.Fatal("truncated grpc frame succeeded")
	}
	if _, err := decodeGRPCHealthStatus(nil); err == nil {
		t.Fatal("missing grpc health status succeeded")
	}
	if status, err := decodeGRPCHealthStatus([]byte{0x08, 0x00}); err != nil || status != GRPCStatusUnknown {
		t.Fatalf("unknown health status = %s, err = %v", status, err)
	}
	if _, _, err := decodeGRPCReflectionResponse([]byte{0x08}); err == nil {
		t.Fatal("malformed grpc reflection key succeeded")
	}
	if _, _, err := decodeGRPCReflectionResponse([]byte{0x08, 0x01}); err != nil {
		t.Fatalf("reflection skip varint error = %v", err)
	}
	if err := parseGRPCReflectionResponse(grpcTestPayloadResponse(nil), "svc"); err == nil {
		t.Fatal("reflection missing descriptor succeeded")
	}
	payload := append([]byte{0x3a}, appendProtoString(nil, 2, "not found")...)
	if err := parseGRPCReflectionResponse(grpcTestPayloadResponse(payload), "svc"); err == nil {
		t.Fatal("reflection error payload succeeded")
	}
	if _, _, err := grpcReflectionError([]byte{0x80}); err == nil {
		t.Fatal("malformed reflection error succeeded")
	}
	if _, _, err := protoStringField([]byte{0x02, 'x'}); err == nil {
		t.Fatal("malformed proto string succeeded")
	}
	if _, err := grpcReflectionErrorMessage([]byte{0x08}); err == nil {
		t.Fatal("malformed reflection error field succeeded")
	}
}

type ioNopCloser struct {
	*bytes.Reader
}

func (c ioNopCloser) Close() error {
	return nil
}

func TestAppendVarintMultiByte(t *testing.T) {
	out := appendVarint(nil, 300)
	got, n := readVarint(out)
	if got != 300 || n != len(out) {
		t.Fatalf("varint got=%d n=%d bytes=%v", got, n, out)
	}
}

func TestGRPCUint32LengthClamps(t *testing.T) {
	if uint32Length(-1) != 0 {
		t.Fatal("negative length did not clamp")
	}
	if uint32Length(1<<33) != ^uint32(0) {
		t.Fatal("large length did not clamp")
	}
}

func grpcTestResponse(status byte) *http.Response {
	return grpcTestPayloadResponse([]byte{0x08, status})
}

func grpcTestPayloadResponse(payload []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(grpcFrame(payload))),
		Header:     http.Header{"Content-Type": []string{"application/grpc"}},
		Trailer:    http.Header{"Grpc-Status": []string{"0"}},
	}
}

type errorReadCloser struct{}

func (*errorReadCloser) Read([]byte) (int, error) {
	return 0, fmt.Errorf("read failed")
}

func (*errorReadCloser) Close() error {
	return nil
}
