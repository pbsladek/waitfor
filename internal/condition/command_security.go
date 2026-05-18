package condition

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const maxExternalCommandOutputBytes int64 = 64 * 1024

type commandOutput struct {
	stdout    []byte
	stderr    []byte
	truncated bool
}

func runLimitedCommand(ctx context.Context, name string, args ...string) (commandOutput, error) {
	var stdout limitedBuffer
	var stderr limitedBuffer
	stdout.limit = maxExternalCommandOutputBytes
	stderr.limit = maxExternalCommandOutputBytes
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- callers pass fixed executables with prevalidated arguments.
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return commandOutput{
		stdout:    stdout.Bytes(),
		stderr:    stderr.Bytes(),
		truncated: stdout.truncated || stderr.truncated,
	}, err
}

func (o commandOutput) combined(limit int64) []byte {
	var output limitedBuffer
	output.limit = limit
	_, _ = output.Write(o.stdout)
	_, _ = output.Write(o.stderr)
	out := output.Bytes()
	if o.truncated || output.truncated {
		out = append(out, []byte("...(truncated)")...)
	}
	return out
}

func rejectOptionLike(label, value string) error {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("%s cannot begin with '-'", label)
	}
	return nil
}

func classifyLimitedCommandError(err error, out commandOutput, ctxErr error) error {
	if ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, exec.ErrNotFound) {
		return exec.ErrNotFound
	}
	detail := strings.TrimSpace(string(out.combined(maxExternalCommandOutputBytes)))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%s: %w", detail, err)
}
