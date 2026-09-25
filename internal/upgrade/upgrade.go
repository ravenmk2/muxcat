package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ravenmk2/muxcat/internal/output"
)

// Options configures an upgrade run.
type Options struct {
	// Current is the running binary's version ("dev" for local builds).
	Current string
	// Version pins the target version (with or without "v"); empty means
	// the latest release.
	Version string
	// Attempts is the per-asset download retry budget (minimum 1).
	Attempts int
	// GOOS/GOARCH select the release asset; empty means the runtime values.
	GOOS   string
	GOARCH string
	// Target overrides the binary path to replace; empty resolves
	// os.Executable(). Tests use this to avoid touching the test runner.
	Target string
}

func (o *Options) platform() (goos, goarch string) {
	goos, goarch = o.GOOS, o.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// Plan is the resolved upgrade decision: what would change, or that the
// binary is already current.
type Plan struct {
	From     string // current version, as reported
	To       string // target version without the leading "v"
	Tag      string // release tag with the leading "v"
	Asset    string // asset file name for this platform
	UpToDate bool   // nothing to do (includes running newer than latest)
	Newer    bool   // current version is newer than the latest release
}

// Resolve queries GitHub and decides what an upgrade would do. It performs
// no downloads beyond the release metadata.
func Resolve(ctx context.Context, c *Client, opts Options) (*Plan, error) {
	goos, goarch := opts.platform()
	var rel *Release
	var err error
	pinned := opts.Version != ""
	if pinned {
		rel, err = c.ReleaseByTag(ctx, opts.Version)
	} else {
		rel, err = c.LatestRelease(ctx)
	}
	if err != nil {
		return nil, err
	}
	p := &Plan{
		From:  opts.Current,
		To:    rel.Version,
		Tag:   rel.Tag,
		Asset: AssetName(rel.Version, goos, goarch),
	}
	if opts.Current != DevVersion {
		cur := normalizeVersion(opts.Current)
		if cur == rel.Version {
			p.UpToDate = true
			return p, nil
		}
		// Guard against silently downgrading past the latest release; an
		// explicitly pinned older version is honored.
		if !pinned {
			if cmp, err := compareVersions(cur, rel.Version); err == nil && cmp > 0 {
				p.UpToDate = true
				p.Newer = true
			}
		}
	}
	return p, nil
}

// Result reports a completed upgrade.
type Result struct {
	From        string
	To          string
	Asset       string
	InstalledTo string
	// Backup is the path of the previous binary when the platform keeps
	// one (Windows .old); empty when there is no backup (Unix).
	Backup string
}

// Apply executes a resolved plan: download the asset and checksums with
// retries, verify the SHA256, extract the binary, and replace the running
// executable. onProgress (may be nil) reports asset download progress.
func Apply(ctx context.Context, c *Client, p *Plan, opts Options, onProgress func(received, total int64)) (*Result, error) {
	target := opts.Target
	if target == "" {
		var err error
		target, err = executablePath()
		if err != nil {
			return nil, err
		}
	}
	if err := probeWritable(filepath.Dir(target)); err != nil {
		return nil, err
	}
	cleanupStale(target)

	tmp, err := os.MkdirTemp("", "muxcat-upgrade-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	assetPath := filepath.Join(tmp, p.Asset)
	assetFile, err := os.Create(assetPath)
	if err != nil {
		return nil, err
	}
	if err := c.DownloadAsset(ctx, &Release{Tag: p.Tag, Version: p.To}, p.Asset, assetFile, opts.Attempts, onProgress); err != nil {
		_ = assetFile.Close()
		return nil, err
	}
	if err := assetFile.Close(); err != nil {
		return nil, err
	}

	sumsPath := filepath.Join(tmp, "checksums.txt")
	sumsFile, err := os.Create(sumsPath)
	if err != nil {
		return nil, err
	}
	if err := c.DownloadAsset(ctx, &Release{Tag: p.Tag, Version: p.To}, "checksums.txt", sumsFile, opts.Attempts, nil); err != nil {
		_ = sumsFile.Close()
		return nil, err
	}
	if err := sumsFile.Close(); err != nil {
		return nil, err
	}
	sumsData, err := os.ReadFile(sumsPath)
	if err != nil {
		return nil, err
	}
	if err := verifyFile(assetPath, parseChecksums(string(sumsData)), p.Asset); err != nil {
		return nil, err
	}

	goos, _ := opts.platform()
	binary, err := extractBinary(assetPath, tmp, goos)
	if err != nil {
		return nil, err
	}
	backup, err := platformReplace(binary, target)
	if err != nil {
		return nil, output.NewError(output.CodeUpgradeFailed,
			"failed to replace "+target+": "+err.Error(),
			"check write permissions on the install directory")
	}
	return &Result{From: p.From, To: p.To, Asset: p.Asset, InstalledTo: target, Backup: backup}, nil
}
