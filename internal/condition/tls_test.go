package condition

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTLSConditionSatisfied(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	cert := testTLSCert("api.example.com", now.Add(-time.Hour), now.Add(45*24*time.Hour))
	cond := NewTLS("api.example.com:443")
	cond.ValidFor = 30 * 24 * time.Hour
	cond.Now = func() time.Time { return now }
	cond.Dial = func(_ context.Context, cfg TLSProbeConfig) (TLSCertificateState, error) {
		if cfg.Address != "api.example.com:443" || cfg.ServerName != "api.example.com" {
			t.Fatalf("probe config = %+v", cfg)
		}
		return verifiedTLSState(cert), nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if !strings.Contains(result.Detail, "certificate valid") {
		t.Fatalf("detail = %q, want certificate detail", result.Detail)
	}
}

func TestTLSConditionServerNameOverride(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	cert := testTLSCert("api.internal", now.Add(-time.Hour), now.Add(24*time.Hour))
	cond := NewTLS("127.0.0.1:443")
	cond.ServerName = "api.internal"
	cond.Now = func() time.Time { return now }
	cond.Dial = func(_ context.Context, cfg TLSProbeConfig) (TLSCertificateState, error) {
		if cfg.ServerName != "api.internal" {
			t.Fatalf("server name = %q, want api.internal", cfg.ServerName)
		}
		return verifiedTLSState(cert), nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestTLSConditionUnsatisfiedCases(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		cert     *x509.Certificate
		chains   [][]*x509.Certificate
		validFor time.Duration
		want     string
	}{
		{
			name:   "no certificate",
			chains: [][]*x509.Certificate{{}},
			want:   "no peer certificate",
		},
		{
			name: "chain missing",
			cert: testTLSCert("api.example.com", now.Add(-time.Hour), now.Add(time.Hour)),
			want: "chain not verified",
		},
		{
			name:   "san mismatch",
			cert:   testTLSCert("other.example.com", now.Add(-time.Hour), now.Add(time.Hour)),
			chains: [][]*x509.Certificate{{testTLSCert("other.example.com", now.Add(-time.Hour), now.Add(time.Hour))}},
			want:   "SAN does not match",
		},
		{
			name:   "not valid yet",
			cert:   testTLSCert("api.example.com", now.Add(time.Hour), now.Add(2*time.Hour)),
			chains: [][]*x509.Certificate{{testTLSCert("api.example.com", now.Add(time.Hour), now.Add(2*time.Hour))}},
			want:   "not valid yet",
		},
		{
			name:   "expired",
			cert:   testTLSCert("api.example.com", now.Add(-2*time.Hour), now.Add(-time.Hour)),
			chains: [][]*x509.Certificate{{testTLSCert("api.example.com", now.Add(-2*time.Hour), now.Add(-time.Hour))}},
			want:   "expired",
		},
		{
			name:     "valid for too short",
			cert:     testTLSCert("api.example.com", now.Add(-time.Hour), now.Add(time.Hour)),
			chains:   [][]*x509.Certificate{{testTLSCert("api.example.com", now.Add(-time.Hour), now.Add(time.Hour))}},
			validFor: 2 * time.Hour,
			want:     "expires before",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewTLS("api.example.com:443")
			cond.ValidFor = tt.validFor
			cond.Now = func() time.Time { return now }
			cond.Dial = func(context.Context, TLSProbeConfig) (TLSCertificateState, error) {
				state := TLSCertificateState{VerifiedChains: tt.chains}
				if tt.cert != nil {
					state.Certificates = []*x509.Certificate{tt.cert}
				}
				return state, nil
			}

			result := cond.Check(t.Context())
			if result.Status != CheckUnsatisfied {
				t.Fatalf("status = %s, want unsatisfied", result.Status)
			}
			if !strings.Contains(result.Detail, tt.want) && (result.Err == nil || !strings.Contains(result.Err.Error(), tt.want)) {
				t.Fatalf("detail/err = %q/%v, want %q", result.Detail, result.Err, tt.want)
			}
		})
	}
}

func TestTLSConditionProbeErrorUnsatisfied(t *testing.T) {
	cond := NewTLS("api.example.com:443")
	cond.Dial = func(context.Context, TLSProbeConfig) (TLSCertificateState, error) {
		return TLSCertificateState{}, errors.New("connection refused")
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestDefaultTLSProbe(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	state, err := defaultTLSProbe(t.Context(), TLSProbeConfig{
		Address:    server.Listener.Addr().String(),
		ServerName: "example.com",
		RootCAs:    pool,
	})
	if err != nil {
		t.Fatalf("defaultTLSProbe() error = %v", err)
	}
	if len(state.Certificates) == 0 || len(state.VerifiedChains) == 0 {
		t.Fatalf("state = %+v, want certificate and verified chain", state)
	}
}

func TestTLSConditionContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := NewTLS("api.example.com:443").Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", result.Err)
	}
}

func TestTLSConditionInvalidConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*TLSCondition)
	}{
		{"missing address", func(c *TLSCondition) { c.Address = "" }},
		{"bad address", func(c *TLSCondition) { c.Address = "example.com" }},
		{"negative valid for", func(c *TLSCondition) { c.ValidFor = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewTLS("api.example.com:443")
			tt.setup(cond)
			result := cond.Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
	}
}

func TestTLSDescriptor(t *testing.T) {
	d := NewTLS("api.example.com:443").Descriptor()
	if d.Backend != "tls" || d.Target != "api.example.com:443" {
		t.Fatalf("descriptor = %+v", d)
	}
}

func testTLSCert(dnsName string, notBefore, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{DNSNames: []string{dnsName}, NotBefore: notBefore, NotAfter: notAfter}
}

func verifiedTLSState(cert *x509.Certificate) TLSCertificateState {
	return TLSCertificateState{Certificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
}
