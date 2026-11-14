// Package secret encrypts the passwords a batch import brings in.
//
// The passwords are kept in the database so that an account whose
// sign-in chain is broken can be signed in again without a human
// bringing the list back. The key is not in the database and not in
// the configuration file: it comes from the environment, so that a
// backup of the database does not carry the key with it.
//
// The cipher is AES-256-GCM. The text that is stored is
// base64(nonce ‖ ciphertext ‖ tag): the nonce is twelve random
// bytes, and the tag is GCM's own.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySize is how long a key is.
const KeySize = 32

// Key is a password-encryption key.
type Key []byte

// FromEnv reads the key from an environment variable.
//
// The variable holds the key in base64. A variable that is not there,
// or a key that is not KeySize bytes, is an error: a key that is the
// wrong size would encrypt things that can never be opened again.
func FromEnv(name string) (Key, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return nil, fmt.Errorf("%s is not set", name)
	}
	raw = strings.TrimSpace(raw)
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not base64: %w", name, err)
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("%s is %d bytes, want %d", name, len(b), KeySize)
	}
	return Key(b), nil
}

// Seal encrypts a password and gives back the text to store.
func (k Key) Seal(plain string) (string, error) {
	if len(k) != KeySize {
		return "", fmt.Errorf("the key is %d bytes, want %d", len(k), KeySize)
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a password that Seal made.
func (k Key) Open(text string) (string, error) {
	if len(k) != KeySize {
		return "", fmt.Errorf("the key is %d bytes, want %d", len(k), KeySize)
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", errors.New("the cipher text is not base64")
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("the cipher text is too short")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", errors.New("the cipher text does not open with this key")
	}
	return string(plain), nil
}
