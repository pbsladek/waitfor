package cli

import (
	"testing"

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestParseArchiveCondition(t *testing.T) {
	parsed, err := parseCondition([]string{"archive", "/tmp/app.zip", "--matches", "bin/*", "--format", "zip"})
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.(*condition.ArchiveCondition)
	if got.Format != condition.ArchiveZip || got.Matches != "bin/*" {
		t.Fatalf("archive = %+v", got)
	}
}
