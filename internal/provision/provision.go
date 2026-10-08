// Package provision renders a zero-touch Raspberry Pi provisioning bundle for a
// new backup node: a set of files to drop onto the freshly imaged SD card's
// boot partition so the Pi self-provisions on first boot with no SSH or manual
// setup. The node only ever needs a one-time enrollment token and the Primary's
// (public) minisign key, so nothing secret beyond a short-lived token is placed
// on the card.
package provision

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// Bundle describes a node to provision. Zero values for NodeUser, StoreDir,
// PullInterval, and CapacityWarnPct are filled with sensible defaults.
type Bundle struct {
	// BackupHost is the Primary endpoint the node pulls from (e.g. backup.example.com).
	BackupHost string
	// NodeLabel is the human-friendly node name registered on the Primary.
	NodeLabel string
	// NodeImage is the fully-qualified node container image reference.
	NodeImage string
	// EnrollmentToken is the one-time bootstrap token (hex), embedded on the card.
	EnrollmentToken string
	// MinisignPubKey is the Primary's public key (.pub file content) the node
	// uses to verify manifests. Not secret.
	MinisignPubKey string
	// NodeUser is the unprivileged login user the node runs as (default "backupnode").
	NodeUser string
	// StoreDir is the node's append-only store path (default /srv/backup-node/store).
	StoreDir string
	// PullInterval is how often the node polls (default "1h").
	PullInterval string
	// CapacityWarnPct is the node free-space warning threshold (default 90).
	CapacityWarnPct int
}

var (
	reLabel    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	reHost     = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	reUser     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reImage    = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,256}$`)
	reStore    = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)
	reInterval = regexp.MustCompile(`^[0-9]+[smh]$`)
	reToken    = regexp.MustCompile(`^[a-f0-9]{32,}$`)
)

// Render validates b and returns the bundle files keyed by filename: a
// cloud-init "user-data" and "meta-data" (dropped on the SD card's boot
// partition) plus a README. On modern Raspberry Pi OS (Debian 12/13) cloud-init
// is the first-boot mechanism, so the node self-provisions with no SSH.
func Render(b Bundle) (map[string]string, error) {
	if b.NodeUser == "" {
		b.NodeUser = "backupnode"
	}
	if b.StoreDir == "" {
		b.StoreDir = "/srv/backup-node/store"
	}
	if b.PullInterval == "" {
		b.PullInterval = "1h"
	}
	if b.CapacityWarnPct == 0 {
		b.CapacityWarnPct = 90
	}

	switch {
	case !reHost.MatchString(b.BackupHost):
		return nil, fmt.Errorf("invalid backup host %q", b.BackupHost)
	case !reLabel.MatchString(b.NodeLabel):
		return nil, fmt.Errorf("invalid node label %q (allowed: letters, digits, . _ -)", b.NodeLabel)
	case !reImage.MatchString(b.NodeImage):
		return nil, fmt.Errorf("invalid node image %q", b.NodeImage)
	case !reToken.MatchString(b.EnrollmentToken):
		return nil, fmt.Errorf("invalid enrollment token (expected hex)")
	case strings.TrimSpace(b.MinisignPubKey) == "":
		return nil, fmt.Errorf("minisign public key is required")
	case !reUser.MatchString(b.NodeUser):
		return nil, fmt.Errorf("invalid node user %q", b.NodeUser)
	case !reStore.MatchString(b.StoreDir):
		return nil, fmt.Errorf("invalid store dir %q (must be an absolute path)", b.StoreDir)
	case !reInterval.MatchString(b.PullInterval):
		return nil, fmt.Errorf("invalid pull interval %q (e.g. 1h, 30m)", b.PullInterval)
	case b.CapacityWarnPct < 1 || b.CapacityWarnPct > 100:
		return nil, fmt.Errorf("capacity warn pct %d out of range 1-100", b.CapacityWarnPct)
	}

	script, err := render(provisionScriptTmpl, b)
	if err != nil {
		return nil, fmt.Errorf("render provision script: %w", err)
	}
	userData, err := renderUserData(b, script)
	if err != nil {
		return nil, fmt.Errorf("render user-data: %w", err)
	}
	readme, err := render(readmeTmpl, b)
	if err != nil {
		return nil, fmt.Errorf("render README: %w", err)
	}
	return map[string]string{
		"user-data":  userData,
		"meta-data":  renderMetaData(b),
		"README.txt": readme,
	}, nil
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func render(tmpl string, b Bundle) (string, error) {
	t, err := template.New("f").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, b); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// indent prefixes every non-empty line with pad, for embedding a multi-line
// file body under a YAML block scalar.
func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// renderMetaData returns the cloud-init NoCloud meta-data.
func renderMetaData(b Bundle) string {
	return fmt.Sprintf("instance-id: backup-node-%s\nlocal-hostname: %s\n", b.NodeLabel, b.NodeLabel)
}

// renderUserData embeds the provisioning script, public key, and one-time token
// into a cloud-init #cloud-config that self-provisions the node on first boot.
func renderUserData(b Bundle, script string) (string, error) {
	t, err := template.New("u").Parse(userDataTmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, map[string]string{
		"Label":    b.NodeLabel,
		"PubKey":   indent(ensureTrailingNewline(b.MinisignPubKey), "      "),
		"TokenB64": base64.StdEncoding.EncodeToString([]byte(b.EnrollmentToken)),
		"Script":   indent(script, "      "),
	})
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// userDataTmpl is the cloud-init NoCloud user-data: it writes the materials and
// the provisioning script to disk, then runs the script once after boot.
const userDataTmpl = `#cloud-config
hostname: {{.Label}}
write_files:
  - path: /var/lib/backup-node-provision/minisign.pub
    permissions: '0644'
    owner: root:root
    content: |
{{.PubKey}}
  - path: /var/lib/backup-node-provision/enrollment_token
    permissions: '0600'
    owner: root:root
    encoding: b64
    content: {{.TokenB64}}
  - path: /usr/local/sbin/backup-node-provision.sh
    permissions: '0755'
    owner: root:root
    content: |
{{.Script}}
runcmd:
  - [ /bin/bash, /usr/local/sbin/backup-node-provision.sh ]
`

// provisionScriptTmpl is the provisioning payload embedded in the cloud-init
// user-data and run once (after network) by runcmd on first boot.
const provisionScriptTmpl = `#!/bin/bash
# offsite-backup-manager node post-network provisioner (auto-generated).
set -euo pipefail
cd /

NODE_USER="{{.NodeUser}}"
BACKUP_HOST="{{.BackupHost}}"
NODE_LABEL="{{.NodeLabel}}"
NODE_IMAGE="{{.NodeImage}}"
STORE_DIR="{{.StoreDir}}"
PULL_INTERVAL="{{.PullInterval}}"
CAPACITY_WARN_PCT="{{.CapacityWarnPct}}"
DEST=/var/lib/backup-node-provision

# Mirror all output to the boot partition so a failure is readable even from a
# machine that can only see the FAT partition (e.g. Windows).
BOOT=/boot/firmware
[ -d "$BOOT" ] || BOOT=/boot
exec > >(tee -a "$BOOT/provision.log") 2>&1

log() { echo "[backup-node provision] $*"; }

# 1. Unprivileged node user (no interactive login). It must be a NON-system user
#    so it gets an /etc/subuid range for rootless podman; names like "backup"
#    are reserved system accounts on Debian and will not work.
if ! id -u "$NODE_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /usr/sbin/nologin "$NODE_USER"
fi
NODE_UID="$(id -u "$NODE_USER")"
NODE_HOME="$(getent passwd "$NODE_USER" | cut -d: -f6)"
if ! grep -q "^${NODE_USER}:" /etc/subuid; then
  log "error: $NODE_USER has no /etc/subuid range; rootless podman needs one (use a non-system user name)"
  exit 1
fi

# 2. Container runtime (installed from the distro; version varies by release).
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y podman

# 3. Rootless boot survival: linger + an active user manager (for /run/user/UID).
loginctl enable-linger "$NODE_USER"
systemctl start "user@${NODE_UID}.service"
RUNTIME_DIR="/run/user/${NODE_UID}"
for _ in $(seq 1 30); do [ -S "${RUNTIME_DIR}/bus" ] && break; sleep 1; done

run_as_user() {
  sudo -u "$NODE_USER" env \
    HOME="$NODE_HOME" \
    XDG_RUNTIME_DIR="$RUNTIME_DIR" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=${RUNTIME_DIR}/bus" "$@"
}

# 4. Store + materials. The node image runs as nonroot uid 65532, which maps to
#    a host subuid under rootless podman, so the store must be chowned to 65532
#    THROUGH the user namespace or the node can't persist its credential.
STORE_PARENT="$(dirname "$STORE_DIR")"
install -d -o "$NODE_USER" -g "$NODE_USER" "$STORE_PARENT" "$STORE_DIR"
install -o "$NODE_USER" -g "$NODE_USER" -m 0644 "$DEST/minisign.pub" "$STORE_PARENT/minisign.pub"
run_as_user podman unshare chown -R 65532:65532 "$STORE_DIR"

# 5. One-time enrollment token -> rootless podman secret. The token file is
#    root-owned (0600), so feed it via stdin; the rootless user cannot read it.
cat "$DEST/enrollment_token" | run_as_user podman secret create enrollment_token -

# 6. Run the node (rootless, restart-on-boot). Outbound-only; stores ciphertext.
run_as_user podman run -d --name backup-node --restart=always \
  --security-opt no-new-privileges --cap-drop ALL --read-only --tmpfs /tmp \
  -e BACKUP_HOST="$BACKUP_HOST" \
  -e ENROLLMENT_TOKEN_FILE=/run/secrets/enrollment_token \
  -e NODE_LABEL="$NODE_LABEL" \
  -e STORE_DIR=/store \
  -e MINISIGN_PUBKEY=/config/minisign.pub \
  -e PULL_INTERVAL="$PULL_INTERVAL" \
  -e CAPACITY_WARN_PCT="$CAPACITY_WARN_PCT" \
  -v "$STORE_DIR":/store \
  -v "$STORE_PARENT/minisign.pub":/config/minisign.pub:ro \
  --secret enrollment_token,type=mount,target=/run/secrets/enrollment_token \
  "$NODE_IMAGE"

run_as_user systemctl --user enable podman-restart.service || true

# 7. Shred the token and remove it from the card; the node is now autonomous
#    (cloud-init runs this only once per instance).
shred -u "$DEST/enrollment_token" 2>/dev/null || rm -f "$DEST/enrollment_token"
rm -f "$BOOT/user-data" || true

log "node $NODE_LABEL provisioned against $BACKUP_HOST"
`

const readmeTmpl = `Offsite backup node — zero-touch provisioning (cloud-init)
=========================================================

Node label : {{.NodeLabel}}
Primary    : {{.BackupHost}}
Image      : {{.NodeImage}}

Steps (no SSH; works from Windows, macOS, or Linux):
  1. Flash Raspberry Pi OS Lite (64-bit) with Raspberry Pi Imager. If it offers
     OS customisation you can decline it; our cloud-init config is used either
     way (just make sure to overwrite user-data and meta-data in the next step).
  2. The card has a small FAT partition (label "bootfs"). Copy BOTH of these
     files from this bundle onto it, replacing any existing copies:
        user-data
        meta-data
  3. Eject the card and boot the Pi (wired Ethernet, DHCP).
  4. Watch it enroll on the Primary:
        primary nodes -roster <ROSTER>
     It appears 'pending', then 'active' after its first heartbeat. If something
     fails, read provision.log on the "bootfs" partition.

Security:
  - The enrollment token is one-time and short-lived; it is removed from the
    card and shredded on the node after enrollment.
  - The node stores only encrypted packages it cannot decrypt.
`
