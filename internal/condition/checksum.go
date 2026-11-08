package condition

import (
	"context"
	"crypto/sha1" //nolint:gosec // SHA-1 is supported only for matching externally supplied artifact hashes.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

type ChecksumAlgorithm string

const (
	ChecksumAuto   ChecksumAlgorithm = "auto"
	ChecksumSHA1   ChecksumAlgorithm = "sha1"
	ChecksumSHA256 ChecksumAlgorithm = "sha256"
	ChecksumSHA512 ChecksumAlgorithm = "sha512"
)

type ChecksumCondition struct {
	Path      string
	Algorithm ChecksumAlgorithm
	Expected  string
}

func NewChecksum(path string) *ChecksumCondition {
	return &ChecksumCondition{Path: path, Algorithm: ChecksumAuto}
}

func (c *ChecksumCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "checksum", Target: c.Path}
}

func (c *ChecksumCondition) Check(ctx context.Context) Result {
	algorithm, expected, err := resolvedChecksum(c)
	if err != nil {
		return Fatal(err)
	}
	if err := validateChecksumConfig(c, algorithm, expected); err != nil {
		return Fatal(err)
	}
	actual, err := fileChecksum(ctx, c.Path, algorithm)
	if err != nil {
		return Unsatisfied("", err)
	}
	if actual != expected {
		return Unsatisfied("checksum mismatch", fmt.Errorf("checksum mismatch"))
	}
	return Satisfied(string(algorithm) + " checksum matched")
}

func validateChecksumConfig(c *ChecksumCondition, algorithm ChecksumAlgorithm, expected string) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("checksum path is required")
	}
	if _, err := checksumHash(algorithm); err != nil {
		return err
	}
	if expected == "" {
		return fmt.Errorf("checksum requires --equals")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("invalid checksum: %w", err)
	}
	if len(expected) != checksumHexLength(algorithm) {
		return fmt.Errorf("%s checksum must be %d hex characters", algorithm, checksumHexLength(algorithm))
	}
	return nil
}

func checksumHexLength(algorithm ChecksumAlgorithm) int {
	switch algorithm {
	case ChecksumSHA1:
		return sha1.Size * 2
	case ChecksumSHA256:
		return sha256.Size * 2
	case ChecksumSHA512:
		return sha512.Size * 2
	default:
		return 0
	}
}

func resolvedChecksum(c *ChecksumCondition) (ChecksumAlgorithm, string, error) {
	algorithm := c.Algorithm
	expected := normalizeChecksum(c.Expected)
	if prefix, digest, ok := strings.Cut(expected, ":"); ok {
		parsed := ChecksumAlgorithm(strings.ToLower(prefix))
		if algorithm != "" && algorithm != ChecksumAuto && algorithm != parsed {
			return "", "", fmt.Errorf("checksum algorithm %q does not match expected prefix %q", algorithm, parsed)
		}
		algorithm = parsed
		expected = digest
	}
	if algorithm == "" || algorithm == ChecksumAuto {
		inferred, err := inferChecksumAlgorithm(expected)
		if err != nil {
			return "", "", err
		}
		algorithm = inferred
	}
	return algorithm, expected, nil
}

func inferChecksumAlgorithm(expected string) (ChecksumAlgorithm, error) {
	switch len(expected) {
	case 40:
		return ChecksumSHA1, nil
	case 64:
		return ChecksumSHA256, nil
	case 128:
		return ChecksumSHA512, nil
	default:
		return "", fmt.Errorf("cannot infer checksum algorithm from %d hex characters", len(expected))
	}
}

func fileChecksum(ctx context.Context, path string, algorithm ChecksumAlgorithm) (string, error) {
	h, err := checksumHash(algorithm)
	if err != nil {
		return "", err
	}
	file, _, err := openRegularFile(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	if _, err := copyWithContext(ctx, h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checksumHash(algorithm ChecksumAlgorithm) (hash.Hash, error) {
	switch algorithm {
	case ChecksumSHA1:
		return sha1.New(), nil // #nosec G401 -- optional SHA-1 matching is for externally supplied legacy checksums.
	case ChecksumSHA256:
		return sha256.New(), nil
	case ChecksumSHA512:
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported checksum algorithm %q", algorithm)
	}
}

func normalizeChecksum(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return written, writeErr
			}
			written += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				return written, nil
			}
			return written, err
		}
	}
}
