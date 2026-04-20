//go:build !(aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris)

package condition

import "os/exec"

func prepareExecCommand(_ *exec.Cmd) {}
