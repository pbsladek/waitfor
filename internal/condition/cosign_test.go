package condition

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestCosignConditionBlobVerification(t *testing.T) {
	cond := NewCosign("artifact")
	cond.Mode = CosignBlob
	cond.Signature = "artifact.sig"
	cond.Verify = func(_ context.Context, got *CosignCondition) error {
		if got.Target != "artifact" || got.Signature != "artifact.sig" {
			t.Fatalf("cosign condition = %+v", got)
		}
		return nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestCosignCheckBranches(t *testing.T) {
	denied := NewCosign("image")
	denied.Verify = func(context.Context, *CosignCondition) error { return errors.New("signature denied") }
	if result := denied.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("denied status = %s, want unsatisfied", result.Status)
	}
	missing := NewCosign("image")
	missing.Verify = func(context.Context, *CosignCondition) error { return exec.ErrNotFound }
	if result := missing.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing cosign status = %s, want fatal", result.Status)
	}
}

func TestCosignValidationArgsAndDescriptor(t *testing.T) {
	cond := NewCosign("image")
	cond.Key = "key.pem"
	cond.Certificate = "cert.pem"
	cond.Identity = "https://github.com/org/repo/.github/workflows/release.yml@refs/heads/main"
	cond.OIDCIssuer = "https://token.actions.githubusercontent.com"
	args := strings.Join(cosignArgs(cond), " ")
	if !strings.Contains(args, "--certificate-identity") || !strings.Contains(args, "--certificate-oidc-issuer") {
		t.Fatalf("cosign args = %q", args)
	}
	invalid := []*CosignCondition{
		{Target: "blob", Mode: CosignBlob},
		{Target: "-keyless", Mode: CosignImage},
		{Target: "", Mode: CosignImage},
		{Target: "image", Key: "-key.pem", Mode: CosignImage},
		{Target: "image", Mode: CosignMode("bundle")},
	}
	for _, candidate := range invalid {
		if err := validateCosignConfig(candidate); err == nil {
			t.Fatalf("validateCosignConfig(%+v) succeeded", candidate)
		}
	}
	desc := NewCosign("ghcr.io/example/app:latest").Descriptor()
	if desc.Backend != "cosign" || desc.Target != "ghcr.io/example/app:latest" {
		t.Fatalf("descriptor = %+v", desc)
	}
}
