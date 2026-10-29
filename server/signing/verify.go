package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// PublicKey is a named Ed25519 public key, as listed in trusted-public-keys.
type PublicKey struct {
	Name string
	key  ed25519.PublicKey
}

// ParsePublicKey parses a public key in the format "name:base64-key".
func ParsePublicKey(s string) (*PublicKey, error) {
	name, keyBase64, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found || name == "" {
		return nil, errors.New("public key must be name:base64-key")
	}

	keyBytes, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	if len(keyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key length: expected %d bytes, got %d", ed25519.PublicKeySize, len(keyBytes))
	}

	return &PublicKey{Name: name, key: ed25519.PublicKey(keyBytes)}, nil
}

// VerifyNarinfo reports whether any of sigs ("name:base64-signature") is a
// valid signature of info by this key.
func (k *PublicKey) VerifyNarinfo(info *NarInfo, sigs []string) bool {
	fingerprint, err := GenerateFingerprint(info)
	if err != nil {
		return false
	}

	for _, sig := range sigs {
		name, sigBase64, found := strings.Cut(sig, ":")
		if !found || name != k.Name {
			continue
		}

		sigBytes, err := base64.StdEncoding.DecodeString(sigBase64)
		if err != nil {
			continue
		}

		if ed25519.Verify(k.key, fingerprint, sigBytes) {
			return true
		}
	}

	return false
}
