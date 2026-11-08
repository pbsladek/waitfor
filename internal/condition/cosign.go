package condition

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type CosignMode string

const (
	CosignImage CosignMode = "image"
	CosignBlob  CosignMode = "blob"
)

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

func NewCosign(target string) *CosignCondition {
	return &CosignCondition{Target: target, Mode: CosignImage}
}

func (c *CosignCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "cosign", Target: c.Target}
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
