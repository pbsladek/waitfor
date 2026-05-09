package condition

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

type TLSCondition struct {
	Address    string
	ServerName string
	ValidFor   time.Duration
	RootCAs    *x509.CertPool
	Dial       func(context.Context, TLSProbeConfig) (TLSCertificateState, error)
	Now        func() time.Time
}

type TLSProbeConfig struct {
	Address    string
	ServerName string
	RootCAs    *x509.CertPool
}

type TLSCertificateState struct {
	Certificates   []*x509.Certificate
	VerifiedChains [][]*x509.Certificate
}

func NewTLS(address string) *TLSCondition {
	return &TLSCondition{Address: address}
}

func (c *TLSCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "tls", Target: c.Address}
}

func (c *TLSCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	if err := validateTLSConfig(c); err != nil {
		return Fatal(err)
	}
	state, err := c.probe(ctx)
	if err != nil {
		return Unsatisfied("", err)
	}
	return checkTLSCertificateState(state, c.serverName(), c.now(), c.ValidFor)
}

func (c *TLSCondition) probe(ctx context.Context) (TLSCertificateState, error) {
	cfg := TLSProbeConfig{Address: c.Address, ServerName: c.serverName(), RootCAs: c.RootCAs}
	if c.Dial != nil {
		return c.Dial(ctx, cfg)
	}
	return defaultTLSProbe(ctx, cfg)
}

func (c *TLSCondition) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *TLSCondition) serverName() string {
	if strings.TrimSpace(c.ServerName) != "" {
		return strings.TrimSpace(c.ServerName)
	}
	host, _, err := net.SplitHostPort(c.Address)
	if err != nil {
		return ""
	}
	return strings.Trim(host, "[]")
}

func validateTLSConfig(c *TLSCondition) error {
	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("tls address is required")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return fmt.Errorf("invalid tls address %q: %w", c.Address, err)
	}
	if c.serverName() == "" {
		return fmt.Errorf("tls server name is required")
	}
	if c.ValidFor < 0 {
		return fmt.Errorf("tls valid-for cannot be negative")
	}
	return nil
}

func defaultTLSProbe(ctx context.Context, cfg TLSProbeConfig) (TLSCertificateState, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{},
		Config: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    cfg.RootCAs,
			ServerName: cfg.ServerName,
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return TLSCertificateState{}, err
	}
	defer func() { _ = conn.Close() }()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return TLSCertificateState{}, fmt.Errorf("tls handshake did not return a TLS connection")
	}
	state := tlsConn.ConnectionState()
	return TLSCertificateState{Certificates: state.PeerCertificates, VerifiedChains: state.VerifiedChains}, nil
}

func checkTLSCertificateState(state TLSCertificateState, serverName string, now time.Time, validFor time.Duration) Result {
	if len(state.Certificates) == 0 {
		return Unsatisfied("no peer certificate", fmt.Errorf("tls server did not present a certificate"))
	}
	leaf := state.Certificates[0]
	if err := checkTLSChain(state); err != nil {
		return Unsatisfied("certificate chain not verified", err)
	}
	if err := leaf.VerifyHostname(serverName); err != nil {
		return Unsatisfied("certificate SAN does not match "+serverName, err)
	}
	if result := checkTLSValidityWindow(leaf, now, validFor); result != nil {
		return *result
	}
	return Satisfied(tlsCertificateDetail(leaf, now))
}

func checkTLSChain(state TLSCertificateState) error {
	if len(state.VerifiedChains) == 0 {
		return errors.New("certificate chain was not verified")
	}
	return nil
}

func checkTLSValidityWindow(cert *x509.Certificate, now time.Time, validFor time.Duration) *Result {
	if now.Before(cert.NotBefore) {
		detail := "certificate is not valid yet"
		r := Unsatisfied(detail, errors.New(detail))
		return &r
	}
	if now.After(cert.NotAfter) {
		detail := "certificate is expired"
		r := Unsatisfied(detail, errors.New(detail))
		return &r
	}
	if validFor > 0 && now.Add(validFor).After(cert.NotAfter) {
		detail := fmt.Sprintf("certificate expires before %s", validFor)
		r := Unsatisfied(detail, errors.New(detail))
		return &r
	}
	return nil
}

func tlsCertificateDetail(cert *x509.Certificate, now time.Time) string {
	remaining := cert.NotAfter.Sub(now).Round(time.Second)
	return fmt.Sprintf("certificate valid for %s until %s", remaining, cert.NotAfter.Format(time.RFC3339))
}
