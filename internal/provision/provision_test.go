package provision

import (
	"encoding/base64"
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

func TestRenderProducesCloudInitFiles(t *testing.T) {
	files, err := Render(validBundle())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"user-data", "meta-data", "README.txt"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("missing bundle file %q", want)
		}
	}

	ud := files["user-data"]
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatalf("user-data must start with #cloud-config, got: %.20q", ud)
	}
	// The one-time token is embedded base64-encoded.
	wantB64 := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("ab", 32)))
	if !strings.Contains(ud, "content: "+wantB64) {
		t.Fatalf("user-data missing base64 token")
	}
	// The embedded provisioning script carries the config + the fixes.
	for _, marker := range []string{
		`BACKUP_HOST="backup.example.com"`,
		`NODE_LABEL="pi-garage"`,
		`NODE_USER="backupnode"`, // non-reserved user (fixes the 'backup' collision)
		"65532:65532",
		"podman secret create enrollment_token -", // token via stdin
	} {
		if !strings.Contains(ud, marker) {
			t.Fatalf("user-data embedded script missing %q", marker)
		}
	}

	if md := files["meta-data"]; !strings.Contains(md, "instance-id: backup-node-pi-garage") {
		t.Fatalf("meta-data missing instance-id, got: %q", md)
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

// TestProvisionScriptIsValidBash syntax-checks the embedded provisioning script
// so a template typo can't ship a broken provisioner.
func TestProvisionScriptIsValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script, err := render(provisionScriptTmpl, validBundle())
	if err != nil {
		t.Fatalf("render script: %v", err)
	}
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provision script failed bash -n: %v\n%s", err, out)
	}
}
