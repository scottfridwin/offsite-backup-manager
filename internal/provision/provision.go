// Package provision renders a zero-touch Raspberry Pi provisioning bundle for a
// new backup node: a set of files to drop onto the freshly imaged SD card's
// boot partition so the Pi self-provisions on first boot with no SSH or manual
// setup. The node only ever needs a one-time enrollment token and the Primary's
// (public) minisign key, so nothing secret beyond a short-lived token is placed
// on the card.
package provision

import (
	"bytes"
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
	// NodeUser is the unprivileged login user the node runs as (default "backup").
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

// Render validates b and returns the bundle files keyed by filename. Every file
// is written to the SD card's boot partition by install-to-boot.sh.
func Render(b Bundle) (map[string]string, error) {
	if b.NodeUser == "" {
		b.NodeUser = "backup"
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

	files := map[string]string{
		"minisign.pub":     ensureTrailingNewline(b.MinisignPubKey),
		"enrollment_token": b.EnrollmentToken,
	}
	for name, tmpl := range templates {
		rendered, err := render(tmpl, b)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", name, err)
		}
		files[name] = rendered
	}
	return files, nil
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

var templates = map[string]string{
	"firstrun.sh":                   firstRunTmpl,
	"provision.sh":                  provisionTmpl,
	"backup-node-provision.service": serviceTmpl,
	"install-to-boot.sh":            installTmpl,
	"README.txt":                    readmeTmpl,
}

// firstRunTmpl runs once, very early (pre-network), via systemd.run in
// cmdline.txt. It must NOT do anything network-dependent; it only relocates the
// materials off the FAT boot partition, installs the post-network provisioner,
// masks the interactive first-boot wizard, scrubs the one-time token from the
// card, and triggers a reboot into the normal system.
const firstRunTmpl = `#!/bin/bash
# offsite-backup-manager node first-boot stage (auto-generated). Runs pre-network.
set -euo pipefail

log() { echo "[backup-node firstrun] $*"; }

BOOT=/boot/firmware
[ -d "$BOOT" ] || BOOT=/boot

DEST=/var/lib/backup-node-provision
install -d -m 0700 "$DEST"
install -m 0644 "$BOOT/minisign.pub"     "$DEST/minisign.pub"
install -m 0600 "$BOOT/enrollment_token" "$DEST/enrollment_token"
install -m 0755 "$BOOT/provision.sh"     /usr/local/sbin/backup-node-provision.sh
install -m 0644 "$BOOT/backup-node-provision.service" /etc/systemd/system/backup-node-provision.service

systemctl enable backup-node-provision.service
# Suppress the interactive first-boot user wizard on headless Lite images.
systemctl mask userconfig.service 2>/dev/null || true

# Scrub provisioning materials (including the one-time token) from the card.
rm -f "$BOOT"/firstrun.sh "$BOOT"/provision.sh "$BOOT"/backup-node-provision.service \
      "$BOOT"/minisign.pub "$BOOT"/enrollment_token "$BOOT"/install-to-boot.sh \
      "$BOOT"/README.txt || true
# Remove our one-shot hook so this never runs again.
sed -i 's# systemd.run=[^ ]*##g; s# systemd.run_success_action=[^ ]*##g; s# systemd.run_failure_action=[^ ]*##g; s# systemd.unit=kernel-command-line.target##g' "$BOOT/cmdline.txt" 2>/dev/null || true

log "staged; rebooting into post-network provisioning"
`

// provisionTmpl runs once after the network is up (invoked by the systemd
// oneshot unit). It does all the network/podman work, then disables itself and
// shreds the token.
const provisionTmpl = `#!/bin/bash
# offsite-backup-manager node post-network provisioner (auto-generated).
set -euo pipefail

NODE_USER="{{.NodeUser}}"
BACKUP_HOST="{{.BackupHost}}"
NODE_LABEL="{{.NodeLabel}}"
NODE_IMAGE="{{.NodeImage}}"
STORE_DIR="{{.StoreDir}}"
PULL_INTERVAL="{{.PullInterval}}"
CAPACITY_WARN_PCT="{{.CapacityWarnPct}}"
DEST=/var/lib/backup-node-provision

log() { echo "[backup-node provision] $*"; }

# 1. Unprivileged node user (no interactive login).
if ! id -u "$NODE_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /usr/sbin/nologin "$NODE_USER"
fi
NODE_UID="$(id -u "$NODE_USER")"

# 2. Container runtime (Raspberry Pi OS Bookworm ships podman 4.3.x).
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

# 5. One-time enrollment token -> rootless podman secret.
run_as_user podman secret create enrollment_token "$DEST/enrollment_token"

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

# 7. Self-disable and shred the token; the node is now autonomous.
systemctl disable backup-node-provision.service || true
shred -u "$DEST/enrollment_token" 2>/dev/null || rm -f "$DEST/enrollment_token"

log "node $NODE_LABEL provisioned against $BACKUP_HOST"
`

const serviceTmpl = `[Unit]
Description=Offsite backup node first-boot provisioner
After=network-online.target
Wants=network-online.target
ConditionPathExists=/usr/local/sbin/backup-node-provision.sh

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/backup-node-provision.sh
RemainAfterExit=no

[Install]
WantedBy=multi-user.target
`

// installTmpl runs on the operator's workstation against the mounted boot
// partition. It copies the bundle and adds the one-shot firstrun hook.
const installTmpl = `#!/usr/bin/env bash
# Copy this node bundle onto a freshly imaged Raspberry Pi OS boot partition.
# Usage: ./install-to-boot.sh /path/to/mounted/boot-partition
set -euo pipefail

BOOT="${1:-}"
if [ -z "$BOOT" ] || [ ! -f "$BOOT/cmdline.txt" ]; then
  echo "usage: $0 /path/to/mounted/boot-partition (must contain cmdline.txt)" >&2
  exit 1
fi

HERE="$(cd "$(dirname "$0")" && pwd)"
for f in firstrun.sh provision.sh backup-node-provision.service minisign.pub enrollment_token; do
  cp "$HERE/$f" "$BOOT/$f"
done
chmod 0700 "$BOOT/enrollment_token" || true

# Add the one-shot firstrun hook to cmdline.txt (single line, idempotent).
CMD="$(tr -d '\n' < "$BOOT/cmdline.txt")"
case "$CMD" in
  *systemd.run=*) : ;; # already hooked
  *) CMD="$CMD systemd.run=/boot/firstrun.sh systemd.run_success_action=reboot systemd.run_failure_action=reboot systemd.unit=kernel-command-line.target" ;;
esac
printf '%s\n' "$CMD" > "$BOOT/cmdline.txt"

echo "Bundle installed to $BOOT. Eject the card and boot the Pi; it self-provisions."
`

const readmeTmpl = `Offsite backup node — zero-touch provisioning bundle
====================================================

Node label : {{.NodeLabel}}
Primary    : {{.BackupHost}}
Image      : {{.NodeImage}}

Steps (no SSH / no interaction with the Pi):
  1. Flash Raspberry Pi OS Lite (64-bit) to the SD card (plain image is fine).
  2. With the card still mounted, run:  ./install-to-boot.sh /path/to/boot
  3. Eject the card and boot the Pi (wired Ethernet, DHCP).
  4. Watch it enroll on the Primary:
        primary nodes -roster <ROSTER>
     It appears 'pending', then 'active' after its first heartbeat.

Security:
  - enrollment_token is one-time and short-lived; it is scrubbed from the card
    and the Pi after the node enrolls.
  - The node stores only encrypted packages it cannot decrypt.
`
