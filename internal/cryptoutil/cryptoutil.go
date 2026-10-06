// Package cryptoutil wraps the encryption (age) and signing (minisign)
// primitives used by the orchestrator.
package cryptoutil

import (
	"fmt"
	"io"
	"strings"

	"aead.dev/minisign"
	"filippo.io/age"
)

// ParseRecipients parses age X25519 recipient strings (age1...).
func ParseRecipients(recips []string) ([]age.Recipient, error) {
	out := make([]age.Recipient, 0, len(recips))
	for _, r := range recips {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		rec, err := age.ParseX25519Recipient(r)
		if err != nil {
			return nil, fmt.Errorf("parse age recipient %q: %w", r, err)
		}
		out = append(out, rec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid age recipients provided")
	}
	return out, nil
}

// EncryptWriter returns a WriteCloser that encrypts everything written to it to
// the given recipients. The caller must Close it to finalize the ciphertext.
func EncryptWriter(dst io.Writer, recipients []age.Recipient) (io.WriteCloser, error) {
	return age.Encrypt(dst, recipients...)
}

// Signer produces detached minisign signatures.
type Signer struct {
	key minisign.PrivateKey
}

// NewSigner wraps an in-memory minisign private key.
func NewSigner(key minisign.PrivateKey) *Signer {
	return &Signer{key: key}
}

// LoadSigner reads a (possibly password-protected) minisign secret key file.
func LoadSigner(path, password string) (*Signer, error) {
	key, err := minisign.PrivateKeyFromFile(password, path)
	if err != nil {
		return nil, fmt.Errorf("load minisign key %q: %w", path, err)
	}
	return &Signer{key: key}, nil
}

// Sign returns a detached minisign signature over message.
func (s *Signer) Sign(message []byte) []byte {
	return minisign.Sign(s.key, message)
}
