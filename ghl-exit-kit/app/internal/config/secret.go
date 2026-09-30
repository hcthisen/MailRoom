package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// resolveSecret returns the env secret, or the persisted key file, creating
// the file with a fresh 256-bit random key when it doesn't exist yet.
func resolveSecret(envSecret, path string) ([]byte, error) {
	if s := strings.TrimSpace(envSecret); s != "" {
		if len(s) < MinSecretLength {
			return nil, fmt.Errorf("invalid configuration: MAILROOM_SECRET must be at least %d characters", MinSecretLength)
		}
		return []byte(s), nil
	}
	if b, err := readSecretFile(path); err == nil {
		return b, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return createSecretFile(path)
}

func readSecretFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(b))
	if len(s) < MinSecretLength {
		return nil, fmt.Errorf("secret key file %s is shorter than %d characters", path, MinSecretLength)
	}
	return []byte(s), nil
}

// createSecretFile writes a new key with O_EXCL, so two processes starting
// at once can't overwrite each other; the loser reads the winner's key.
func createSecretFile(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}
	key := hex.EncodeToString(raw)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return readSecretFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("create secret key file: %w", err)
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("write secret key file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("write secret key file: %w", err)
	}
	return []byte(key), nil
}
