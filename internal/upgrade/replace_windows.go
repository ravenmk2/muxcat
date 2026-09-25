//go:build windows

package upgrade

import (
	"os"
)

// platformReplace swaps target with newBinary on Windows: a running exe
// cannot be overwritten, but it can be renamed — so the current binary is
// moved aside to <target>.old first, then the new file is written. The .old
// file is kept as a backup (its path is returned) and cleaned up at the
// next upgrade's start.
func platformReplace(newBinary, target string) (string, error) {
	old := target + ".old"
	_ = os.Remove(old)
	if err := os.Rename(target, old); err != nil {
		return "", err
	}
	if err := copyFile(newBinary, target, 0o755); err != nil {
		// Restore the previous binary on failure.
		_ = os.Rename(old, target)
		return "", err
	}
	return old, nil
}
