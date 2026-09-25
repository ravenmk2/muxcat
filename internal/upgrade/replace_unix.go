//go:build !windows

package upgrade

import (
	"os"
	"path/filepath"
)

// platformReplace atomically swaps target with newBinary: write a temp file
// in the same directory, then rename over the target. The old binary is
// replaced outright, so there is no backup path.
func platformReplace(newBinary, target string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".muxcat-new-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := copyFile(newBinary, tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return "", nil
}
