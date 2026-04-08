package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (h *handlers) requireSSH(w http.ResponseWriter) bool {
	h.mu.RLock()
	kvm := h.kvm
	h.mu.RUnlock()
	if kvm == nil || !kvm.HasSSH() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"success": false,
			"error":   "SSH not configured — add SSH credentials in Settings first",
		})
		return false
	}
	return true
}

// edidStatus checks the current negotiated HDMI input resolution.
func (h *handlers) edidStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireSSH(w) {
		return
	}
	h.mu.RLock()
	kvm := h.kvm
	h.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	const script = `
TIMINGS=$(v4l2-ctl -d /dev/video0 --get-dv-timings 2>/dev/null)
if echo "$TIMINGS" | grep -q "Active width"; then
  W=$(echo "$TIMINGS" | grep "Active width"  | awk '{print $NF}')
  H=$(echo "$TIMINGS" | grep "Active height" | awk '{print $NF}')
  echo "input_res=${W}x${H}"
else
  echo "input_res=no signal"
fi
if command -v kvmd-edidconf >/dev/null 2>&1; then
  echo "edid_tool=kvmd-edidconf"
  kvmd-edidconf --list-presets 2>/dev/null | grep -i 1080 | head -3 | sed 's/^/preset: /' || true
else
  echo "edid_tool=none (kvmd-edidconf not found)"
fi
`

	out, err := kvm.SSHRun(ctx, script)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   friendlySSHErr(err),
			"output":  out,
		})
		return
	}

	inputRes := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "input_res=") {
			inputRes = strings.TrimPrefix(line, "input_res=")
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"input_res": inputRes,
		"output":    out,
	})
}

// edidSet injects a 1080p EDID so the HDMI source negotiates 1920×1080.
// Uses kvmd-edidconf which ships on all PiKVM systems.
func (h *handlers) edidSet(w http.ResponseWriter, r *http.Request) {
	if !h.requireSSH(w) {
		return
	}
	h.mu.RLock()
	kvm := h.kvm
	h.mu.RUnlock()

	const script = `
set -e
rw 2>/dev/null || true

if ! command -v kvmd-edidconf >/dev/null 2>&1; then
  echo "ERROR: kvmd-edidconf not found"
  echo "Update PiKVM first: rw && pacman -Syu kvmd && ro"
  exit 1
fi

echo "kvmd-edidconf version: $(kvmd-edidconf --version 2>/dev/null | head -1 || echo unknown)"
echo ""

# List available 1080p presets so we pick the right one
PRESETS=$(kvmd-edidconf --list-presets 2>/dev/null | grep -i "1080p-by-default" || true)
echo "Available 1080p presets:"
echo "${PRESETS:-  (none found — trying generic names)}"
echo ""

# Try presets from newest to oldest hardware revision
APPLIED=""
for PRESET in v4.1080p-by-default v3.1080p-by-default v2.1080p-by-default v1.1080p-by-default; do
  if echo "$PRESETS" | grep -qF "$PRESET"; then
    echo "Applying preset: $PRESET"
    kvmd-edidconf --import-preset="$PRESET" --apply
    APPLIED="$PRESET"
    break
  fi
done

if [ -z "$APPLIED" ]; then
  # No versioned preset matched — try the first available 1080p preset
  FIRST=$(kvmd-edidconf --list-presets 2>/dev/null | grep -i "1080" | awk '{print $1}' | head -1)
  if [ -n "$FIRST" ]; then
    echo "Applying first available 1080p preset: $FIRST"
    kvmd-edidconf --import-preset="$FIRST" --apply
    APPLIED="$FIRST"
  else
    echo "ERROR: No 1080p presets found in kvmd-edidconf"
    echo "Run 'kvmd-edidconf --list-presets' on the PiKVM to see what is available"
    exit 1
  fi
fi

echo ""
echo "Preset '$APPLIED' applied to HDMI capture device."

# kvmd re-applies EDID on start — restart it to make the setting persist across reboots
systemctl restart kvmd 2>/dev/null && echo "kvmd restarted (setting is now persistent)" || echo "note: could not restart kvmd"

echo ""
echo "Done. If the source resolution has not changed, unplug and replug the HDMI cable."
`

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	out, err := kvm.SSHRun(ctx, script)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   friendlySSHErr(err),
			"output":  out,
		})
		return
	}

	success := !strings.Contains(out, "ERROR:")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": success,
		"output":  out,
	})
}

// edidRestore removes the custom EDID and restores the PiKVM default.
func (h *handlers) edidRestore(w http.ResponseWriter, r *http.Request) {
	if !h.requireSSH(w) {
		return
	}
	h.mu.RLock()
	kvm := h.kvm
	h.mu.RUnlock()

	const script = `
set -e
rw 2>/dev/null || true

if command -v kvmd-edidconf >/dev/null 2>&1; then
  kvmd-edidconf --reset --apply 2>/dev/null \
    && echo "EDID reset to device default via kvmd-edidconf" \
    || echo "note: --reset not supported on this version of kvmd-edidconf"
else
  echo "kvmd-edidconf not found — nothing to restore"
fi

systemctl restart kvmd 2>/dev/null && echo "kvmd restarted" || true
echo "Done. Replug HDMI cable if resolution has not changed."
`

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	out, err := kvm.SSHRun(ctx, script)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   friendlySSHErr(err),
			"output":  out,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"output":  out,
	})
}

// friendlySSHErr converts raw SSH dial errors into readable messages.
func friendlySSHErr(err error) string {
	s := err.Error()
	if strings.Contains(s, "i/o timeout") || strings.Contains(s, "connection timed out") {
		return "PiKVM unreachable — is it online and on the same network?"
	}
	if strings.Contains(s, "connection refused") {
		return "SSH port refused — is SSH enabled on the PiKVM?"
	}
	if strings.Contains(s, "unable to authenticate") || strings.Contains(s, "no supported methods") {
		return "SSH authentication failed — check SSH credentials in Settings"
	}
	return fmt.Sprintf("SSH error: %s", s)
}
