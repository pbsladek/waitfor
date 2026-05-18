package condition

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type LaunchdState string

const (
	LaunchdLoaded  LaunchdState = "loaded"
	LaunchdRunning LaunchdState = "running"
)

type CosignMode string

const (
	CosignImage CosignMode = "image"
	CosignBlob  CosignMode = "blob"
)

type LaunchdCondition struct {
	Label string
	State LaunchdState
	Print func(context.Context, string) (string, error)
}

type CosignCondition struct {
	Target      string
	Mode        CosignMode
	Key         string
	Signature   string
	Certificate string
	Identity    string
	OIDCIssuer  string
	Verify      func(context.Context, *CosignCondition) error
}

type ICMPCondition struct {
	Host           string
	Count          int
	AttemptTimeout time.Duration
	Ping           func(context.Context, string) error
}

const maxICMPTimeout = 24 * time.Hour

func NewLaunchd(label string) *LaunchdCondition {
	return &LaunchdCondition{Label: label, State: LaunchdRunning}
}

func NewCosign(target string) *CosignCondition {
	return &CosignCondition{Target: target, Mode: CosignImage}
}

func NewICMP(host string) *ICMPCondition {
	return &ICMPCondition{Host: host, Count: 1, AttemptTimeout: time.Second}
}

func (c *LaunchdCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "launchd", Target: c.Label}
}

func (c *CosignCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "cosign", Target: c.Target}
}

func (c *ICMPCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "icmp", Target: c.Host}
}

func (c *LaunchdCondition) Check(ctx context.Context) Result {
	if err := validateLaunchdConfig(c); err != nil {
		return Fatal(err)
	}
	output, err := c.print(ctx)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Fatal(fmt.Errorf("launchctl command not found"))
		}
		return Unsatisfied("", err)
	}
	if c.State == LaunchdLoaded {
		return Satisfied("launchd service is loaded")
	}
	return checkLaunchdRunning(output)
}

func (c *CosignCondition) Check(ctx context.Context) Result {
	if err := validateCosignConfig(c); err != nil {
		return Fatal(err)
	}
	if err := c.verify(ctx); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Fatal(fmt.Errorf("cosign command not found"))
		}
		return Unsatisfied("", err)
	}
	return Satisfied("cosign verification succeeded")
}

func (c *ICMPCondition) Check(ctx context.Context) Result {
	if err := validateICMPConfig(c); err != nil {
		return Fatal(err)
	}
	if err := c.ping(ctx); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Fatal(fmt.Errorf("ping command not found"))
		}
		return Unsatisfied("", err)
	}
	return Satisfied("icmp ping succeeded")
}

func validateICMPConfig(c *ICMPCondition) error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("icmp host is required")
	}
	if err := rejectOptionLike("icmp host", c.Host); err != nil {
		return err
	}
	if c.Count <= 0 {
		return fmt.Errorf("--count must be positive")
	}
	if c.AttemptTimeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	if c.AttemptTimeout > maxICMPTimeout {
		return fmt.Errorf("--timeout must be at most %s", maxICMPTimeout)
	}
	return nil
}

func validateLaunchdConfig(c *LaunchdCondition) error {
	if strings.TrimSpace(c.Label) == "" {
		return fmt.Errorf("launchd label is required")
	}
	if err := rejectOptionLike("launchd label", c.Label); err != nil {
		return err
	}
	switch c.State {
	case LaunchdLoaded, LaunchdRunning:
		return nil
	default:
		return fmt.Errorf("unsupported launchd state %q", c.State)
	}
}

func validateCosignConfig(c *CosignCondition) error {
	if strings.TrimSpace(c.Target) == "" {
		return fmt.Errorf("cosign target is required")
	}
	for label, value := range map[string]string{
		"cosign target":                  c.Target,
		"cosign key":                     c.Key,
		"cosign signature":               c.Signature,
		"cosign certificate":             c.Certificate,
		"cosign certificate identity":    c.Identity,
		"cosign certificate oidc issuer": c.OIDCIssuer,
	} {
		if err := rejectOptionLike(label, value); err != nil {
			return err
		}
	}
	switch c.Mode {
	case CosignImage:
		return nil
	case CosignBlob:
		if c.Signature == "" {
			return fmt.Errorf("cosign blob verification requires --signature")
		}
		return nil
	default:
		return fmt.Errorf("unsupported cosign mode %q", c.Mode)
	}
}

func checkLaunchdRunning(output string) Result {
	if launchdOutputHasPID(output) {
		return Satisfied("launchd service is running")
	}
	return Unsatisfied("launchd service is loaded but not running", fmt.Errorf("launchd service is not running"))
}

func launchdOutputHasPID(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 3 && fields[0] == "pid" && fields[1] == "=" && fields[2] != "0" {
			return true
		}
	}
	return false
}

func (c *LaunchdCondition) print(ctx context.Context) (string, error) {
	if c.Print != nil {
		return c.Print(ctx, c.Label)
	}
	return defaultLaunchctlPrint(ctx, c.Label)
}

func defaultLaunchctlPrint(ctx context.Context, label string) (string, error) {
	out, err := runLimitedCommand(ctx, "launchctl", "print", label)
	if err != nil {
		return "", classifyLimitedCommandError(err, out, ctx.Err())
	}
	return string(out.stdout), nil
}

func (c *CosignCondition) verify(ctx context.Context) error {
	if c.Verify != nil {
		return c.Verify(ctx, c)
	}
	return defaultCosignVerify(ctx, c)
}

func defaultCosignVerify(ctx context.Context, c *CosignCondition) error {
	args := cosignArgs(c)
	out, err := runLimitedCommand(ctx, "cosign", args...)
	if err != nil {
		return classifyLimitedCommandError(err, out, ctx.Err())
	}
	return nil
}

func cosignArgs(c *CosignCondition) []string {
	if c.Mode == CosignBlob {
		args := []string{"verify-blob", "--signature", c.Signature}
		return appendCosignOptions(append(args, c.Target), c)
	}
	return appendCosignOptions([]string{"verify", c.Target}, c)
}

func appendCosignOptions(args []string, c *CosignCondition) []string {
	if c.Key != "" {
		args = append(args, "--key", c.Key)
	}
	if c.Certificate != "" {
		args = append(args, "--certificate", c.Certificate)
	}
	if c.Identity != "" {
		args = append(args, "--certificate-identity", c.Identity)
	}
	if c.OIDCIssuer != "" {
		args = append(args, "--certificate-oidc-issuer", c.OIDCIssuer)
	}
	return args
}

func (c *ICMPCondition) ping(ctx context.Context) error {
	if c.Ping != nil {
		return c.Ping(ctx, c.Host)
	}
	return defaultPing(ctx, c.Host, c.Count, c.AttemptTimeout)
}

func defaultPing(ctx context.Context, host string, count int, timeout time.Duration) error {
	args := pingArgs(host, count, timeout)
	out, err := runLimitedCommand(ctx, "ping", args...)
	if err != nil {
		return classifyLimitedCommandError(err, out, ctx.Err())
	}
	return nil
}

func pingArgs(host string, count int, timeout time.Duration) []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"-n", strconv.Itoa(count), "-w", strconv.Itoa(timeoutMillis(timeout)), host}
	case "darwin":
		return []string{"-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutMillis(timeout)), host}
	default:
		return []string{"-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutSeconds(timeout)), host}
	}
}

func timeoutMillis(timeout time.Duration) int {
	if timeout <= 0 {
		return 1000
	}
	ms := int(timeout.Round(time.Millisecond) / time.Millisecond)
	if ms <= 0 {
		return 1
	}
	return ms
}

func timeoutSeconds(timeout time.Duration) int {
	if timeout <= 0 {
		return 1
	}
	seconds := int(timeout.Round(time.Second) / time.Second)
	if seconds <= 0 {
		return 1
	}
	return seconds
}
