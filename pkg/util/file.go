package util

import (
	"os"
)

// FileExists reports whether a regular file exists at filename. every stat
// error - not just not-exist - reports false: an unstatable path (a symlink
// loop, an unreadable directory) cannot be reported as an existing file, and
// the kubeconfig-detection callers fall back to their in-cluster config path
// on false, which is the safe outcome for an unreadable kubeconfig.
func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
