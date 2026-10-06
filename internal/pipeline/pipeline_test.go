package pipeline

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"aead.dev/minisign"
	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/scottfridwin/offsite-backup-manager/internal/config"
)

func TestRunEndToEnd(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()
	work := t.TempDir()
	keyDir := t.TempDir()

	writeFile(t, filepath.Join(src, "svc", "a.txt"), "alpha")
	writeFile(t, filepath.Join(src, "svc", "b.txt"), "bravo")

	// Recipient (decryption identity kept locally for verification).
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}

	// Signing key written to a password-protected minisign key file.
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate minisign key: %v", err)
	}
	encKey, err := minisign.EncryptKey("pw", priv)
	if err != nil {
		t.Fatalf("encrypt minisign key: %v", err)
	}
	keyPath := filepath.Join(keyDir, "minisign.key")
	if err := os.WriteFile(keyPath, encKey, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		SourceDir:        src,
		WorkDir:          work,
		OutputDir:        out,
		AgeRecipients:    []string{id.Recipient().String()},
		MinisignKeyFile:  keyPath,
		MinisignPassword: "pw",
	}

	res, err := Run(cfg, "test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, p := range []string{res.PackagePath, res.ManifestPath, res.SignaturePath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected artifact %q: %v", p, err)
		}
	}

	// Manifest checksum must match the package file on disk.
	pkgBytes, err := os.ReadFile(res.PackagePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pkgBytes)
	if got := hex.EncodeToString(sum[:]); got != res.Manifest.Package.SHA256 {
		t.Errorf("manifest sha256 = %s, package sha256 = %s", res.Manifest.Package.SHA256, got)
	}
	if res.Manifest.Package.Bytes != int64(len(pkgBytes)) {
		t.Errorf("manifest size = %d, actual = %d", res.Manifest.Package.Bytes, len(pkgBytes))
	}

	// Signature must verify over the manifest file bytes.
	mb, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(res.SignaturePath)
	if err != nil {
		t.Fatal(err)
	}
	if !minisign.Verify(pub, mb, sig) {
		t.Fatal("manifest signature did not verify")
	}

	// Decrypt + decompress + untar and confirm the payload.
	contents := decryptPackage(t, res.PackagePath, id)
	if contents["svc/a.txt"] != "alpha" || contents["svc/b.txt"] != "bravo" {
		t.Fatalf("unexpected package contents: %v", contents)
	}

	// Content stats in the manifest should reflect the two files.
	if res.Manifest.Contents.FileCount != 2 {
		t.Errorf("manifest file count = %d, want 2", res.Manifest.Contents.FileCount)
	}
}

func decryptPackage(t *testing.T, path string, id *age.X25519Identity) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	r, err := age.Decrypt(f, id)
	if err != nil {
		t.Fatalf("age decrypt: %v", err)
	}
	zr, err := zstd.NewReader(r)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			out[hdr.Name] = string(b)
		}
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}
