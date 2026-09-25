package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ravenmk2/muxcat/internal/output"
)

// AssetName builds the release asset file name for a platform, matching the
// goreleaser name_template (version without the leading "v"; Windows ships
// zip, everything else tar.gz).
func AssetName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("muxcat_%s_%s_%s%s", normalizeVersion(version), goos, goarch, ext)
}

// BinaryName is the executable name inside the archive.
func BinaryName(goos string) string {
	if goos == "windows" {
		return "muxcat.exe"
	}
	return "muxcat"
}

// parseChecksums parses a goreleaser checksums.txt into name → hex sha256.
// Lines look like "<64 hex chars>  <filename>".
func parseChecksums(data string) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sums[fields[len(fields)-1]] = fields[0]
	}
	return sums
}

// verifyFile checks the file at path against its checksums.txt entry.
func verifyFile(path string, sums map[string]string, name string) error {
	want, ok := sums[name]
	if !ok {
		return output.NewError(output.CodeUpgradeFailed,
			"checksums.txt has no entry for "+name, "the release may be incomplete; retry later")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return output.NewError(output.CodeUpgradeFailed,
			fmt.Sprintf("checksum mismatch for %s (got %s, want %s)", name, got, want),
			"the download may be corrupted; retry the upgrade")
	}
	return nil
}

// extractBinary unpacks the muxcat executable from a release archive
// (tar.gz or zip, chosen by file extension) into destDir and returns its
// path with executable bits set.
func extractBinary(archivePath, destDir, goos string) (string, error) {
	want := BinaryName(goos)
	var out string
	var err error
	if strings.HasSuffix(archivePath, ".zip") {
		out, err = extractFromZip(archivePath, destDir, want)
	} else {
		out, err = extractFromTarGz(archivePath, destDir, want)
	}
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", output.NewError(output.CodeUpgradeFailed,
			"archive does not contain "+want, "the release asset layout may have changed")
	}
	if err := os.Chmod(out, 0o755); err != nil {
		return "", err
	}
	return out, nil
}

func extractFromTarGz(archivePath, destDir, want string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == want {
			return writeExtracted(filepath.Join(destDir, want), tr)
		}
	}
}

func extractFromZip(archivePath, destDir, want string) (string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = zr.Close() }()
	for _, zf := range zr.File {
		if filepath.Base(zf.Name) != want {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		out, werr := writeExtracted(filepath.Join(destDir, want), rc)
		_ = rc.Close()
		return out, werr
	}
	return "", nil
}

func writeExtracted(dst string, r io.Reader) (string, error) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return dst, nil
}
