//go:build !unix

package cli

import (
	"fmt"
	"os"
)

func openRegularFile(path string) (*os.File, os.FileInfo, error) {
	file, err := os.Open(path) // #nosec G304 -- callers intentionally read user-selected CLI input files.
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("file must be a regular file")
	}
	return file, info, nil
}
