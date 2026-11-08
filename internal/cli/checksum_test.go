package cli

import (
	"testing"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseChecksumCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"checksum", "/tmp/app", "--equals", "sha512:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.(*condition.ChecksumCondition); got.Algorithm != condition.ChecksumAuto {
		t.Fatalf("checksum = %+v", got)
	}
}
