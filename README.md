# PiKVM Smash Deck

A replacement web UI for PiKVM v3 that closes most of the v3-to-v4 gap in software, plus features the v4 stock UI still does not have.

Part of the [Smash Deck](https://github.com/niski84/smash-deck-catalog) family - self-hosted dashboards built in Go for the homelab.

## What It Does

Fixes the headline PiKVM v3 problem (4K source signals causing a black screen on the 1080p capture chip) by injecting a 1080p EDID over SSH using `kvmd-edidconf`. It tries hardware presets v4, v3, v2, then v1 automatically, restarts kvmd so the fix persists, and triggers an HPD pulse to force the GPU to renegotiate. There is a Restore Default action to roll it back.

Replaces the stock MJPEG stream with WebRTC. It installs go2rtc as a systemd service over SSH (one click), bridges the uStreamer Unix socket to TCP, and serves H.264 video via WHEP at roughly 30ms end-to-end latency.

Adds the things the stock UI is missing: H.264 / H.265 MP4 segment recording with a playback timeline, OCR over a drag-selected region (PaddleOCR with a Tesseract fallback), text macros with token templates and human-mode typing, remote diagnostics (CPU temp, disk, RAM, network, systemd service status over SSH), one-click `pikvm-update` with streaming terminal output, and a bitrate slider that writes `/etc/kvmd/override.yaml` and restarts kvmd.

## Tech Stack

- Go (single binary, no runtime dependencies)
- `spf13/cobra` for CLI commands
- `a-h/templ` for server-rendered components
- `golang.org/x/crypto/ssh` for SSH-driven configuration
- Docker / Compose support included

## Running

```bash
go build -o pikvm ./cmd/pikvm
./pikvm
```

Configure via environment variables (see `.env.example`). Requires PiKVM web credentials and SSH access to the device. Default port is 8095.

## Status

Active development.

## License

MIT
