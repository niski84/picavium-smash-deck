# PiKVM Smash Deck

**A software upgrade for PiKVM v3 — bringing it closer to v4 without buying new hardware.**

If you're running a PiKVM v3, you've probably hit the wall: your source machine outputs 4K and the capture chip sees nothing. Black screen. Unsupported signal. Or it locks on but the image is blurry, badly scaled, or drops out. PiKVM v4 handles this internally — it accepts higher-bandwidth signals and downscales to 1080p on the hardware side. v3 can't do that on its own.

PiKVM Smash Deck fixes that in software. It injects a custom EDID over SSH that tells your GPU to output 1080p instead of 4K — so the v3 capture chip gets a signal it can actually lock on to. That's one feature. It also replaces the stock web UI entirely with a purpose-built dashboard that adds everything the v4 UI still doesn't have: WebRTC streaming, screen recording, OCR, text macros, remote diagnostics, and more.

---

## The v3 → v4 Gap, Closed in Software

| Problem on PiKVM v3 | What Smash Deck Does |
|---|---|
| 4K source → black screen / unsupported signal | Injects 1080p EDID via SSH — GPU downgrades output before it reaches the capture chip |
| Blurry or dropped signal at high resolutions | Forces a stable 1080p lock through `kvmd-edidconf` presets (tries v4→v3→v2→v1 automatically) |
| ~500ms MJPEG latency | go2rtc WebRTC (WHEP) — H.264 at ~30ms latency, same as v4's streaming |
| No screen recording | H.264/H.265 MP4 segments + JPEG frames, scheduled, with playback timeline |
| No OCR | PaddleOCR (AI) + Tesseract fallback — drag a selection box, get text |
| No text automation | `{{token}}` macro templates + human-mode typing (typos, backspaces, natural pace) |
| No remote diagnostics | CPU temp (°C/°F), disk, RAM, network, 7 systemd service statuses over SSH |
| Firmware upgrade is manual SSH | One-click `pikvm-update` with live streaming terminal output in browser |
| Bitrate tuning requires YAML editing | Web UI slider — writes to `/etc/kvmd/override.yaml` and restarts kvmd |

---

## Features

### EDID Fix — The Core v3 Problem

PiKVM v3's capture chip tops out at 1080p. If your PC or console outputs 4K (or any signal above the chip's bandwidth), the result is a black screen or an "unsupported signal" message in the stock UI. PiKVM v4 avoids this by downscaling the signal in hardware before it reaches the capture chip.

Smash Deck solves the same problem in software:

1. **Check Input** — reads the currently negotiated source resolution via `v4l2-ctl` over SSH
2. **Force 1080p** — runs `kvmd-edidconf` to inject a 1080p EDID, tries hardware presets v4→v3→v2→v1 automatically, restarts kvmd so the fix persists across reboots, then triggers an HPD (hot-plug detect) pulse to force the GPU to renegotiate
3. **Restore Default** — removes the custom EDID if you need to revert

No manual SSH, no hex editing. The output log shows every step.

### WebRTC Streaming — v4-Level Latency on v3

The stock PiKVM v3 UI streams MJPEG over HTTP — typically 400–600ms end-to-end. v4 addresses this with better hardware but the latency is still not great out of the box.

Smash Deck installs **go2rtc** as a systemd service on the PiKVM over SSH (one click), then streams H.264 video via WHEP (WebRTC). End-to-end latency drops to ~30ms — the same class as v4's best-case streaming.

The install:
1. Detects architecture (arm64 / arm / amd64)
2. Downloads the latest go2rtc binary from GitHub
3. Creates a socat bridge from the uStreamer Unix socket to TCP 8082
4. Writes a go2rtc config for H.264 transcoding via ffmpeg
5. Enables and starts the service with systemd

Idempotent — re-running it only reinstalls if a newer version is available.

### Four Capture Modes

All modes are available on both the Dashboard and Recording tabs, each with a description shown inline:

- **Auto** — SSH tunnel first, MJPEG poll fallback. Best default.
- **SSH Tunnel** — direct SSH port-forward to uStreamer's internal HTTP server. Full native framerate, ~50–100ms latency. Requires SSH credentials.
- **MJPEG Poll** — three-tier HTTP fallback (kvmd snapshot API → SHM device → V4L2). No SSH needed, ~3 fps.
- **Screenshot** — on-demand JPEG, or auto-refresh every 3 seconds.
- **WebRTC** — H.264 via go2rtc WHEP. ~30ms. Requires go2rtc installed (one click in Settings).

### Screen Recording

Two modes, fully scheduled:

- **Video** — H.264 or H.265 MP4 segments (one per hour), configurable CRF and encoding preset. Fragmented MP4 makes segments playable mid-recording.
- **JPEG** — individual frames at configurable intervals (2–60s).

Schedule by day of week and hour range. Automatic cleanup by age (7–90 days) and max storage (500 MB–10 GB). Smart **frame-skip** detects unchanged screens and saves 50%+ storage when the target machine is idle.

Full playback timeline in the browser: day picker, frame slider, 1x / 10x / 30x / 60x speed, keyboard shortcuts.

### OCR — Read Text Off the Screen

Drag a selection box over the live preview. The server captures a fresh screenshot, crops and upscales 4× with contrast enhancement, then runs PaddleOCR (deep-learning) or Tesseract as fallback. Extracted text appears in a copyable panel. Works in both Dashboard and Recording tabs, including fullscreen.

### Text Macro — Type Anything, Anywhere

Paste text into the macro panel. Define `{{token}} = value` substitution pairs — live preview shows the resolved text before sending.

**Human mode** makes it look like a real person typed it: ~7% adjacent-key typo rate (auto-corrected with Backspace), ~2% double-press rate, 80ms per-keystroke pace. Realistic rhythm that bypasses basic bot detection.

### Remote Diagnostics

Over SSH, one click:

- KVMD, kernel, and streamer versions
- Uptime, CPU temperature (°C / °F, color-coded: green < 55°C, yellow 55–70°C, red > 70°C)
- Disk and RAM usage
- Network interfaces with IP addresses and WiFi SSID
- Status of 7 systemd services: `kvmd`, `kvmd-nginx`, `kvmd-oled`, `kvmd-vnc`, `kvmd-janus`, `go2rtc`, `ustreamer-bridge`

### Live Status Header

Always visible at the top:

- **Power ON/OFF** — derived from ATX LED or HDMI signal presence (ATX LED isn't wired on all v3 builds)
- **HDMI** — shows resolution and captured fps (e.g. `1080p · 50fps`) when signal is present
- **USB HID** — SSH check of the Linux UDC gadget state (`/sys/class/udc/*/state`) — green when `configured` (host has enumerated the USB keyboard+mouse), red when not connected

---

## Quick Start

```bash
# Build
go build -o pikvm ./cmd/pikvm/

# Configure
cp .env.example .env
# Edit .env — set PIKVM_HOST, PIKVM_USER, PIKVM_PASS, PIKVM_SSH_PASS

# Start
./pikvm server          # http://localhost:8095
./pikvm server -P 9000  # custom port
```

## Docker — Quickest Way to Run

A pre-built image is published to the GitHub Container Registry. No Go toolchain or Node.js required.

**Option A — one-liner** (no config file needed):

```bash
docker run -d --name pikvm-smash-deck \
  -p 8095:8095 \
  -e PIKVM_HOST=192.168.1.x \
  -e PIKVM_USER=admin \
  -e PIKVM_PASS=yourpassword \
  -e PIKVM_SSH_USER=root \
  -e PIKVM_SSH_PASS=yourpassword \
  ghcr.io/niski84/picavium-smash-deck:latest
```

Then open `http://localhost:8095`.

**Option B — docker compose** (recommended, keeps credentials in a `.env` file):

```bash
# 1. Grab the compose file
curl -O https://raw.githubusercontent.com/niski84/picavium-smash-deck/main/docker-compose.yml

# 2. Create your config
cp .env.example .env   # or create .env manually — see Environment Variables below
nano .env              # set PIKVM_HOST, PIKVM_PASS, PIKVM_SSH_PASS at minimum

# 3. Start
docker compose up -d

# View logs
docker compose logs -f

# Stop
docker compose down
```

**Updating to the latest image:**

```bash
docker compose pull && docker compose up -d
# or for the one-liner:
docker pull ghcr.io/niski84/picavium-smash-deck:latest
```

**Build locally instead** (if you want to modify the code):

```bash
git clone https://github.com/niski84/picavium-smash-deck.git
cd picavium-smash-deck
docker build --target slim -t picavium-smash-deck .
docker run -p 8095:8095 --env-file .env picavium-smash-deck
```

**Full image with PaddleOCR AI engine** (~2 GB, better OCR accuracy):

```bash
docker build --target full -t picavium-smash-deck:full .
docker run -p 8095:8095 --env-file .env picavium-smash-deck:full
```

The default (`slim`) image includes Tesseract OCR, which works well for most screen text. PaddleOCR handles compression artifacts and unusual fonts better but adds significant image size.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PIKVM_HOST` | — | Hostname or IP (https:// added automatically) |
| `PIKVM_USER` | `admin` | PiKVM web UI username |
| `PIKVM_PASS` | `admin` | PiKVM web UI password (append TOTP if 2FA enabled) |
| `PIKVM_SSH_USER` | `root` | SSH username |
| `PIKVM_SSH_PASS` | — | SSH password (key auth tried first) |
| `PIKVM_SSH_KEY` | — | Path to SSH private key |
| `PORT` | `8095` | Web server listen port |

## CLI

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

Key names follow the [Web KeyboardEvent.code](https://developer.mozilla.org/en-US/docs/Web/API/UI_Events/Keyboard_event_code_values) spec.

## Notes

- PiKVM uses a self-signed TLS cert — verification is skipped automatically
- SSH access is required for EDID fix, go2rtc install, diagnostics, upgrade, and bitrate tuning
- SSH key auth is attempted before password fallback
- 2FA: append your TOTP code directly to the password, no separator needed

## Stack

Go · [Templ](https://templ.guide) · Alpine.js · [HTMX](https://htmx.org) · Tailwind CSS 4 · DaisyUI 5
