package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/ravenmk2/muxcat/internal/output"
)

// stubKeyring replaces the keyring with an unavailable one (always
// ErrNotFound), so tests only exercise the env path.
func stubKeyring(t *testing.T) {
	t.Helper()
	origGet, origSet := keyringGet, keyringSet
	keyringGet = func(_, _ string) (string, error) { return "", keyring.ErrNotFound }
	keyringSet = func(_, _, _ string) error { return errors.New("keyring unavailable in test") }
	t.Cleanup(func() { keyringGet, keyringSet = origGet, origSet })
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey(t)
	blob, err := Encrypt(key, []byte("s3cret-password"))
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}
	if !strings.HasPrefix(blob, Prefix) {
		t.Fatalf("cipher text %q missing %q prefix", blob, Prefix)
	}
	plain, err := Decrypt(key, blob)
	if err != nil {
		t.Fatalf("Decrypt() error: %v", err)
	}
	if string(plain) != "s3cret-password" {
		t.Fatalf("Decrypt() = %q, want %q", plain, "s3cret-password")
	}
}

func TestEncryptRandomNonce(t *testing.T) {
	key := testKey(t)
	a, _ := Encrypt(key, []byte("same"))
	b, _ := Encrypt(key, []byte("same"))
	if a == b {
		t.Fatal("two Encrypt calls on same plaintext should differ (random nonce)")
	}
}

func TestDecryptBadInput(t *testing.T) {
	key := testKey(t)
	if _, err := Decrypt(key, "not-a-muxcat-blob"); err == nil {
		t.Fatal("Decrypt() should reject blob without enc:v1: prefix")
	}
	if _, err := Decrypt(key, Prefix+"!!!not-base64!!!"); err == nil {
		t.Fatal("Decrypt() should reject invalid base64")
	}
	wrong := testKey(t)
	blob, _ := Encrypt(key, []byte("x"))
	if _, err := Decrypt(wrong, blob); err == nil {
		t.Fatal("Decrypt() with wrong key should fail")
	}
}

func TestMasterKeyEnv(t *testing.T) {
	stubKeyring(t)
	key := testKey(t)
	t.Setenv(envKey, base64.StdEncoding.EncodeToString(key))

	got, src, err := MasterKey()
	if err != nil {
		t.Fatalf("MasterKey() error: %v", err)
	}
	if src != SourceEnv {
		t.Fatalf("source = %q, want %q", src, SourceEnv)
	}
	if string(got) != string(key) {
		t.Fatal("MasterKey() returned wrong key bytes")
	}
}

func TestMasterKeyEnvInvalid(t *testing.T) {
	stubKeyring(t)
	t.Setenv(envKey, "!!!not-base64!!!")

	_, _, err := MasterKey()
	if err == nil {
		t.Fatal("MasterKey() should fail on invalid MUXCAT_KEY")
	}
	e := output.ToError(err)
	if e.Code != output.CodeKeyUnavailable {
		t.Fatalf("code = %s, want %s", e.Code, output.CodeKeyUnavailable)
	}
	if e.Hint == "" {
		t.Fatal("KEY_UNAVAILABLE should carry a hint")
	}

	// valid base64 but not 32 bytes long
	t.Setenv(envKey, base64.StdEncoding.EncodeToString([]byte("short")))
	if _, _, err := MasterKey(); err == nil {
		t.Fatal("MasterKey() should fail on short key")
	}
}

func TestMasterKeyUnavailable(t *testing.T) {
	stubKeyring(t)
	t.Setenv(envKey, "")

	_, _, err := MasterKey()
	if err == nil {
		t.Fatal("MasterKey() should fail when no source available")
	}
	e := output.ToError(err)
	if e.Code != output.CodeKeyUnavailable {
		t.Fatalf("code = %s, want %s", e.Code, output.CodeKeyUnavailable)
	}
	if !strings.Contains(e.Hint, "muxcat config key init") {
		t.Fatalf("hint %q should point to config key init", e.Hint)
	}
}

func TestProbeEnvOnly(t *testing.T) {
	stubKeyring(t)
	t.Setenv(envKey, base64.StdEncoding.EncodeToString(testKey(t)))

	st := Probe()
	if st.Source != SourceEnv || !st.EnvSet || !st.EnvValid {
		t.Fatalf("Probe() = %+v, want env source", st)
	}
	if st.KeychainHasKey {
		t.Fatal("keychain stubbed empty, KeychainHasKey should be false")
	}
}

func TestProbeNone(t *testing.T) {
	stubKeyring(t)
	t.Setenv(envKey, "")

	st := Probe()
	if st.Source != SourceNone {
		t.Fatalf("Probe().Source = %q, want empty", st.Source)
	}
}
