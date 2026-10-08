// Package cryptoutil wraps the encryption (age) and signing (minisign)
// primitives used by the orchestrator.
package cryptoutil

import (
	"crypto/rand"
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

// PublicKeyText returns the signer's minisign public key as the two-line
// ".pub" file content (comment line + base64), ready to hand to a node for
// manifest verification. It is derived from the private key, so no separate
// public-key file is needed.
func (s *Signer) PublicKeyText() (string, error) {
	pub, ok := s.key.Public().(minisign.PublicKey)
	if !ok {
		return "", fmt.Errorf("unexpected minisign public key type %T", s.key.Public())
	}
	txt, err := pub.MarshalText()
	if err != nil {
		return "", fmt.Errorf("marshal minisign public key: %w", err)
	}
	return string(txt) + "\n", nil
}

// Verifier checks detached minisign signatures against a trusted public key.
type Verifier struct {
	key minisign.PublicKey
}

// LoadVerifier reads a minisign public key file.
func LoadVerifier(path string) (*Verifier, error) {
	key, err := minisign.PublicKeyFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("load minisign public key %q: %w", path, err)
	}
	return &Verifier{key: key}, nil
}

// Verify reports whether signature is a valid detached minisign signature of
// message produced by the corresponding private key.
func (v *Verifier) Verify(message, signature []byte) bool {
	return minisign.Verify(v.key, message, signature)
}

// GenerateAgeIdentity creates a new X25519 identity, returning its plaintext
// identity string (AGE-SECRET-KEY-1...) and its public recipient (age1...).
func GenerateAgeIdentity() (identity, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("generate age identity: %w", err)
	}
	return id.String(), id.Recipient().String(), nil
}

// GenerateMinisignKeyPair creates a new minisign keypair, returning the
// on-disk text of the public key and the (optionally password-protected)
// secret key, in the format LoadVerifier/LoadSigner expect. An empty
// password produces an unprotected secret key.
func GenerateMinisignKeyPair(password string) (pubText, secText []byte, err error) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate minisign key: %w", err)
	}
	pubText, err = pub.MarshalText()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal minisign public key: %w", err)
	}
	secText, err = minisign.EncryptKey(password, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("encode minisign secret key: %w", err)
	}
	return pubText, secText, nil
}
