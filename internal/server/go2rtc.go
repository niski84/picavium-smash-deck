package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// go2rtcBase returns the plain-HTTP base URL for go2rtc on the PiKVM.
// go2rtc listens on :1984 (HTTP, not HTTPS).
func (h *handlers) go2rtcBase() string {
	h.mu.RLock()
	host := h.host
	h.mu.RUnlock()

	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	// Strip port if any
	if idx := strings.LastIndex(host, ":"); idx > strings.LastIndex(host, "]") {
		host = host[:idx]
	}
	return "http://" + host + ":1984"
}

// go2rtcHTTPClient returns an http.Client suitable for talking to go2rtc
// (plain HTTP, 10s timeout).
func go2rtcHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

// go2rtcStatus reports whether go2rtc is reachable on the PiKVM.
func (h *handlers) go2rtcStatus(w http.ResponseWriter, r *http.Request) {
	base := h.go2rtcBase()
	resp, err := go2rtcHTTPClient().Get(base + "/api/streams")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"running": false,
			"url":     base,
			"error":   err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"running": resp.StatusCode == http.StatusOK,
		"url":     base,
	})
}

// go2rtcInstall SSHes into the PiKVM and installs go2rtc as a systemd service.
// PiKVM uses a read-only root fs — the script runs `rw` first.
func (h *handlers) go2rtcInstall(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	kvm := h.kvm
	h.mu.RUnlock()

	if kvm == nil || !kvm.HasSSH() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"success": false,
			"error":   "SSH not configured — set PIKVM_SSH_USER and PIKVM_SSH_PASS in settings",
		})
		return
	}

	const installScript = `
set -e
rw 2>/dev/null || true   # PiKVM: unlock read-only root filesystem

# ── 0. Idempotency check — skip download if already current ──────────────────
ARCH=$(uname -m)
case "$ARCH" in
  aarch64) GOARCH=arm64 ;;
  armv7l|armv6l) GOARCH=arm ;;
  x86_64)  GOARCH=amd64 ;;
  *) echo "ERROR: unsupported arch $ARCH"; exit 1 ;;
esac
echo "arch: $ARCH → $GOARCH"

LATEST_URL="https://github.com/AlexxIT/go2rtc/releases/latest/download/go2rtc_linux_${GOARCH}"
LATEST_VER=$(curl -sfI "https://github.com/AlexxIT/go2rtc/releases/latest" \
  | grep -i "^location:" | grep -oP 'tag/\K[^\r]+' || echo "unknown")
INSTALLED_VER=$(/usr/local/bin/go2rtc --version 2>/dev/null | head -1 || echo "none")
echo "latest: $LATEST_VER  installed: $INSTALLED_VER"

if [ "$LATEST_VER" != "unknown" ] && echo "$INSTALLED_VER" | grep -qF "${LATEST_VER#v}"; then
  if systemctl is-active --quiet go2rtc; then
    echo "go2rtc $INSTALLED_VER already installed and running — nothing to do"
    echo "Tip: click Reinstall to force a fresh install anyway"
    exit 0
  fi
fi

# ── 1. Install dependencies ──────────────────────────────────────────────────
for pkg in ffmpeg socat; do
  if ! command -v $pkg >/dev/null 2>&1; then
    echo "installing $pkg via pacman..."
    pacman -Sy --noconfirm $pkg
  fi
done
echo "ffmpeg: $(ffmpeg -version 2>&1 | head -1)"

# ── 2. Stop running service before overwriting binary ────────────────────────
systemctl stop go2rtc 2>/dev/null || true

# ── 3. Download go2rtc binary ────────────────────────────────────────────────
URL="$LATEST_URL"
echo "downloading $URL"
curl -fsSL "$URL" -o /tmp/go2rtc_new \
  || wget -q "$URL" -O /tmp/go2rtc_new
mv /tmp/go2rtc_new /usr/local/bin/go2rtc
chmod +x /usr/local/bin/go2rtc
echo "installed: $(/usr/local/bin/go2rtc --version 2>&1 | head -1)"

# ── 5. Socat bridge: ustreamer Unix socket → TCP 127.0.0.1:8082 ──────────────
# go2rtc uses ffmpeg to read MJPEG from the socat bridge and transcode to H.264.
# The H.264 SHM approach fails on static screens (drop-same-frames blocks reads).
cat > /etc/systemd/system/ustreamer-bridge.service << 'SVC'
[Unit]
Description=ustreamer MJPEG TCP bridge (127.0.0.1:8082 → /run/kvmd/ustreamer.sock)
After=kvmd.service
BindsTo=kvmd.service

[Service]
ExecStart=/usr/bin/socat TCP-LISTEN:8082,bind=127.0.0.1,fork,reuseaddr UNIX-CLIENT:/run/kvmd/ustreamer.sock
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
SVC
systemctl daemon-reload
systemctl enable ustreamer-bridge
systemctl restart ustreamer-bridge
echo "ustreamer-bridge service enabled"

# ── 6. Write go2rtc config ────────────────────────────────────────────────────
mkdir -p /etc/go2rtc
cat > /etc/go2rtc/go2rtc.yaml << 'YAML'
api:
  listen: ":1984"
  origin: "*"

ffmpeg:
  bin: ffmpeg
  h264: -c:v libx264 -preset ultrafast -tune zerolatency -pix_fmt yuv420p -g 30

streams:
  kvm:
    # go2rtc reads MJPEG from ustreamer via socat bridge and transcodes to H.264.
    # This works even on static screens (no drop-same-frames blocking issue).
    - "ffmpeg:http://127.0.0.1:8082/stream#video=h264"

webrtc:
  ice_tcp: true
YAML
echo "config written to /etc/go2rtc/go2rtc.yaml"

# ── 7. Systemd service ────────────────────────────────────────────────────────
cat > /etc/systemd/system/go2rtc.service << 'SVC'
[Unit]
Description=go2rtc — lightweight WebRTC/WHEP streaming server
After=network.target

[Service]
ExecStart=/usr/local/bin/go2rtc -config /etc/go2rtc/go2rtc.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
SVC

systemctl daemon-reload
systemctl enable go2rtc
systemctl restart go2rtc
sleep 1
systemctl is-active go2rtc && echo "go2rtc is running" || echo "WARNING: go2rtc may have failed — check: journalctl -u go2rtc"
`

	// Allow up to 5 minutes for pacman + download + install
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	out, err := kvm.SSHRun(ctx, installScript)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("SSH install failed: %v", err),
			"output":  out,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"output":  out,
	})
}

// whepProxy forwards a WHEP SDP offer to go2rtc on the PiKVM and returns
// the SDP answer. go2rtc bundles all ICE candidates in the answer (no trickle),
// so this call blocks until go2rtc finishes ICE gathering (~3-8 s on first connect).
func (h *handlers) whepProxy(w http.ResponseWriter, r *http.Request) {
	// Handle CORS preflight
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	base := h.go2rtcBase()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		base+"/api/webrtc?src=kvm", bytes.NewReader(offer))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/sdp")

	// go2rtc's WHEP can take 3-8s while it gathers ICE candidates
	fwdClient := &http.Client{Timeout: 20 * time.Second}
	resp, err := fwdClient.Do(req)
	if err != nil {
		http.Error(w, "go2rtc unreachable — is it installed and running? "+err.Error(),
			http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/sdp")
	// go2rtc returns 201; some clients expect 200 — normalise to 200
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answer)
}
