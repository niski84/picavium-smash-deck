# pikvm-key-cli

**A feature-packed open-source web dashboard that transforms your PiKVM into a professional-grade KVM-over-IP platform.**

The stock PiKVM web UI covers the basics. This replaces it with a purpose-built dashboard that adds screen recording, WebRTC streaming, OCR text extraction, macro templating, one-click firmware upgrades, remote diagnostics, and full HDMI resolution control — all from a single Go binary with no external runtime dependencies.

---

## What's New vs. Stock PiKVM

| Capability | Stock PiKVM UI | pikvm-key-cli |
|---|---|---|
| Screen recording | None | H.264/H.265 video + JPEG frames, scheduled |
| WebRTC streaming | None | go2rtc WHEP — ~30ms latency |
| OCR text extraction | None | PaddleOCR (AI) + Tesseract fallback |
| Macro / text templates | None | `{{token}}` substitution + human-mode typing |
| Human typing simulation | None | Typos, backspaces, natural pace |
| 4K → 1080p EDID fix | None | One-click `kvmd-edidconf` injection |
| Firmware upgrade UI | None | SSE streaming terminal in browser |
| Remote diagnostics | None | CPU temp, services, disk, RAM, network |
| H.264 bitrate tuning | YAML file edit | Web UI slider, persisted to device |
| Stream latency (MJPEG) | ~500ms | ~30ms via WebRTC (go2rtc) |
| Frame skip on idle | None | Similarity-based, saves 50%+ storage |
| Playback / timeline | None | Per-day selector, frame slider, 1–60x speed |
| Portable binary | No | Single `go build`, zero runtime deps |

---

## Features

### Live Video — Four Capture Modes

- **WebRTC (go2rtc)** — H.264 at ~30ms latency via WHEP. Installed as a systemd service on the PiKVM over SSH in one click.
- **SSH Tunnel** — Native uStreamer MJPEG via SSH port-forward. Full framerate, no polling overhead.
- **MJPEG Poll** — Three-tier HTTP fallback (kvmd snapshot → SHM → V4L2).
- **Screenshot** — On-demand or 3-second auto-refresh.

All modes include an FPS counter and one-click reconnect.

### Screen Recording

Two modes, fully scheduled:

- **Video** — H.264 or H.265 MP4 segments (one per hour), configurable CRF (18–36) and encoding preset. Fragmented MP4 makes segments playable mid-recording.
- **JPEG** — Individual frames at configurable intervals (2–60s). Browse and step through frames in the browser.

Schedule by day of week and hour range. Automatic cleanup by age (7–90 days) and max storage (500 MB–10 GB). Smart **frame-skip** detects unchanged screens and saves 50%+ storage when the target is idle.

Full playback timeline in the browser: day picker, frame slider, 1x / 10x / 30x / 60x speed, keyboard shortcuts (space, arrow keys), and fullscreen.

### OCR — Read Text Off the Screen

Drag a selection rectangle over the live preview. The server:
1. Captures a fresh screenshot
2. Crops and upscales 4× with contrast enhancement
3. Runs **PaddleOCR** (deep-learning, handles compression artifacts) or **Tesseract** as fallback
4. Returns extracted text in a copyable panel

Works in both dashboard live view and recording preview. One-click copy.

### Text Macro — Type Anything, Anywhere

Paste a block of text into the macro panel. Define `{{token}} = value` substitution pairs — the UI shows a live preview of the resolved text before sending.

**Human mode** makes it look like a real person typed it:
- 7% per-character chance of an adjacent QWERTY key typo → auto-corrected with Backspace
- 2% double-press chance → auto-corrected
- 80ms per-keystroke pace (configurable via PiKVM's `delay` API param)
- Result: realistic rhythm, noise, and hesitation patterns

Supports 8 keymaps: `en-us`, `de`, `ru`, `fr`, `es`, `ja`, `pt`, plus custom.

### HDMI Resolution — Fix 4K Sources

PiKVM's capture chip supports up to 1080p. If your source outputs 4K, the signal won't lock. The EDID panel:

- **Check Input** — shows the currently negotiated source resolution (reads `v4l2-ctl` timings over SSH)
- **Force 1080p** — injects a 1080p EDID via `kvmd-edidconf` (tries hardware presets v4→v3→v2→v1 automatically), restarts kvmd to persist across reboots, triggers HPD renegotiation
- **Restore Default** — resets EDID back to device default

No manual SSH required. Output log shows every step.

### go2rtc WebRTC — One-Click Install

Installs **go2rtc** as a systemd service on the PiKVM over SSH:

1. Detects architecture (arm64 / arm / amd64)
2. Downloads latest go2rtc binary from GitHub
3. Creates a socat bridge: uStreamer Unix socket → TCP 8082
4. Writes go2rtc config for H.264 transcoding via ffmpeg
5. Enables + starts the service

Health monitor (Server-Sent Events) shows go2rtc up/down events in real time. Auto-restarts on failure via the watcher.

### Remote Diagnostics

One-click over SSH:

- KVMD, kernel, and streamer versions
- Uptime, CPU temperature (color-coded: green < 55°C, yellow 55–70°C, red > 70°C)
- Disk and RAM usage
- Network interfaces (eth0, wlan0) with IP addresses and WiFi SSID
- Status of 7 systemd services: `kvmd`, `kvmd-nginx`, `kvmd-oled`, `kvmd-vnc`, `kvmd-janus`, `go2rtc`, `ustreamer-bridge`

### Firmware Upgrade

Runs `pikvm-update --no-reboot` via SSH. Live streaming output to a retro-terminal-style panel in the browser (Server-Sent Events). Color-coded by line type: errors red, warnings yellow, success green. Update check reports pending package versions before you commit to running the upgrade.

### H.264 Bitrate Tuning

Slider from 1,000 to 20,000 kbps. Default PiKVM is 5,000; recommended for quality is 10,000. Writes to `/etc/kvmd/override.yaml` on the device and restarts kvmd. Reference markers in the UI show default, recommended, and max.

---

## Quick Start

```bash
# Build
go build -o pikvm ./cmd/pikvm/

# Configure
cp .env.example .env
# Edit .env — set PIKVM_HOST, PIKVM_USER, PIKVM_PASS, PIKVM_SSH_PASS

# Start the web dashboard
./pikvm server          # http://localhost:8095
./pikvm server -P 9000  # custom port
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PIKVM_HOST` | — | Hostname or IP (no `https://` needed) |
| `PIKVM_USER` | `admin` | PiKVM web username |
| `PIKVM_PASS` | `admin` | PiKVM web password |
| `PIKVM_SSH_USER` | `root` | SSH username |
| `PIKVM_SSH_PASS` | — | SSH password (key auth tried first) |
| `PIKVM_SSH_KEY` | — | Path to SSH private key |
| `PORT` | `8095` | Web server port |

## CLI Usage

The binary also ships a CLI for scripting and automation:

```bash
pikvm type "Hello World"
pikvm type "guten tag" --keymap de
pikvm key Enter
pikvm shortcut ControlLeft,AltLeft,Delete
pikvm power on / off / off-hard / reset
pikvm screenshot --out /tmp/s.jpg
pikvm msd connect / disconnect
pikvm gpio pulse __v3_usb_breaker__
```

Key names follow the [Web KeyboardEvent.code](https://developer.mozilla.org/en-US/docs/Web/API/UI_Events/Keyboard_event_code_values) spec (`KeyA`, `ControlLeft`, `F1`, etc.).

## Notes

- PiKVM uses a self-signed cert — TLS verification is skipped automatically
- 2FA: append your TOTP code directly to the password, no separator
- SSH access is required for diagnostics, go2rtc install, EDID changes, upgrade, and bitrate tuning
- Key auth is attempted before password fallback for SSH

## deck-hub Integration

```json
{
  "id": "pikvm",
  "display_name": "PiKVM Control",
  "command": ["$GOPROJECTS/pikvm-key-cli/pikvm", "server"],
  "cwd": "$GOPROJECTS/pikvm-key-cli",
  "env": { "PORT": "8095" },
  "upstream": "http://127.0.0.1:8095",
  "health_path": "/api/health"
}
```

## Stack

Go · [Templ](https://templ.guide) · Alpine.js · [HTMX](https://htmx.org) · Tailwind CSS 4 · DaisyUI 5
