package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravenmk2/muxcat/internal/output"
)

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v0.2.0":   "0.2.0",
		"0.2.0":    "0.2.0",
		"  v1.0.0": "1.0.0",
		"dev":      "dev",
	} {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0},
		{"v0.2.0", "0.2.0", 0},
		{"0.1.9", "0.2.0", -1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.9.9", 1},
	}
	for _, c := range cases {
		got, err := compareVersions(c.a, c.b)
		if err != nil {
			t.Fatalf("compareVersions(%q, %q): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"dev", "1.0", "1.0.0-rc1", "a.b.c"} {
		if _, err := compareVersions(bad, "0.2.0"); err == nil {
			t.Errorf("compareVersions(%q, ...) should fail", bad)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	data := "aaa111  muxcat_0.2.0_linux_amd64.tar.gz\n" +
		"bbb222\t muxcat_0.2.0_windows_amd64.zip\n" +
		"\n" +
		"ccc333  muxcat_0.2.0_darwin_arm64.tar.gz\n"
	sums := parseChecksums(data)
	if len(sums) != 3 {
		t.Fatalf("parsed %d entries, want 3: %v", len(sums), sums)
	}
	if sums["muxcat_0.2.0_windows_amd64.zip"] != "bbb222" {
		t.Errorf("whitespace-separated entry not parsed: %v", sums)
	}
}

// makeArchive builds a release archive (tar.gz or zip per GOOS) containing
// a single muxcat binary with the given content.
func makeArchive(t *testing.T, goos string, content []byte) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	name := BinaryName(goos)
	if goos == "windows" {
		zw := zip.NewWriter(buf)
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testServer serves a fake GitHub: release metadata, the platform asset
// (content "binary-<version>"), and checksums.txt. failFirst makes the
// asset endpoint return 500 for that many requests before succeeding.
func testServer(t *testing.T, version string, failFirst *atomic.Int64) (*httptest.Server, string) {
	t.Helper()
	goos, goarch := runtime.GOOS, runtime.GOARCH
	assetName := AssetName(version, goos, goarch)
	asset := makeArchive(t, goos, []byte("binary-"+version))
	sum := sha256.Sum256(asset)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), assetName)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/ravenmk2/muxcat/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"tag_name":"v%s"}`, version)
	})
	mux.HandleFunc("/api/repos/ravenmk2/muxcat/releases/tags/v"+version, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"tag_name":"v%s"}`, version)
	})
	mux.HandleFunc("/dl/ravenmk2/muxcat/releases/download/v"+version+"/"+assetName, func(w http.ResponseWriter, _ *http.Request) {
		if failFirst != nil && failFirst.Add(-1) >= 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(asset)))
		_, _ = w.Write(asset)
	})
	mux.HandleFunc("/dl/ravenmk2/muxcat/releases/download/v"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	return httptest.NewServer(mux), assetName
}

func testClient(srv *httptest.Server) *Client {
	return &Client{
		APIBase:      srv.URL + "/api",
		DownloadBase: srv.URL + "/dl",
		HTTP:         srv.Client(),
		Backoff:      func(int) time.Duration { return time.Millisecond },
	}
}

func TestResolveLatest(t *testing.T) {
	srv, assetName := testServer(t, "0.2.0", nil)
	defer srv.Close()
	plan, err := Resolve(t.Context(), testClient(srv), Options{Current: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.UpToDate || plan.To != "0.2.0" || plan.Tag != "v0.2.0" || plan.Asset != assetName {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestResolveDevIsUpgradable(t *testing.T) {
	srv, _ := testServer(t, "0.2.0", nil)
	defer srv.Close()
	plan, err := Resolve(t.Context(), testClient(srv), Options{Current: DevVersion})
	if err != nil {
		t.Fatal(err)
	}
	if plan.UpToDate {
		t.Fatalf("dev build must be upgradable: %+v", plan)
	}
}

func TestResolveUpToDate(t *testing.T) {
	srv, _ := testServer(t, "0.2.0", nil)
	defer srv.Close()
	plan, err := Resolve(t.Context(), testClient(srv), Options{Current: "v0.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.UpToDate || plan.Newer {
		t.Fatalf("same version must be up to date: %+v", plan)
	}
}

func TestResolveNewerThanLatest(t *testing.T) {
	srv, _ := testServer(t, "0.2.0", nil)
	defer srv.Close()
	plan, err := Resolve(t.Context(), testClient(srv), Options{Current: "0.9.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.UpToDate || !plan.Newer {
		t.Fatalf("newer current version must not downgrade: %+v", plan)
	}
}

func TestResolvePinnedOlderVersionAllowed(t *testing.T) {
	srv, _ := testServer(t, "0.2.0", nil)
	defer srv.Close()
	plan, err := Resolve(t.Context(), testClient(srv), Options{Current: "0.9.0", Version: "0.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.UpToDate {
		t.Fatalf("pinned older version must be honored: %+v", plan)
	}
}

func TestApply(t *testing.T) {
	srv, _ := testServer(t, "0.2.0", nil)
	defer srv.Close()
	target := filepath.Join(t.TempDir(), BinaryName(runtime.GOOS))
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := testClient(srv)
	plan, err := Resolve(t.Context(), client, Options{Current: DevVersion})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Apply(t.Context(), client, plan, Options{Current: DevVersion, Attempts: 3, Target: target}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.InstalledTo != target || res.To != "0.2.0" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if runtime.GOOS == "windows" {
		if res.Backup != target+".old" {
			t.Fatalf("backup = %q, want %q", res.Backup, target+".old")
		}
	} else if res.Backup != "" {
		t.Fatalf("unix replace must not produce a backup, got %q", res.Backup)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "binary-0.2.0" {
		t.Fatalf("target content = %q, want the new binary", got)
	}
}

func TestDownloadRetriesThenSucceeds(t *testing.T) {
	var fails atomic.Int64
	fails.Store(2)
	srv, assetName := testServer(t, "0.2.0", &fails)
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "asset")
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rel := &Release{Tag: "v0.2.0", Version: "0.2.0"}
	if err := testClient(srv).DownloadAsset(t.Context(), rel, assetName, f, 5, nil); err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
}

func TestDownloadAttemptsExhausted(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := testClient(srv)
	client.APIBase = srv.URL
	client.DownloadBase = srv.URL
	dest := filepath.Join(t.TempDir(), "asset")
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rel := &Release{Tag: "v0.2.0", Version: "0.2.0"}
	err = client.DownloadAsset(t.Context(), rel, "x", f, 3, nil)
	if err == nil || !strings.Contains(err.Error(), "3 attempt(s)") {
		t.Fatalf("error = %v, want mention of 3 attempts", err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server hits = %d, want 3", got)
	}
}

func TestDownloadNoRetryOn404(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := testClient(srv)
	client.DownloadBase = srv.URL
	dest := filepath.Join(t.TempDir(), "asset")
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rel := &Release{Tag: "v9.9.9", Version: "9.9.9"}
	err = client.DownloadAsset(t.Context(), rel, "x", f, 10, nil)
	if err == nil || !strings.Contains(err.Error(), "1 attempt(s)") {
		t.Fatalf("error = %v, want failure after 1 actual attempt", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 (no retry on 404)", got)
	}
}

// A stalled attempt (no data within IdleTimeout) must be aborted and
// retried; the next attempt succeeds.
func TestDownloadStallRetries(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// First attempt: never send anything; wait for the client to
			// give up (watchdog cancels the request context).
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	client := testClient(srv)
	client.DownloadBase = srv.URL
	client.IdleTimeout = 10 * time.Millisecond
	dest := filepath.Join(t.TempDir(), "asset")
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rel := &Release{Tag: "v0.2.0", Version: "0.2.0"}
	if err := client.DownloadAsset(t.Context(), rel, "x", f, 3, nil); err != nil {
		t.Fatalf("stalled attempt must be retried and succeed: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (stall then success)", got)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 7)
	if _, err := f.Read(got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Fatalf("content = %q, want payload", got)
	}
}

func TestVerifyFileMismatch(t *testing.T) {
	dir := t.TempDir()
	asset := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(asset, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyFile(asset, map[string]string{"a.tar.gz": strings.Repeat("0", 64)}, "a.tar.gz")
	e := output.ToError(err)
	if e.Code != output.CodeUpgradeFailed {
		t.Fatalf("error code = %s, want UPGRADE_FAILED", e.Code)
	}
	if err := verifyFile(asset, map[string]string{}, "a.tar.gz"); err == nil {
		t.Fatal("missing checksum entry must fail")
	}
}

func TestPlatformReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, BinaryName(runtime.GOOS))
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "new")
	if err := os.WriteFile(newBin, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup, err := platformReplace(newBin, target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target content = %q, want new", got)
	}
	if runtime.GOOS == "windows" {
		if backup != target+".old" {
			t.Fatalf("backup = %q, want %q", backup, target+".old")
		}
		old, err := os.ReadFile(backup)
		if err != nil || string(old) != "old" {
			t.Fatalf("backup content = %q, %v; want the previous binary", old, err)
		}
	} else if backup != "" {
		t.Fatalf("unix replace must not produce a backup, got %q", backup)
	}
}
