package condition

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
)

const maxFileContainsBytes int64 = 10 * 1024 * 1024
const fileContainsBufferSize = 32 * 1024

type FileState string

const (
	FileExists   FileState = "exists"
	FileDeleted  FileState = "deleted"
	FileNonEmpty FileState = "nonempty"
)

type FileCondition struct {
	Path     string
	State    FileState
	Contains string
}

func NewFile(path string, state FileState) *FileCondition {
	return &FileCondition{Path: path, State: state}
}

func (c *FileCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "file", Target: c.Path}
}

func (c *FileCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}

	if err := validateFileConfig(c); err != nil {
		return Fatal(err)
	}

	info, err := os.Stat(c.Path)
	if c.State == FileDeleted {
		return checkFileDeleted(err)
	}
	if err != nil {
		return checkFileStatError(err)
	}
	return checkExistingFile(ctx, c, info)
}

func validateFileConfig(c *FileCondition) error {
	if c.Path == "" {
		return fmt.Errorf("file path is required")
	}
	switch c.State {
	case FileExists, FileDeleted, FileNonEmpty:
	default:
		return fmt.Errorf("unsupported file state %q", c.State)
	}
	if c.State == FileDeleted && c.Contains != "" {
		return fmt.Errorf("--deleted cannot be combined with --contains")
	}
	return nil
}

func checkFileStatError(err error) Result {
	if err != nil {
		if os.IsNotExist(err) {
			return Unsatisfied("file does not exist", err)
		}
		return Unsatisfied("", err)
	}
	return Satisfied("exists")
}

func checkExistingFile(ctx context.Context, c *FileCondition, info os.FileInfo) Result {
	if c.State == FileNonEmpty && info.Size() == 0 {
		return Unsatisfied("file is empty", fmt.Errorf("file is empty"))
	}
	if c.Contains != "" {
		if !info.Mode().IsRegular() {
			return Fatal(fmt.Errorf("file content checks require a regular file"))
		}
		return checkFileContent(ctx, c.Path, c.Contains)
	}
	return Satisfied(string(c.State))
}

func checkFileDeleted(statErr error) Result {
	if os.IsNotExist(statErr) {
		return Satisfied("file is deleted")
	}
	if statErr != nil {
		return Unsatisfied("", statErr)
	}
	return Unsatisfied("file still exists", fmt.Errorf("file still exists"))
}

func checkFileContent(ctx context.Context, path, contains string) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	found, err := streamFileContainsLimit(ctx, path, []byte(contains), maxFileContainsBytes)
	if err != nil {
		return Unsatisfied("", err)
	}
	if !found {
		return Unsatisfied("file substring not found", fmt.Errorf("file does not contain required substring"))
	}
	return Satisfied("file contains required substring")
}

func streamFileContainsLimit(ctx context.Context, path string, needle []byte, limit int64) (bool, error) {
	file, _, err := openRegularFile(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()

	return readerContainsLimit(ctx, file, needle, limit)
}

func readerContainsLimit(ctx context.Context, reader io.Reader, needle []byte, limit int64) (bool, error) {
	if len(needle) == 0 {
		return true, nil
	}
	buf := make([]byte, fileContainsBufferSize)
	carry := []byte(nil)
	remaining := limit
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := reader.Read(buf[:limitedReadSize(len(buf), remaining)])
		if n > 0 {
			remaining -= int64(n)
			found, nextCarry := containsWithCarry(carry, buf[:n], needle)
			if found {
				return true, nil
			}
			carry = nextCarry
		}
		if err != nil {
			if err == io.EOF {
				return false, nil
			}
			return false, err
		}
	}
	return false, nil
}

func limitedReadSize(size int, remaining int64) int {
	if int64(size) > remaining {
		return int(remaining)
	}
	return size
}

func containsWithCarry(carry, chunk, needle []byte) (bool, []byte) {
	if bytes.Contains(chunk, needle) || boundaryContains(carry, chunk, needle) {
		return true, nil
	}
	return false, trailingWindow(carry, chunk, len(needle)-1)
}

func boundaryContains(carry, chunk, needle []byte) bool {
	if len(carry) == 0 || len(chunk) == 0 || len(needle) <= 1 {
		return false
	}
	prefixLen := minInt(len(needle)-1, len(chunk))
	window := make([]byte, 0, len(carry)+prefixLen)
	window = append(window, carry...)
	window = append(window, chunk[:prefixLen]...)
	return bytes.Contains(window, needle)
}

func trailingWindow(carry, chunk []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	if len(chunk) >= n {
		return trailingBytes(chunk, n)
	}
	window := make([]byte, 0, len(carry)+len(chunk))
	window = append(window, carry...)
	window = append(window, chunk...)
	return trailingBytes(window, n)
}

func trailingBytes(data []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	if len(data) > n {
		data = data[len(data)-n:]
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
