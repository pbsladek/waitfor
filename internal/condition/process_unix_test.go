//go:build unix

package condition

import (
	"errors"
	"syscall"
	"testing"
)

func TestPIDExistsFromSignalError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantExists bool
		wantErr    error
	}{
		{name: "nil", wantExists: true},
		{name: "permission", err: syscall.EPERM, wantExists: true},
		{name: "missing", err: syscall.ESRCH},
		{name: "other", err: syscall.EINVAL, wantErr: syscall.EINVAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exists, err := pidExistsFromSignalError(tt.err)
			if exists != tt.wantExists {
				t.Fatalf("exists = %v, want %v", exists, tt.wantExists)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
