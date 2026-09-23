// Package secret manages muxcat's master key and secret encryption.
//
// The master key is resolved in order: OS keychain (service=muxcat,
// key=master-key) → MUXCAT_KEY environment variable (base64-encoded 32
// bytes) → KEY_UNAVAILABLE error. Encryption is AES-256-GCM; ciphertext
// format: enc:v1: + base64(nonce|ciphertext).
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/ravenmk2/muxcat/internal/output"
)

const (
	keyringService = "muxcat"
	keyringKey     = "master-key"
	envKey         = "MUXCAT_KEY"

	// Prefix is the ciphertext version prefix.
	Prefix = "enc:v1:"
	// KeySize is the master key length in bytes (AES-256).
	KeySize = 32
)

// keyring functions as package-level variables so tests can replace them
// (CI / keychain-less environments).
var (
	keyringGet = keyring.Get
	keyringSet = keyring.Set
)

// Source identifies where the master key comes from.
type Source string

const (
	SourceNone     Source = ""
	SourceKeychain Source = "keychain"
	SourceEnv      Source = "env"
)

// MasterKey resolves the master key: keychain → MUXCAT_KEY →
// KEY_UNAVAILABLE. The returned key is held as []byte.
func MasterKey() ([]byte, Source, error) {
	if s, err := keyringGet(keyringService, keyringKey); err == nil {
		key, derr := base64.StdEncoding.DecodeString(s)
		if derr != nil || len(key) != KeySize {
			return nil, SourceKeychain, output.NewError(output.CodeKeyUnavailable,
				"master key in keychain has invalid format", "run muxcat config key init again")
		}
		return key, SourceKeychain, nil
	}
	if v := os.Getenv(envKey); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(key) != KeySize {
			return nil, SourceEnv, output.NewError(output.CodeKeyUnavailable,
				envKey+" is not a valid base64-encoded 32-byte key",
				"example: head -c 32 /dev/urandom | base64")
		}
		return key, SourceEnv, nil
	}
	return nil, SourceNone, output.NewError(output.CodeKeyUnavailable,
		"master key unavailable", "run muxcat config key init to generate one, or set the "+envKey+" environment variable")
}

// InitKey generates a random 32-byte master key and stores it in the
// keychain. An existing key is only overwritten when overwrite is true.
func InitKey(overwrite bool) error {
	_, err := keyringGet(keyringService, keyringKey)
	switch {
	case err == nil && !overwrite:
		return output.NewError(output.CodeMissingArgument,
			"a master key already exists in the keychain", "pass --yes to overwrite it")
	case err != nil && !errors.Is(err, keyring.ErrNotFound):
		return output.NewError(output.CodeKeyUnavailable,
			"system keychain unavailable: "+err.Error(), "use the "+envKey+" environment variable instead")
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return output.NewError(output.CodeKeyUnavailable, "failed to generate random key: "+err.Error(), "")
	}
	if err := keyringSet(keyringService, keyringKey, base64.StdEncoding.EncodeToString(key)); err != nil {
		return output.NewError(output.CodeKeyUnavailable,
			"cannot write to system keychain: "+err.Error(), "use the "+envKey+" environment variable instead")
	}
	return nil
}

// Status describes the master key's source and availability; it never
// contains the key itself.
type Status struct {
	Source            Source `json:"source"`
	KeychainAvailable bool   `json:"keychain_available"`
	KeychainHasKey    bool   `json:"keychain_has_key"`
	EnvSet            bool   `json:"env_set"`
	EnvValid          bool   `json:"env_valid"`
}

// Probe detects the master key's source and availability.
func Probe() Status {
	var st Status
	if s, err := keyringGet(keyringService, keyringKey); err == nil {
		st.KeychainAvailable = true
		if k, derr := base64.StdEncoding.DecodeString(s); derr == nil && len(k) == KeySize {
			st.KeychainHasKey = true
		}
	} else if errors.Is(err, keyring.ErrNotFound) {
		st.KeychainAvailable = true
	}
	if v := os.Getenv(envKey); v != "" {
		st.EnvSet = true
		if k, err := base64.StdEncoding.DecodeString(v); err == nil && len(k) == KeySize {
			st.EnvValid = true
		}
	}
	switch {
	case st.KeychainHasKey:
		st.Source = SourceKeychain
	case st.EnvValid:
		st.Source = SourceEnv
	}
	return st
}

// Encrypt encrypts with AES-256-GCM and returns
// enc:v1: + base64(nonce|ciphertext).
func Encrypt(key, plain []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plain, nil)
	return Prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt decrypts ciphertext produced by Encrypt.
func Decrypt(key []byte, blob string) ([]byte, error) {
	if !strings.HasPrefix(blob, Prefix) {
		return nil, output.NewError(output.CodeConfigInvalid,
			"ciphertext is missing the "+Prefix+" prefix; not a muxcat-encrypted value", "")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, Prefix))
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, "ciphertext base64 decode failed: "+err.Error(), "")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, output.NewError(output.CodeConfigInvalid, "ciphertext has invalid length", "")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, "decryption failed: "+err.Error(), "the key may not match")
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, output.NewError(output.CodeKeyUnavailable, "key must be 32 bytes", "")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
