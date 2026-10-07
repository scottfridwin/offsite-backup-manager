package cryptoutil

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"aead.dev/minisign"
	"filippo.io/age"
)

func TestEncryptRoundTrip(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	recipients, err := ParseRecipients([]string{id.Recipient().String()})
	if err != nil {
		t.Fatalf("parse recipients: %v", err)
	}

	var cipher bytes.Buffer
	w, err := EncryptWriter(&cipher, recipients)
	if err != nil {
		t.Fatalf("encrypt writer: %v", err)
	}
	plaintext := []byte("top secret backup bytes")
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r, err := age.Decrypt(&cipher, id)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestParseRecipientsRejectsJunk(t *testing.T) {
	if _, err := ParseRecipients([]string{"not-a-recipient"}); err == nil {
		t.Fatal("expected error for invalid recipient")
	}
	if _, err := ParseRecipients([]string{"", "   "}); err == nil {
		t.Fatal("expected error when no valid recipients")
	}
}

func TestSignAndVerify(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer := NewSigner(priv)

	msg := []byte("manifest contents")
	sig := signer.Sign(msg)

	if !minisign.Verify(pub, msg, sig) {
		t.Fatal("signature did not verify")
	}
	if minisign.Verify(pub, []byte("tampered"), sig) {
		t.Fatal("verification should fail for tampered message")
	}
}

func TestLoadVerifier(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubText, err := pub.MarshalText()
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPath := filepath.Join(t.TempDir(), "minisign.pub")
	if err := os.WriteFile(pubPath, pubText, 0o640); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	verifier, err := LoadVerifier(pubPath)
	if err != nil {
		t.Fatalf("load verifier: %v", err)
	}

	msg := []byte("manifest contents")
	sig := minisign.Sign(priv, msg)

	if !verifier.Verify(msg, sig) {
		t.Fatal("signature did not verify")
	}
	if verifier.Verify([]byte("tampered"), sig) {
		t.Fatal("verification should fail for tampered message")
	}
}

func TestLoadVerifierMissingFile(t *testing.T) {
	if _, err := LoadVerifier(filepath.Join(t.TempDir(), "missing.pub")); err == nil {
		t.Fatal("expected error for missing public key file")
	}
}

func TestGenerateAgeIdentity(t *testing.T) {
	identity, recipient, err := GenerateAgeIdentity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}

	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		t.Fatalf("parse generated identity: %v", err)
	}
	if id.Recipient().String() != recipient {
		t.Fatalf("recipient mismatch: got %q, want %q", recipient, id.Recipient().String())
	}

	// Round-trip: encrypt to the recipient, decrypt with the identity.
	recipients, err := ParseRecipients([]string{recipient})
	if err != nil {
		t.Fatalf("parse recipients: %v", err)
	}
	var cipher bytes.Buffer
	w, err := EncryptWriter(&cipher, recipients)
	if err != nil {
		t.Fatalf("encrypt writer: %v", err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r, err := age.Decrypt(&cipher, id)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestGenerateMinisignKeyPairUnprotected(t *testing.T) {
	pubText, secText, err := GenerateMinisignKeyPair("")
	if err != nil {
		t.Fatalf("generate minisign keypair: %v", err)
	}

	dir := t.TempDir()
	pubPath := filepath.Join(dir, "minisign.pub")
	secPath := filepath.Join(dir, "minisign.key")
	if err := os.WriteFile(pubPath, pubText, 0o640); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	if err := os.WriteFile(secPath, secText, 0o600); err != nil {
		t.Fatalf("write secret key: %v", err)
	}

	verifier, err := LoadVerifier(pubPath)
	if err != nil {
		t.Fatalf("load verifier: %v", err)
	}
	signer, err := LoadSigner(secPath, "")
	if err != nil {
		t.Fatalf("load signer: %v", err)
	}

	msg := []byte("manifest contents")
	sig := signer.Sign(msg)
	if !verifier.Verify(msg, sig) {
		t.Fatal("signature did not verify")
	}
}

func TestGenerateMinisignKeyPairPasswordProtected(t *testing.T) {
	_, secText, err := GenerateMinisignKeyPair("correct-horse")
	if err != nil {
		t.Fatalf("generate minisign keypair: %v", err)
	}

	secPath := filepath.Join(t.TempDir(), "minisign.key")
	if err := os.WriteFile(secPath, secText, 0o600); err != nil {
		t.Fatalf("write secret key: %v", err)
	}

	if _, err := LoadSigner(secPath, "wrong-password"); err == nil {
		t.Fatal("expected error loading with the wrong password")
	}
	if _, err := LoadSigner(secPath, "correct-horse"); err != nil {
		t.Fatalf("load signer with correct password: %v", err)
	}
}
