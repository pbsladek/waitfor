package cli

import (
	"testing"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseCosignCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"cosign", "--blob", "artifact", "--signature", "artifact.sig", "--certificate-identity", "id", "--certificate-oidc-issuer", "issuer"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.CosignCondition)
	if got.Mode != condition.CosignBlob || got.Signature != "artifact.sig" || got.Identity != "id" || got.OIDCIssuer != "issuer" {
		t.Fatalf("cosign = %+v", got)
	}
}
