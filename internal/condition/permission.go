package condition

import (
	"context"
	"fmt"
	"os"
	"strings"
)

type PermissionPathType string

const (
	PermissionAny     PermissionPathType = "any"
	PermissionFile    PermissionPathType = "file"
	PermissionDir     PermissionPathType = "dir"
	PermissionSymlink PermissionPathType = "symlink"
)

type PermissionCondition struct {
	Path string
	Mode os.FileMode
	UID  *int
	GID  *int
	Type PermissionPathType
}

func NewPermission(path string) *PermissionCondition {
	return &PermissionCondition{Path: path, Type: PermissionAny}
}

func (c *PermissionCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "permission", Target: c.Path}
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
