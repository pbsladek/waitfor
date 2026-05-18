//go:build !unix

package condition

import "os"

func fileOwnerIDs(os.FileInfo) (int, int, bool) {
	return 0, 0, false
}
