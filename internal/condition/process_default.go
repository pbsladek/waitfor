//go:build !unix

package condition

import "context"

func defaultPIDExists(ctx context.Context, pid int) (bool, error) {
	processes, err := defaultProcessList(ctx)
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		if process.PID == pid {
			return true, nil
		}
	}
	return false, nil
}
