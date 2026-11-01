package oauth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultIterations is the PBKDF2-SHA256 work factor for owner passphrases.
const DefaultIterations = 600_000

// HashPassphrase returns "pbkdf2-sha256$<iterations>$<salt>$<key>".
func HashPassphrase(passphrase string, iterations int) (string, error) {
	if len(passphrase) < 12 {
		return "", errors.New("the owner passphrase must have at least 12 characters")
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func parseHash(h string) (iter int, salt, key []byte, err error) {
	parts := strings.Split(strings.TrimSpace(h), "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return 0, nil, nil, errors.New("want pbkdf2-sha256$<iterations>$<salt>$<key>")
	}
	if iter, err = strconv.Atoi(parts[1]); err != nil || iter < 1 {
		return 0, nil, nil, errors.New("bad iteration count")
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[2]); err != nil {
		return 0, nil, nil, errors.New("bad salt")
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[3]); err != nil || len(key) != 32 {
		return 0, nil, nil, errors.New("bad key")
	}
	return iter, salt, key, nil
}

func checkPassphrase(hash, passphrase string) bool {
	iter, salt, key, err := parseHash(hash)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, passphrase, salt, iter, 32)
	return err == nil && subtle.ConstantTimeCompare(got, key) == 1
}
