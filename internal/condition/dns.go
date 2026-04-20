package condition

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	wdns "codeberg.org/miekg/dns"
)

type DNSRecordType string

const (
	DNSRecordA     DNSRecordType = "A"
	DNSRecordAAAA  DNSRecordType = "AAAA"
	DNSRecordCNAME DNSRecordType = "CNAME"
	DNSRecordTXT   DNSRecordType = "TXT"
	DNSRecordANY   DNSRecordType = "ANY"
	DNSRecordMX    DNSRecordType = "MX"
	DNSRecordSRV   DNSRecordType = "SRV"
	DNSRecordNS    DNSRecordType = "NS"
	DNSRecordCAA   DNSRecordType = "CAA"
	DNSRecordHTTPS DNSRecordType = "HTTPS"
	DNSRecordSVCB  DNSRecordType = "SVCB"
)

type DNSResolverMode string

const (
	DNSResolverSystem DNSResolverMode = "system"
	DNSResolverWire   DNSResolverMode = "wire"
)

type DNSAbsentMode string

const (
	DNSAbsentAny      DNSAbsentMode = "any"
	DNSAbsentNXDomain DNSAbsentMode = "nxdomain"
	DNSAbsentNODATA   DNSAbsentMode = "nodata"
)

type DNSTransport string

const (
	DNSTransportUDP DNSTransport = "udp"
	DNSTransportTCP DNSTransport = "tcp"
)

type DNSCondition struct {
	Host         string
	RecordType   DNSRecordType
	ResolverMode DNSResolverMode
	Contains     string
	Equals       []string
	MinCount     int
	Absent       bool
	AbsentMode   DNSAbsentMode
	Server       string
	RCode        string
	Transport    DNSTransport
	EDNS0        bool
	UDPSize      uint16
	LookupHost   func(context.Context, string) ([]string, error)
	LookupIP     func(context.Context, string, string) ([]net.IP, error)
	LookupCNAME  func(context.Context, string) (string, error)
	LookupTXT    func(context.Context, string) ([]string, error)
	WireExchange func(context.Context, *wdns.Msg, string, string) (*wdns.Msg, error)
}

type dnsLookupResponse struct {
	Values    []string
	RCode     string
	NXDOMAIN  bool
	NODATA    bool
	Truncated bool
}

var dnsWireTypes = map[DNSRecordType]uint16{
	DNSRecordA:     wdns.TypeA,
	DNSRecordAAAA:  wdns.TypeAAAA,
	DNSRecordCNAME: wdns.TypeCNAME,
	DNSRecordTXT:   wdns.TypeTXT,
	DNSRecordANY:   wdns.TypeANY,
	DNSRecordMX:    wdns.TypeMX,
	DNSRecordSRV:   wdns.TypeSRV,
	DNSRecordNS:    wdns.TypeNS,
	DNSRecordCAA:   wdns.TypeCAA,
	DNSRecordHTTPS: wdns.TypeHTTPS,
	DNSRecordSVCB:  wdns.TypeSVCB,
}

func NewDNS(host string) *DNSCondition {
	return &DNSCondition{
		Host:         host,
		RecordType:   DNSRecordA,
		ResolverMode: DNSResolverSystem,
		AbsentMode:   DNSAbsentAny,
		Transport:    DNSTransportUDP,
		UDPSize:      wdns.DefaultMsgSize,
	}
}

func (c *DNSCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "dns", Target: c.Host}
}

func (c *DNSCondition) Check(ctx context.Context) Result {
	if err := c.validate(); err != nil {
		return Fatal(err)
	}
	response, err := c.lookup(ctx)
	if err != nil {
		if c.Absent && isDNSNotFound(err) && c.absentMode() == DNSAbsentAny {
			return Satisfied("dns record absent")
		}
		return Unsatisfied("", err)
	}
	return c.evaluate(response)
}

func (c *DNSCondition) validate() error {
	if err := validateDNSName(c.Host); err != nil {
		return err
	}
	if !validDNSRecordType(c.recordType()) {
		return fmt.Errorf("unsupported dns record type %q", c.RecordType)
	}
	if err := c.validateResolverOptions(); err != nil {
		return err
	}
	if err := c.validateMatchers(); err != nil {
		return err
	}
	return c.validateSystemResolverOptions()
}

func (c *DNSCondition) validateResolverOptions() error {
	switch c.resolverMode() {
	case DNSResolverSystem, DNSResolverWire:
	default:
		return fmt.Errorf("unsupported dns resolver %q", c.ResolverMode)
	}
	if !validDNSAbsentMode(c.absentMode()) {
		return fmt.Errorf("unsupported dns absent mode %q", c.AbsentMode)
	}
	if !validDNSTransport(c.transport()) {
		return fmt.Errorf("unsupported dns transport %q", c.Transport)
	}
	if c.RCode != "" && !ValidDNSRCode(c.RCode) {
		return fmt.Errorf("unsupported dns rcode %q", c.RCode)
	}
	if c.resolverMode() == DNSResolverWire && c.Server == "" && c.WireExchange == nil {
		return fmt.Errorf("dns wire resolver requires a server")
	}
	return nil
}

func (c *DNSCondition) validateMatchers() error {
	if c.MinCount < 0 {
		return fmt.Errorf("dns min-count cannot be negative")
	}
	if c.Absent && (c.Contains != "" || len(c.Equals) > 0 || c.MinCount > 0) {
		return fmt.Errorf("dns absent cannot be combined with contains, equals, or min-count")
	}
	return nil
}

func (c *DNSCondition) validateSystemResolverOptions() error {
	if c.resolverMode() != DNSResolverSystem {
		return nil
	}
	if c.RCode != "" {
		return fmt.Errorf("dns rcode checks require resolver wire")
	}
	if c.absentMode() != DNSAbsentAny {
		return fmt.Errorf("dns absent mode %s requires resolver wire", c.absentMode())
	}
	if !systemSupportsRecordType(c.recordType()) {
		return fmt.Errorf("dns record type %s requires --resolver wire", c.recordType())
	}
	return nil
}

func (c *DNSCondition) evaluate(response dnsLookupResponse) Result {
	if result, ok := c.checkRCode(response); ok {
		return result
	}
	if c.Absent {
		return c.checkAbsent(response)
	}
	if c.rcodeOnly() {
		return Satisfied(fmt.Sprintf("rcode %s", response.RCode))
	}
	if len(response.Values) == 0 {
		return Unsatisfied("no records found", fmt.Errorf("no dns records found"))
	}
	if result, ok := c.checkPresentValues(response); ok {
		return result
	}
	return Satisfied(fmt.Sprintf("%s record found", c.recordType()))
}

func (c *DNSCondition) rcodeOnly() bool {
	return c.RCode != "" && c.Contains == "" && len(c.Equals) == 0 && c.MinCount == 0
}

func (c *DNSCondition) checkRCode(response dnsLookupResponse) (Result, bool) {
	if c.RCode == "" || strings.EqualFold(response.RCode, c.RCode) {
		return Result{}, false
	}
	detail := fmt.Sprintf("rcode %s, expected %s", response.RCode, strings.ToUpper(c.RCode))
	return Unsatisfied(detail, errors.New(detail)), true
}

func (c *DNSCondition) checkPresentValues(response dnsLookupResponse) (Result, bool) {
	if c.MinCount > 0 && len(response.Values) < c.MinCount {
		detail := fmt.Sprintf("%d record(s), expected at least %d", len(response.Values), c.MinCount)
		return Unsatisfied(detail, errors.New(detail)), true
	}
	if c.Contains != "" && !containsAny(response.Values, c.Contains) {
		return Unsatisfied("dns record substring not found", fmt.Errorf("dns record does not contain required substring")), true
	}
	if len(c.Equals) > 0 && !matchesAllDNSValues(response.Values, c.Equals, c.recordType()) {
		return Unsatisfied("dns record value not found", fmt.Errorf("dns record does not equal required value")), true
	}
	return Result{}, false
}

func (c *DNSCondition) checkAbsent(response dnsLookupResponse) Result {
	switch c.absentMode() {
	case DNSAbsentNXDomain:
		if response.NXDOMAIN {
			return Satisfied("dns name absent")
		}
	case DNSAbsentNODATA:
		if response.NODATA {
			return Satisfied("dns record type absent")
		}
	default:
		if response.NXDOMAIN || response.NODATA || len(response.Values) == 0 {
			return Satisfied("dns record absent")
		}
	}
	detail := fmt.Sprintf("%d dns record(s) found", len(response.Values))
	return Unsatisfied(detail, errors.New(detail))
}

func (c *DNSCondition) lookup(ctx context.Context) (dnsLookupResponse, error) {
	if c.resolverMode() == DNSResolverWire {
		return c.lookupWire(ctx)
	}
	return c.lookupSystem(ctx)
}

func (c *DNSCondition) lookupSystem(ctx context.Context) (dnsLookupResponse, error) {
	values, err := c.lookupSystemValues(ctx)
	if err != nil {
		return dnsLookupResponse{}, err
	}
	return dnsLookupResponse{Values: values, RCode: "NOERROR", NODATA: len(values) == 0}, nil
}

func (c *DNSCondition) lookupSystemValues(ctx context.Context) ([]string, error) {
	switch c.recordType() {
	case DNSRecordA:
		return c.lookupIP(ctx, "ip4")
	case DNSRecordAAAA:
		return c.lookupIP(ctx, "ip6")
	case DNSRecordCNAME:
		return c.lookupCNAME(ctx)
	case DNSRecordTXT:
		return c.lookupTXT(ctx)
	case DNSRecordANY:
		return c.lookupHost(ctx)
	default:
		return nil, fmt.Errorf("dns record type %s requires --resolver wire", c.recordType())
	}
}

func (c *DNSCondition) lookupWire(ctx context.Context) (dnsLookupResponse, error) {
	msg := wdns.NewMsg(c.Host, c.wireType())
	if msg == nil {
		return dnsLookupResponse{}, fmt.Errorf("unsupported dns record type %q", c.RecordType)
	}
	if c.EDNS0 || c.UDPSize > 0 {
		msg.UDPSize = c.udpSize()
	}
	response, err := c.exchangeWireResponse(ctx, msg, c.transport())
	if err != nil {
		return dnsLookupResponse{}, err
	}
	if response.Truncated && c.transport() == DNSTransportUDP {
		response, err = c.exchangeWireResponse(ctx, msg, DNSTransportTCP)
		if err != nil {
			return dnsLookupResponse{}, err
		}
	}
	return c.responseFromWire(response), nil
}

func (c *DNSCondition) exchangeWireResponse(ctx context.Context, msg *wdns.Msg, transport DNSTransport) (*wdns.Msg, error) {
	response, err := c.exchangeWire(ctx, msg, string(transport))
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("empty dns response")
	}
	return response, nil
}

func (c *DNSCondition) exchangeWire(ctx context.Context, msg *wdns.Msg, network string) (*wdns.Msg, error) {
	if c.WireExchange != nil {
		return c.WireExchange(ctx, msg, network, c.Server)
	}
	response, _, err := wdns.NewClient().Exchange(ctx, msg, network, c.Server)
	return response, err
}

func (c *DNSCondition) responseFromWire(msg *wdns.Msg) dnsLookupResponse {
	values := dnsValuesFromRRs(msg.Answer, c.recordType())
	return dnsLookupResponse{
		Values:    values,
		RCode:     dnsRCodeString(msg.Rcode),
		NXDOMAIN:  msg.Rcode == wdns.RcodeNameError,
		NODATA:    msg.Rcode == wdns.RcodeSuccess && len(values) == 0,
		Truncated: msg.Truncated,
	}
}

func dnsValuesFromRRs(records []wdns.RR, recordType DNSRecordType) []string {
	values := make([]string, 0, len(records))
	for _, rr := range records {
		if !dnsRRMatchesType(rr, recordType) {
			continue
		}
		values = append(values, dnsValueFromRR(rr))
	}
	return values
}

func dnsRRMatchesType(rr wdns.RR, recordType DNSRecordType) bool {
	if recordType == DNSRecordANY {
		return true
	}
	return wdns.RRToType(rr) == dnsWireTypes[recordType]
}

func dnsValueFromRR(rr wdns.RR) string {
	if value, ok := coreDNSValueFromRR(rr); ok {
		return value
	}
	return extendedDNSValueFromRR(rr)
}

func coreDNSValueFromRR(rr wdns.RR) (string, bool) {
	switch typed := rr.(type) {
	case *wdns.A:
		return typed.Addr.String(), true
	case *wdns.AAAA:
		return typed.Addr.String(), true
	case *wdns.CNAME:
		return typed.Target, true
	case *wdns.TXT:
		return strings.Join(typed.Txt, ""), true
	case *wdns.MX:
		return fmt.Sprintf("%d %s", typed.Preference, typed.Mx), true
	default:
		return "", false
	}
}

func extendedDNSValueFromRR(rr wdns.RR) string {
	switch typed := rr.(type) {
	case *wdns.SRV:
		return fmt.Sprintf("%d %d %d %s", typed.Priority, typed.Weight, typed.Port, typed.Target)
	case *wdns.NS:
		return typed.Ns
	case *wdns.CAA:
		return fmt.Sprintf("%d %s %s", typed.Flag, typed.Tag, typed.Value)
	case *wdns.HTTPS:
		return typed.SVCB.SVCB.String()
	case *wdns.SVCB:
		return typed.SVCB.String()
	default:
		return rr.Data().String()
	}
}

func (c *DNSCondition) recordType() DNSRecordType {
	if c.RecordType == "" {
		return DNSRecordA
	}
	return DNSRecordType(strings.ToUpper(string(c.RecordType)))
}

func (c *DNSCondition) resolverMode() DNSResolverMode {
	if c.ResolverMode == "" {
		return DNSResolverSystem
	}
	return DNSResolverMode(strings.ToLower(string(c.ResolverMode)))
}

func (c *DNSCondition) absentMode() DNSAbsentMode {
	if c.AbsentMode == "" {
		return DNSAbsentAny
	}
	return DNSAbsentMode(strings.ToLower(string(c.AbsentMode)))
}

func (c *DNSCondition) transport() DNSTransport {
	if c.Transport == "" {
		return DNSTransportUDP
	}
	return DNSTransport(strings.ToLower(string(c.Transport)))
}

func (c *DNSCondition) udpSize() uint16 {
	if c.UDPSize == 0 {
		return wdns.DefaultMsgSize
	}
	return c.UDPSize
}

func (c *DNSCondition) wireType() uint16 {
	return dnsWireTypes[c.recordType()]
}

func validDNSRecordType(recordType DNSRecordType) bool {
	_, ok := dnsWireTypes[recordType]
	return ok
}

func validDNSAbsentMode(absentMode DNSAbsentMode) bool {
	switch absentMode {
	case DNSAbsentAny, DNSAbsentNXDomain, DNSAbsentNODATA:
		return true
	default:
		return false
	}
}

func validDNSTransport(transport DNSTransport) bool {
	switch transport {
	case DNSTransportUDP, DNSTransportTCP:
		return true
	default:
		return false
	}
}

func ValidDNSRCode(rcode string) bool {
	_, ok := dnsRCodeValue(rcode)
	return ok
}

func dnsRCodeValue(rcode string) (uint16, bool) {
	want := strings.ToUpper(strings.TrimSpace(rcode))
	for code, value := range wdns.RcodeToString {
		if value == want {
			return code, true
		}
	}
	return 0, false
}

func systemSupportsRecordType(recordType DNSRecordType) bool {
	switch recordType {
	case DNSRecordA, DNSRecordAAAA, DNSRecordCNAME, DNSRecordTXT, DNSRecordANY:
		return true
	default:
		return false
	}
}

func (c *DNSCondition) lookupHost(ctx context.Context) ([]string, error) {
	if c.LookupHost != nil {
		return c.LookupHost(ctx, c.Host)
	}
	return c.resolver().LookupHost(ctx, c.Host)
}

func (c *DNSCondition) lookupIP(ctx context.Context, network string) ([]string, error) {
	var ips []net.IP
	var err error
	if c.LookupIP != nil {
		ips, err = c.LookupIP(ctx, network, c.Host)
	} else {
		ips, err = c.resolver().LookupIP(ctx, network, c.Host)
	}
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(ips))
	for _, ip := range ips {
		values = append(values, ip.String())
	}
	return values, nil
}

func (c *DNSCondition) lookupCNAME(ctx context.Context) ([]string, error) {
	var value string
	var err error
	if c.LookupCNAME != nil {
		value, err = c.LookupCNAME(ctx, c.Host)
	} else {
		value, err = c.resolver().LookupCNAME(ctx, c.Host)
	}
	if err != nil {
		return nil, err
	}
	return []string{value}, nil
}

func (c *DNSCondition) lookupTXT(ctx context.Context) ([]string, error) {
	if c.LookupTXT != nil {
		return c.LookupTXT(ctx, c.Host)
	}
	return c.resolver().LookupTXT(ctx, c.Host)
}

func (c *DNSCondition) resolver() *net.Resolver {
	if c.Server == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: 2 * time.Second}
			return dialer.DialContext(ctx, network, c.Server)
		},
	}
}

func containsAny(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func matchesAllDNSValues(values, wants []string, recordType DNSRecordType) bool {
	for _, want := range wants {
		if !matchesDNSValue(values, want, recordType) {
			return false
		}
	}
	return true
}

func matchesDNSValue(values []string, want string, recordType DNSRecordType) bool {
	for _, value := range values {
		if dnsValuesEqual(value, want, recordType) {
			return true
		}
	}
	return false
}

func dnsValuesEqual(got, want string, recordType DNSRecordType) bool {
	if isDNSNameValue(recordType) {
		return normalizeDNSName(got) == normalizeDNSName(want)
	}
	return got == want
}

func isDNSNameValue(recordType DNSRecordType) bool {
	switch recordType {
	case DNSRecordCNAME, DNSRecordNS:
		return true
	default:
		return false
	}
}

func normalizeDNSName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func isDNSNotFound(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}

func dnsRCodeString(code uint16) string {
	if value, ok := wdns.RcodeToString[code]; ok {
		return value
	}
	return fmt.Sprintf("RCODE%d", code)
}

func validateDNSName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("dns host is required")
	}
	trimmed := strings.TrimSuffix(name, ".")
	if len(trimmed) > 253 {
		return fmt.Errorf("dns host exceeds 253 octets")
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" {
			return fmt.Errorf("dns host contains an empty label")
		}
		if len(label) > 63 {
			return fmt.Errorf("dns label exceeds 63 octets")
		}
	}
	return nil
}
