//go:build unix

package condition

import (
	"context"
	"errors"
	"syscall"
)

func defaultPIDExists(ctx context.Context, pid int) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	return pidExistsFromSignalError(syscall.Kill(pid, 0))
}

func pidExistsFromSignalError(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}
