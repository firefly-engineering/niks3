package client

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Mic92/niks3/api"
)

// ConvertHashToNix32 converts a hash from SRI format (sha256-base64) or
// Nix32 format (sha256:nix32) to Nix32 format (sha256:nix32).
// If the hash is already in Nix32 format, it returns it unchanged.
func ConvertHashToNix32(hash string) (string, error) {
	// Check if already in Nix32 format (sha256:...)
	if strings.HasPrefix(hash, "sha256:") && !strings.Contains(hash, "-") && !strings.Contains(hash, "+") && !strings.Contains(hash, "/") && !strings.Contains(hash, "=") {
		// Already in Nix32 format (no base64 chars)
		return hash, nil
	}

	// Parse SRI format (sha256-base64)
	if !strings.HasPrefix(hash, "sha256-") {
		return "", fmt.Errorf("unsupported hash format: %s (expected sha256-... or sha256:...)", hash)
	}

	// Extract base64 part
	base64Hash := strings.TrimPrefix(hash, "sha256-")

	// Decode from base64
	hashBytes, err := base64.StdEncoding.DecodeString(base64Hash)
	if err != nil {
		return "", fmt.Errorf("decoding base64 hash: %w", err)
	}

	// Encode to Nix32
	return "sha256:" + api.EncodeNixBase32(hashBytes), nil
}
