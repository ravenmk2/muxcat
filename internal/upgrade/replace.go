package upgrade

import (
	"io"
	"os"
	"path/filepath"

	"github.com/ravenmk2/muxcat/internal/output"
)

// executablePath resolves the real path of the running binary, following
// symlinks so the replacement lands on the actual file.
func executablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		p = rp
	}
	return p, nil
}

// TargetPath resolves the binary path an upgrade would replace.
func TargetPath() (string, error) {
	return executablePath()
}

// probeWritable verifies the target directory allows creating files.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".muxcat-probe-*")
	if err != nil {
		return output.NewError(output.CodeUpgradeFailed,
			"no write permission on "+dir+": "+err.Error(),
			"install to a user-writable directory (e.g. ~/.local/bin) or re-run with elevated privileges")
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return nil
}

// cleanupStale removes a leftover <binary>.old from a previous Windows
// upgrade. Best-effort; errors are ignored.
func cleanupStale(target string) {
	_ = os.Remove(target + ".old")
}

// copyFile copies src to dst with the given permissions.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
