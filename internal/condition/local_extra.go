package condition

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // SHA-1 is supported only for matching externally supplied artifact hashes.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type LockfileState string

const (
	LockfilePresent LockfileState = "present"
	LockfileAbsent  LockfileState = "absent"
)

type ChecksumAlgorithm string

const (
	ChecksumAuto   ChecksumAlgorithm = "auto"
	ChecksumSHA1   ChecksumAlgorithm = "sha1"
	ChecksumSHA256 ChecksumAlgorithm = "sha256"
	ChecksumSHA512 ChecksumAlgorithm = "sha512"
)

type PermissionPathType string

const (
	PermissionAny     PermissionPathType = "any"
	PermissionFile    PermissionPathType = "file"
	PermissionDir     PermissionPathType = "dir"
	PermissionSymlink PermissionPathType = "symlink"
)

type ArchiveFormat string

const (
	maxPIDFileBytes        = 4096
	maxArchiveEntries      = 100000
	maxArchiveScannedBytes = 512 * 1024 * 1024
	maxArchiveMemberBytes  = 512 * 1024 * 1024

	ArchiveAuto ArchiveFormat = "auto"
	ArchiveTar  ArchiveFormat = "tar"
	ArchiveTgz  ArchiveFormat = "tgz"
	ArchiveZip  ArchiveFormat = "zip"
)

type PIDFileCondition struct {
	Path      string
	State     ProcessState
	PIDExists func(context.Context, int) (bool, error)
}

type LockfileCondition struct {
	Path      string
	State     LockfileState
	OlderThan time.Duration
}

type PermissionCondition struct {
	Path string
	Mode os.FileMode
	UID  *int
	GID  *int
	Type PermissionPathType
}

type ChecksumCondition struct {
	Path      string
	Algorithm ChecksumAlgorithm
	Expected  string
}

type ArchiveCondition struct {
	Path    string
	Member  string
	Matches string
	Format  ArchiveFormat
}

func NewPIDFile(path string) *PIDFileCondition {
	return &PIDFileCondition{Path: path, State: ProcessRunning}
}

func NewLockfile(path string) *LockfileCondition {
	return &LockfileCondition{Path: path, State: LockfileAbsent}
}

func NewPermission(path string) *PermissionCondition {
	return &PermissionCondition{Path: path, Type: PermissionAny}
}

func NewChecksum(path string) *ChecksumCondition {
	return &ChecksumCondition{Path: path, Algorithm: ChecksumAuto}
}

func NewArchive(path string) *ArchiveCondition {
	return &ArchiveCondition{Path: path, Format: ArchiveAuto}
}

func (c *PIDFileCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "pidfile", Target: c.Path}
}

func (c *LockfileCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "lockfile", Target: c.Path}
}

func (c *PermissionCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "permission", Target: c.Path}
}

func (c *ChecksumCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "checksum", Target: c.Path}
}

func (c *ArchiveCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "archive", Target: c.Path}
}

func (c *PIDFileCondition) Check(ctx context.Context) Result {
	if err := validatePIDFileConfig(c); err != nil {
		return Fatal(err)
	}
	pid, err := readPIDFile(ctx, c.Path)
	if err != nil {
		return checkPIDFileReadError(err, c.State)
	}
	exists, err := c.pidExists(ctx, pid)
	if err != nil {
		return Unsatisfied("", err)
	}
	return checkProcessFound(exists, c.State, fmt.Sprintf("pidfile pid %d", pid))
}

func (c *LockfileCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	if err := validateLockfileConfig(c); err != nil {
		return Fatal(err)
	}
	info, err := os.Lstat(c.Path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return Unsatisfied("", err)
	}
	return checkLockfileExists(exists, info, c)
}

func (c *PermissionCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	if err := validatePermissionConfig(c); err != nil {
		return Fatal(err)
	}
	info, err := os.Lstat(c.Path)
	if err != nil {
		return Unsatisfied("", err)
	}
	return checkPermissions(c, info)
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

func validatePIDFileConfig(c *PIDFileCondition) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("pidfile path is required")
	}
	switch c.State {
	case ProcessRunning, ProcessStopped:
		return nil
	default:
		return fmt.Errorf("unsupported pidfile state %q", c.State)
	}
}

func validateLockfileConfig(c *LockfileCondition) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("lockfile path is required")
	}
	switch c.State {
	case LockfilePresent, LockfileAbsent:
	default:
		return fmt.Errorf("unsupported lockfile state %q", c.State)
	}
	if c.OlderThan < 0 {
		return fmt.Errorf("--older-than must be non-negative")
	}
	if c.OlderThan > 0 && c.State != LockfilePresent {
		return fmt.Errorf("--older-than requires --present")
	}
	return nil
}

func validatePermissionConfig(c *PermissionCondition) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("permission path is required")
	}
	switch c.Type {
	case PermissionAny, PermissionFile, PermissionDir, PermissionSymlink:
	default:
		return fmt.Errorf("unsupported permission type %q", c.Type)
	}
	if c.Mode == 0 && c.UID == nil && c.GID == nil && c.Type == PermissionAny {
		return fmt.Errorf("permission requires --mode, --uid, --gid, or --type")
	}
	if c.Mode&^os.ModePerm != 0 {
		return fmt.Errorf("permission mode must contain permission bits only")
	}
	return nil
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

func readPIDFile(ctx context.Context, path string) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	body, err := readPIDFileLimit(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pidfile does not contain a positive pid")
	}
	return pid, nil
}

func readPIDFileLimit(path string) ([]byte, error) {
	file, _, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxPIDFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPIDFileBytes {
		return nil, fmt.Errorf("pidfile exceeds %d bytes", maxPIDFileBytes)
	}
	return body, nil
}

func checkPIDFileReadError(err error, want ProcessState) Result {
	if errorsIsNotExist(err) && want == ProcessStopped {
		return Satisfied("pidfile is absent")
	}
	if errorsIsNotExist(err) {
		return Unsatisfied("pidfile does not exist", err)
	}
	return Unsatisfied("", err)
}

func (c *PIDFileCondition) pidExists(ctx context.Context, pid int) (bool, error) {
	if c.PIDExists != nil {
		return c.PIDExists(ctx, pid)
	}
	return defaultPIDExists(ctx, pid)
}

func errorsIsNotExist(err error) bool {
	return os.IsNotExist(err)
}

func checkLockfileExists(exists bool, info os.FileInfo, c *LockfileCondition) Result {
	if exists && c.State == LockfilePresent {
		return checkLockfileAge(info, c.OlderThan)
	}
	if !exists && c.State == LockfileAbsent {
		return Satisfied("lockfile is absent")
	}
	if c.State == LockfilePresent {
		return Unsatisfied("lockfile does not exist", fmt.Errorf("lockfile does not exist"))
	}
	return Unsatisfied("lockfile still exists", fmt.Errorf("lockfile still exists"))
}

func checkLockfileAge(info os.FileInfo, olderThan time.Duration) Result {
	if olderThan == 0 {
		return Satisfied("lockfile exists")
	}
	age := time.Since(info.ModTime())
	if age >= olderThan {
		return Satisfied(fmt.Sprintf("lockfile age %s is at least %s", age.Round(time.Millisecond), olderThan))
	}
	return Unsatisfied("lockfile is not old enough", fmt.Errorf("lockfile age %s is less than %s", age.Round(time.Millisecond), olderThan))
}

func checkPermissions(c *PermissionCondition, info os.FileInfo) Result {
	if result := checkPermissionType(c.Type, info.Mode()); result.Status != CheckSatisfied {
		return result
	}
	if c.Mode != 0 && info.Mode().Perm() != c.Mode {
		return Unsatisfied("mode mismatch", fmt.Errorf("mode %04o, expected %04o", info.Mode().Perm(), c.Mode))
	}
	return checkPermissionOwner(c, info)
}

func checkPermissionOwner(c *PermissionCondition, info os.FileInfo) Result {
	uid, gid, ok := fileOwnerIDs(info)
	if (c.UID != nil || c.GID != nil) && !ok {
		return Fatal(fmt.Errorf("owner checks are not supported on this platform"))
	}
	if c.UID != nil && uid != *c.UID {
		return Unsatisfied("uid mismatch", fmt.Errorf("uid %d, expected %d", uid, *c.UID))
	}
	if c.GID != nil && gid != *c.GID {
		return Unsatisfied("gid mismatch", fmt.Errorf("gid %d, expected %d", gid, *c.GID))
	}
	return Satisfied("permissions matched")
}

func checkPermissionType(want PermissionPathType, mode os.FileMode) Result {
	switch want {
	case PermissionAny:
		return Satisfied("path type matched")
	case PermissionFile:
		return checkPermissionTypeMatch(mode.IsRegular(), "file")
	case PermissionDir:
		return checkPermissionTypeMatch(mode.IsDir(), "directory")
	case PermissionSymlink:
		return checkPermissionTypeMatch(mode&os.ModeSymlink != 0, "symlink")
	default:
		return Fatal(fmt.Errorf("unsupported permission type %q", want))
	}
}

func checkPermissionTypeMatch(ok bool, name string) Result {
	if ok {
		return Satisfied("path is " + name)
	}
	return Unsatisfied("path is not "+name, fmt.Errorf("path is not %s", name))
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
