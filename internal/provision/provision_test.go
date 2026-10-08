package provision

import (
	"os/exec"
	"strings"
	"testing"
)

func validBundle() Bundle {
	return Bundle{
		BackupHost:      "backup.example.com",
		NodeLabel:       "pi-garage",
		NodeImage:       "ghcr.io/example/offsite-backup-manager-node:v0.2.0",
		EnrollmentToken: strings.Repeat("ab", 32),
		MinisignPubKey:  "untrusted comment: minisign public key: 4D88C985E82CFA28\nRWQo+izohcmITZl499nxYIG7eXF9K2HddWRS6eaY/1g0qK+K5ihJoct1\n",
	}
}

func TestRenderProducesBundleFiles(t *testing.T) {
	files, err := Render(validBundle())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"firstrun.sh", "provision.sh", "backup-node-provision.service",
		"install-to-boot.sh", "minisign.pub", "enrollment_token", "README.txt",
	} {
		if _, ok := files[want]; !ok {
			t.Fatalf("missing bundle file %q", want)
		}
	}

	// The one-time token and the embedded config land where expected.
	if got := files["enrollment_token"]; got != strings.Repeat("ab", 32) {
		t.Fatalf("enrollment_token content = %q", got)
	}
	if !strings.Contains(files["provision.sh"], `BACKUP_HOST="backup.example.com"`) {
		t.Fatalf("provision.sh missing backup host")
	}
	if !strings.Contains(files["provision.sh"], `NODE_LABEL="pi-garage"`) {
		t.Fatalf("provision.sh missing node label")
	}
	if !strings.Contains(files["provision.sh"], "65532:65532") {
		t.Fatalf("provision.sh missing nonroot store chown")
	}
	// Defaults applied.
	if !strings.Contains(files["provision.sh"], `PULL_INTERVAL="1h"`) {
		t.Fatalf("provision.sh missing default pull interval")
	}
	if !strings.Contains(files["provision.sh"], `NODE_USER="backup"`) {
		t.Fatalf("provision.sh missing default node user")
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	cases := map[string]func(*Bundle){
		"bad host":     func(b *Bundle) { b.BackupHost = "bad host!" },
		"bad label":    func(b *Bundle) { b.NodeLabel = "pi garage" },
		"bad token":    func(b *Bundle) { b.EnrollmentToken = "NOTHEX" },
		"empty pubkey": func(b *Bundle) { b.MinisignPubKey = "   " },
		"bad store":    func(b *Bundle) { b.StoreDir = "relative/path" },
		"bad interval": func(b *Bundle) { b.PullInterval = "1 hour" },
		"bad pct":      func(b *Bundle) { b.CapacityWarnPct = 150 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := validBundle()
			mutate(&b)
			if _, err := Render(b); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

// TestRenderedShellScriptsAreValid syntax-checks the generated shell scripts
// with `bash -n` so a template typo can't ship a broken provisioner.
func TestRenderedShellScriptsAreValid(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	files, err := Render(validBundle())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, name := range []string{"firstrun.sh", "provision.sh", "install-to-boot.sh"} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(files[name])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed bash -n: %v\n%s", name, err, out)
		}
	}
}
