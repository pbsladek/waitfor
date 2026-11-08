package condition

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type ArchiveFormat string

const (
	maxArchiveEntries      = 100000
	maxArchiveScannedBytes = 512 * 1024 * 1024
	maxArchiveMemberBytes  = 512 * 1024 * 1024

	ArchiveAuto ArchiveFormat = "auto"
	ArchiveTar  ArchiveFormat = "tar"
	ArchiveTgz  ArchiveFormat = "tgz"
	ArchiveZip  ArchiveFormat = "zip"
)

type ArchiveCondition struct {
	Path    string
	Member  string
	Matches string
	Format  ArchiveFormat
}

func NewArchive(path string) *ArchiveCondition {
	return &ArchiveCondition{Path: path, Format: ArchiveAuto}
}

func (c *ArchiveCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "archive", Target: c.Path}
}

func (c *ArchiveCondition) Check(ctx context.Context) Result {
	if err := validateArchiveConfig(c); err != nil {
		return Fatal(err)
	}
	found, err := archiveContains(ctx, c)
	if err != nil {
		return Unsatisfied("", err)
	}
	if !found {
		return Unsatisfied("archive member not found", fmt.Errorf("archive member not found"))
	}
	return Satisfied("archive contains " + c.Member)
}

func validateArchiveConfig(c *ArchiveCondition) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("archive path is required")
	}
	if strings.TrimSpace(c.Member) == "" && strings.TrimSpace(c.Matches) == "" {
		return fmt.Errorf("archive requires --contains or --matches")
	}
	if strings.TrimSpace(c.Member) != "" && strings.TrimSpace(c.Matches) != "" {
		return fmt.Errorf("--contains and --matches are mutually exclusive")
	}
	switch c.Format {
	case ArchiveAuto, ArchiveTar, ArchiveTgz, ArchiveZip:
	default:
		return fmt.Errorf("unsupported archive format %q", c.Format)
	}
	if strings.TrimSpace(c.Matches) != "" {
		if _, err := filepath.Match(cleanArchiveName(c.Matches), ""); err != nil {
			return fmt.Errorf("invalid archive glob: %w", err)
		}
	}
	return nil
}

func archiveContains(ctx context.Context, c *ArchiveCondition) (bool, error) {
	resolved := resolveArchiveFormat(c.Path, c.Format)
	switch resolved {
	case ArchiveZip:
		return zipContains(ctx, c)
	case ArchiveTar:
		return tarContains(ctx, c, false)
	case ArchiveTgz:
		return tarContains(ctx, c, true)
	default:
		return false, fmt.Errorf("unsupported archive format %q", resolved)
	}
}

func resolveArchiveFormat(path string, format ArchiveFormat) ArchiveFormat {
	if format != ArchiveAuto {
		return format
	}
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".zip") {
		return ArchiveZip
	}
	if strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz") {
		return ArchiveTgz
	}
	return ArchiveTar
}

func zipContains(ctx context.Context, c *ArchiveCondition) (bool, error) {
	file, info, err := openRegularFile(c.Path)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	if info.Size() > maxArchiveScannedBytes {
		return false, fmt.Errorf("archive file exceeds scan limit")
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return false, err
	}
	if len(reader.File) > maxArchiveEntries {
		return false, fmt.Errorf("archive has too many entries")
	}
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if archiveMemberMatches(file.Name, c.Member, c.Matches) {
			return true, nil
		}
	}
	return false, nil
}

func tarContains(ctx context.Context, c *ArchiveCondition, gzipped bool) (bool, error) {
	file, _, err := openRegularFile(c.Path)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	if gzipped {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return false, err
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	return tarReaderContains(ctx, tar.NewReader(reader), c)
}

func tarReaderContains(ctx context.Context, reader *tar.Reader, c *ArchiveCondition) (bool, error) {
	entries := 0
	var scannedBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		header, err := reader.Next()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		entries++
		if err := validateArchiveHeaderBudget(header, entries, scannedBytes); err != nil {
			return false, err
		}
		scannedBytes += header.Size
		if archiveMemberMatches(header.Name, c.Member, c.Matches) {
			return true, nil
		}
	}
}

func validateArchiveHeaderBudget(header *tar.Header, entries int, scannedBytes int64) error {
	if entries > maxArchiveEntries {
		return fmt.Errorf("archive has too many entries")
	}
	if header.Size < 0 || header.Size > maxArchiveMemberBytes {
		return fmt.Errorf("archive member content exceeds scan limit")
	}
	if scannedBytes > maxArchiveScannedBytes-header.Size {
		return fmt.Errorf("archive member content exceeds scan limit")
	}
	return nil
}

func archiveMemberMatches(name, exact, pattern string) bool {
	cleaned := cleanArchiveName(name)
	if exact != "" {
		return cleaned == cleanArchiveName(exact)
	}
	matched, err := filepath.Match(cleanArchiveName(pattern), cleaned)
	return err == nil && matched
}

func cleanArchiveName(name string) string {
	return filepath.Clean(strings.TrimPrefix(name, "/"))
}
