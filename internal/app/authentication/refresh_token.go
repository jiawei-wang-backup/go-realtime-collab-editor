package authentication

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
)

func hashRefresh(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
func parseRefresh(token string) (string, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", identity.ErrInvalidRefreshToken
	}
	return parts[0], parts[1], nil
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
