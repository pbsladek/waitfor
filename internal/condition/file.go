package condition

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
)

const maxFileContainsBytes int64 = 10 * 1024 * 1024

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
	return Descriptor{Backend: "file", Target: c.Path, Name: fmt.Sprintf("file %s %s", c.Path, c.State)}
}

func (c *FileCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}

	info, err := os.Stat(c.Path)
	if c.State == FileDeleted {
		return checkFileDeleted(err)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return Unsatisfied("file does not exist", err)
		}
		return Unsatisfied("", err)
	}
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
	body, err := readFileContentLimit(path, maxFileContainsBytes)
	if err != nil {
		return Unsatisfied("", err)
	}
	if !bytes.Contains(body, []byte(contains)) {
		return Unsatisfied("file substring not found", fmt.Errorf("file does not contain required substring"))
	}
	return Satisfied("file contains required substring")
}

func readFileContentLimit(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	return io.ReadAll(io.LimitReader(file, limit))
}
