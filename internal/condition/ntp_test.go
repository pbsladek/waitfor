package condition

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNTPConditionSatisfiedAndQueryError(t *testing.T) {
	cond := NewNTP("time.example:123")
	cond.MaxOffset = time.Second
	cond.Query = func(_ context.Context, address string) (time.Duration, error) {
		if address != "time.example:123" {
			t.Fatalf("ntp address = %q", address)
		}
		return 5 * time.Millisecond, nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("ntp satisfied status = %s, err = %v", result.Status, result.Err)
	}
	failing := NewNTP("time.example:123")
	failing.Query = func(context.Context, string) (time.Duration, error) {
		return 0, errors.New("ntp unavailable")
	}
	if result := failing.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("ntp query error status = %s, want unsatisfied", result.Status)
	}
}

func TestNTPDescriptor(t *testing.T) {
	desc := NewNTP("time.example").Descriptor()
	if desc.Backend != "ntp" || desc.Target != "time.example" {
		t.Fatalf("descriptor = %+v", desc)
	}
}

func TestNTPConditionMaxOffset(t *testing.T) {
	cond := NewNTP("time.example:123")
	cond.MaxOffset = time.Second
	cond.Query = func(context.Context, string) (time.Duration, error) {
		return 2 * time.Second, nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestNTPHelpers(t *testing.T) {
	if got := ntpAddress("time.example"); got != "time.example:123" {
		t.Fatalf("ntpAddress = %q", got)
	}
	if got := absDuration(-time.Second); got != time.Second {
		t.Fatalf("absDuration = %s", got)
	}
	if clampInt64ToUint32(-1) != 0 || clampUint64ToUint32(uint64(^uint32(0))+1) != ^uint32(0) {
		t.Fatal("clamp helpers did not clamp")
	}
	packet := make([]byte, ntpPacketSize)
	packet[0] = 0x24
	packet[1] = 1
	origin := make([]byte, 8)
	writeNTPTimestamp(origin, time.Now())
	copy(packet[24:32], origin)
	writeNTPTimestamp(packet[32:40], time.Now())
	writeNTPTimestamp(packet[40:48], time.Now())
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err != nil {
		t.Fatalf("parseNTPOffset() error = %v", err)
	}
	packet[0] = 0
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err == nil {
		t.Fatal("invalid NTP mode succeeded")
	}
	packet[0] = 0x24
	packet[1] = 16
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err == nil {
		t.Fatal("unsynchronized NTP stratum succeeded")
	}
	if err := validateNTPConfig(&NTPCondition{Address: "time.example:bad"}); err == nil {
		t.Fatal("invalid NTP port succeeded")
	}
	if got := ntpAddress("[::1]"); got != "[::1]:123" {
		t.Fatalf("ntpAddress IPv6 = %q", got)
	}
	if err := validateNTPConfig(&NTPCondition{Address: "[not-ip]"}); err == nil {
		t.Fatal("invalid bracketed NTP host succeeded")
	}
}

func TestReadNTPTimestampZero(t *testing.T) {
	if got := readNTPTimestamp(make([]byte, 8)); !got.IsZero() {
		t.Fatalf("zero timestamp = %s", got)
	}
}

func TestNTPValidationBranches(t *testing.T) {
	cond := NewNTP("")
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing ntp address status = %s", result.Status)
	}
	cond = NewNTP("time.example")
	cond.MaxOffset = -time.Second
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("negative ntp offset status = %s", result.Status)
	}
	packet := make([]byte, ntpPacketSize)
	packet[0] = 0x24
	packet[1] = 1
	origin := make([]byte, 8)
	writeNTPTimestamp(origin, time.Now())
	copy(packet[24:32], origin)
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err == nil {
		t.Fatal("missing NTP timestamps succeeded")
	}
	if clampInt64ToUint32(int64(^uint32(0))+1) != ^uint32(0) {
		t.Fatal("int64 clamp did not cap max")
	}
	cond = NewNTP("time.example")
	cond.AttemptTimeout = -time.Second
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("negative ntp timeout status = %s", result.Status)
	}
	fixed := time.Unix(100, 0)
	cond.now = func() time.Time { return fixed }
	if got := cond.clock()(); !got.Equal(fixed) {
		t.Fatalf("custom ntp clock = %s, want %s", got, fixed)
	}
}

func TestNTPRFCValidationBranches(t *testing.T) {
	origin := make([]byte, 8)
	writeNTPTimestamp(origin, time.Now())
	packet := make([]byte, ntpPacketSize)
	packet[0] = 0x24
	packet[1] = 1
	copy(packet[24:32], origin)
	now := time.Now()
	writeNTPTimestamp(packet[32:40], now)
	writeNTPTimestamp(packet[40:48], now.Add(time.Millisecond))

	tests := []struct {
		name  string
		patch func([]byte)
	}{
		{name: "unsynchronized leap", patch: func(p []byte) { p[0] = 0xe4 }},
		{name: "old version", patch: func(p []byte) { p[0] = 0x14 }},
		{name: "client mode", patch: func(p []byte) { p[0] = 0x23 }},
		{name: "stratum zero", patch: func(p []byte) { p[1] = 0 }},
		{name: "originate mismatch", patch: func(p []byte) { p[24] ^= 0xff }},
		{name: "transmit before receive", patch: func(p []byte) {
			writeNTPTimestamp(p[32:40], now.Add(time.Second))
			writeNTPTimestamp(p[40:48], now)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := append([]byte(nil), packet...)
			tt.patch(candidate)
			if _, err := parseNTPOffset(candidate, origin, now, now.Add(2*time.Millisecond)); err == nil {
				t.Fatal("invalid NTP packet succeeded")
			}
		})
	}
}
