package condition

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	ntpPacketSize  = 48
	ntpUnixOffset  = 2208988800
	defaultNTPPort = "123"
)

type NTPCondition struct {
	Address        string
	MaxOffset      time.Duration
	AttemptTimeout time.Duration
	Query          func(context.Context, string) (time.Duration, error)
	now            func() time.Time
}

func NewNTP(address string) *NTPCondition {
	return &NTPCondition{Address: address, AttemptTimeout: 2 * time.Second}
}

func (c *NTPCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "ntp", Target: c.Address}
}

func (c *NTPCondition) Check(ctx context.Context) Result {
	if err := validateNTPConfig(c); err != nil {
		return Fatal(err)
	}
	offset, err := c.query(ctx)
	if err != nil {
		return Unsatisfied("", err)
	}
	if c.MaxOffset > 0 && absDuration(offset) > c.MaxOffset {
		return Unsatisfied("ntp offset too large", fmt.Errorf("ntp offset %s exceeds %s", offset, c.MaxOffset))
	}
	return Satisfied(fmt.Sprintf("ntp responded with offset %s", offset))
}

func validateNTPConfig(c *NTPCondition) error {
	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("ntp address is required")
	}
	if err := validateNTPAddress(c.Address); err != nil {
		return err
	}
	if c.MaxOffset < 0 {
		return fmt.Errorf("--max-offset must be non-negative")
	}
	if c.AttemptTimeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	return nil
}

func validateNTPAddress(address string) error {
	if host, port, err := net.SplitHostPort(address); err == nil {
		if host == "" || !validPortNumber(port) {
			return fmt.Errorf("invalid ntp address %q", address)
		}
		return nil
	}
	if strings.HasPrefix(address, "[") || strings.HasSuffix(address, "]") {
		host := strings.TrimPrefix(strings.TrimSuffix(address, "]"), "[")
		if net.ParseIP(host) == nil {
			return fmt.Errorf("invalid ntp address %q", address)
		}
		return nil
	}
	if strings.Count(address, ":") == 1 {
		return fmt.Errorf("invalid ntp address %q", address)
	}
	return nil
}

func validPortNumber(port string) bool {
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1 && value <= 65535
}

func (c *NTPCondition) query(ctx context.Context) (time.Duration, error) {
	if c.Query != nil {
		return c.Query(ctx, c.Address)
	}
	timeout := c.AttemptTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return defaultNTPQuery(ctx, ntpAddress(c.Address), timeout, c.clock())
}

func (c *NTPCondition) clock() func() time.Time {
	if c.now != nil {
		return c.now
	}
	return time.Now
}

func ntpAddress(address string) string {
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		return net.JoinHostPort(strings.TrimPrefix(strings.TrimSuffix(address, "]"), "["), defaultNTPPort)
	}
	return net.JoinHostPort(address, defaultNTPPort)
}

func defaultNTPQuery(ctx context.Context, address string, timeout time.Duration, now func() time.Time) (time.Duration, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	stopCancelWatcher := closeOnContextDone(ctx, conn)
	defer stopCancelWatcher()
	deadline := now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	return exchangeNTP(ctx, conn, now)
}

func exchangeNTP(ctx context.Context, conn net.Conn, now func() time.Time) (time.Duration, error) {
	request := make([]byte, ntpPacketSize)
	request[0] = 0x23 // LI=0, VN=4, Mode=3 client, as specified by RFC 5905.
	t1 := now()
	writeNTPTimestamp(request[40:], t1)
	if _, err := conn.Write(request); err != nil {
		return 0, err
	}
	response := make([]byte, ntpPacketSize)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(conn, response); err != nil {
		return 0, err
	}
	t4 := now()
	return parseNTPOffset(response, request[40:48], t1, t4)
}

func parseNTPOffset(packet []byte, originate []byte, t1, t4 time.Time) (time.Duration, error) {
	if len(packet) < ntpPacketSize {
		return 0, fmt.Errorf("short ntp packet")
	}
	if err := validateNTPHeader(packet, originate); err != nil {
		return 0, err
	}
	t2 := readNTPTimestamp(packet[32:])
	t3 := readNTPTimestamp(packet[40:])
	if t2.IsZero() || t3.IsZero() {
		return 0, fmt.Errorf("ntp response missing timestamps")
	}
	if t3.Before(t2) {
		return 0, fmt.Errorf("ntp response transmit timestamp precedes receive timestamp")
	}
	return (t2.Sub(t1) + t3.Sub(t4)) / 2, nil
}

func validateNTPHeader(packet []byte, originate []byte) error {
	li := packet[0] >> 6
	version := (packet[0] >> 3) & 0x7
	mode := packet[0] & 0x7
	if li == 3 {
		return fmt.Errorf("ntp server clock unsynchronized")
	}
	if version < 3 || version > 4 {
		return fmt.Errorf("unsupported ntp version %d", version)
	}
	if mode != 4 {
		return fmt.Errorf("invalid ntp mode %d", mode)
	}
	if packet[1] == 0 {
		return fmt.Errorf("ntp kiss-of-death or unspecified stratum")
	}
	if packet[1] >= 16 {
		return fmt.Errorf("ntp stratum %d is unsynchronized or invalid", packet[1])
	}
	if len(originate) == 8 && !bytes.Equal(packet[24:32], originate) {
		return fmt.Errorf("ntp originate timestamp mismatch")
	}
	return nil
}

func writeNTPTimestamp(dst []byte, t time.Time) {
	seconds := clampInt64ToUint32(t.Unix() + ntpUnixOffset)
	fraction := uint64(float64(t.Nanosecond()) * (1 << 32) / 1e9)
	binary.BigEndian.PutUint32(dst[0:4], seconds)
	binary.BigEndian.PutUint32(dst[4:8], clampUint64ToUint32(fraction))
}

func readNTPTimestamp(src []byte) time.Time {
	seconds := binary.BigEndian.Uint32(src[0:4])
	fraction := binary.BigEndian.Uint32(src[4:8])
	if seconds == 0 && fraction == 0 {
		return time.Time{}
	}
	unixSeconds := int64(seconds) - ntpUnixOffset
	nanos := int64(math.Round(float64(fraction) * 1e9 / (1 << 32)))
	return time.Unix(unixSeconds, nanos)
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func clampInt64ToUint32(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func clampUint64ToUint32(value uint64) uint32 {
	if value > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}
