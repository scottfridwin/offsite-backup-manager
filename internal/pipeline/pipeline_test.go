package pipeline

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aead.dev/minisign"
	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/scottfridwin/offsite-backup-manager/internal/config"
	"github.com/scottfridwin/offsite-backup-manager/internal/health"
	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func TestRunEndToEnd(t *testing.T) {
	src := t.TempDir()
	out := t.TempDir()
	work := t.TempDir()
	keyDir := t.TempDir()

	writeFile(t, filepath.Join(src, "svc", "a.txt"), "alpha")
	writeFile(t, filepath.Join(src, "svc", "b.txt"), "bravo")
	if err := os.Chmod(filepath.Join(src, "svc"), 0o711); err != nil {
		t.Fatal(err)
	}

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

	// Default distribution scheme is replicate-all, resolved at pull time.
	if res.Manifest.Distribution.Scheme != manifest.SchemeReplicateAll {
		t.Errorf("distribution scheme = %q, want %q", res.Manifest.Distribution.Scheme, manifest.SchemeReplicateAll)
	}
	if len(res.Manifest.Distribution.AssignedNodeIDs) != 0 {
		t.Errorf("replicate-all should not pin node IDs at package time, got %v", res.Manifest.Distribution.AssignedNodeIDs)
	}

	// The package describes itself: an embedded manifest is its last entry.
	embedded, ok := contents[EmbeddedManifestName]
	if !ok {
		t.Fatalf("package has no %s", EmbeddedManifestName)
	}
	var em manifest.Manifest
	if err := json.Unmarshal([]byte(embedded), &em); err != nil {
		t.Fatalf("parse embedded manifest: %v", err)
	}
	if em.RunID != res.RunID || em.Contents.FileCount != 2 || em.Package.SHA256 != "" || em.Package.Name != filepath.Base(res.PackagePath) {
		t.Errorf("embedded manifest = %+v", em)
	}

	// Headers carry the source's mode and owner, not the staging copy's.
	for name, hdr := range decryptHeaders(t, res.PackagePath, id) {
		if name == EmbeddedManifestName {
			continue
		}
		if hdr.Uid != os.Getuid() || hdr.Uname != "" {
			t.Errorf("%s: uid=%d uname=%q, want uid %d and no name", name, hdr.Uid, hdr.Uname, os.Getuid())
		}
		if name == "svc/a.txt" && hdr.Mode != 0o640 {
			t.Errorf("svc/a.txt mode = %o, want 640", hdr.Mode)
		}
		if name == "svc/" && hdr.Mode != 0o711 {
			t.Errorf("svc/ mode = %o, want 711", hdr.Mode)
		}
	}

	st, err := health.ReadRunStatus(out)
	if err != nil || st == nil || st.Error != "" || st.RunID != res.RunID {
		t.Fatalf("run status = %+v, %v; want success for %s", st, err, res.RunID)
	}
}

func TestRunRecordsFailure(t *testing.T) {
	out := t.TempDir()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := minisign.EncryptKey("pw", priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "minisign.key")
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		SourceDir:        filepath.Join(t.TempDir(), "missing"),
		WorkDir:          t.TempDir(),
		OutputDir:        out,
		AgeRecipients:    []string{id.Recipient().String()},
		MinisignKeyFile:  keyPath,
		MinisignPassword: "pw",
	}
	if _, err := Run(cfg, "test"); err == nil {
		t.Fatal("expected Run to fail for a missing source")
	}
	st, err := health.ReadRunStatus(out)
	if err != nil || st == nil || st.Error == "" {
		t.Fatalf("run status = %+v, %v; want a recorded failure", st, err)
	}
}

func decryptHeaders(t *testing.T, path string, id *age.X25519Identity) map[string]*tar.Header {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r, err := age.Decrypt(f, id)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zstd.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = hdr
	}
}

func TestBuildDistributionRoundRobin(t *testing.T) {
	rosterPath := filepath.Join(t.TempDir(), "roster.json")
	s := roster.Open(rosterPath)
	a := enrollNode(t, s, "a")
	b := enrollNode(t, s, "b")

	cfg := config.Config{DistributionScheme: manifest.SchemeRoundRobin, NodeRosterFile: rosterPath}

	first, err := buildDistribution(cfg)
	if err != nil {
		t.Fatalf("buildDistribution: %v", err)
	}
	second, err := buildDistribution(cfg)
	if err != nil {
		t.Fatalf("buildDistribution: %v", err)
	}

	if first.Scheme != manifest.SchemeRoundRobin || second.Scheme != manifest.SchemeRoundRobin {
		t.Fatalf("unexpected schemes: %+v %+v", first, second)
	}
	if len(first.AssignedNodeIDs) != 1 || len(second.AssignedNodeIDs) != 1 {
		t.Fatalf("expected exactly one assigned node per run: %+v %+v", first, second)
	}
	if first.AssignedNodeIDs[0] == second.AssignedNodeIDs[0] {
		t.Fatalf("expected successive runs to rotate nodes, got the same node twice: %v", first.AssignedNodeIDs[0])
	}
	ids := map[string]bool{a.ID: true, b.ID: true}
	if !ids[first.AssignedNodeIDs[0]] || !ids[second.AssignedNodeIDs[0]] {
		t.Fatalf("assigned node not in roster: %+v %+v", first, second)
	}
}

func TestBuildDistributionRoundRobinNoActiveNodes(t *testing.T) {
	rosterPath := filepath.Join(t.TempDir(), "roster.json")
	cfg := config.Config{DistributionScheme: manifest.SchemeRoundRobin, NodeRosterFile: rosterPath}
	if _, err := buildDistribution(cfg); err == nil {
		t.Fatal("expected error with no active nodes")
	}
}

func enrollNode(t *testing.T, s *roster.Store, label string) roster.Node {
	t.Helper()
	tok, err := s.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}
	node, _, err := s.Enroll(tok, label)
	if err != nil {
		t.Fatalf("enroll %s: %v", label, err)
	}
	// A node only becomes an assignable participant once it confirms via a
	// first heartbeat.
	if err := s.Heartbeat(node.ID, 1, 2, ""); err != nil {
		t.Fatalf("heartbeat %s: %v", label, err)
	}
	return node
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
