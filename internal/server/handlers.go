package server

import (
	"bytes"
	"context"
	"fmt"
	stdjpeg "image/jpeg"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"pikvm-key-cli/internal/client"
)

type handlers struct {
	mu         sync.RWMutex
	kvm        *client.Client
	host       string
	user       string
	pass       string
	sshUser    string
	sshPass    string
	sshKeyPath string
	envPath    string
	log        *log.Logger
}

func (h *handlers) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// requireKVM checks that a KVM client is configured, returns it if so.
func (h *handlers) requireKVM(w http.ResponseWriter) *client.Client {
	h.mu.RLock()
	kvm := h.kvm
	h.mu.RUnlock()
	if kvm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"success": false, "error": "no PiKVM host configured — set PIKVM_HOST",
		})
		return nil
	}
	return kvm
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	writeOK(w, "pikvm-ui running")
}

func (h *handlers) captureLog(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data":    kvm.CaptureLog(),
	})
}

func (h *handlers) info(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	info, err := kvm.Info(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *handlers) state(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()

	type combined struct {
		ATX      interface{} `json:"atx"`
		HID      interface{} `json:"hid"`
		MSD      interface{} `json:"msd"`
		Streamer interface{} `json:"streamer"`
	}
	var out combined
	atx, err := kvm.ATXState(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	out.ATX = atx

	hid, err := kvm.HIDState(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	out.HID = hid

	msd, err := kvm.MSDState(ctx)
	if err != nil {
		out.MSD = map[string]interface{}{"error": err.Error()}
	} else {
		out.MSD = msd
	}

	streamer, err := kvm.StreamerState(ctx)
	if err != nil {
		out.Streamer = map[string]interface{}{"error": err.Error()}
	} else {
		out.Streamer = streamer
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": out})
}

func (h *handlers) screenshot(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	data, err := kvm.Screenshot(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// ocr captures a screenshot, crops to the requested region (x,y,w,h as 0-1
// fractions of the image), runs tesseract, and returns the extracted text.
func (h *handlers) ocr(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}

	q := r.URL.Query()
	parseF := func(k string) float64 {
		v, _ := strconv.ParseFloat(q.Get(k), 64)
		return v
	}
	fx, fy, fw, fh := parseF("x"), parseF("y"), parseF("w"), parseF("h")
	if fw <= 0 || fh <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid region"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	jpeg, err := kvm.Screenshot(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}

	// Decode JPEG to get dimensions
	imgCfg, err := stdjpeg.DecodeConfig(bytes.NewReader(jpeg))
	if err != nil {
		writeErr(w, fmt.Errorf("decode screenshot: %w", err))
		return
	}
	iw, ih := float64(imgCfg.Width), float64(imgCfg.Height)

	// Convert fractions to pixel coordinates
	x0 := int(fx * iw)
	y0 := int(fy * ih)
	x1 := int((fx + fw) * iw)
	y1 := int((fy + fh) * ih)
	if x0 < 0 { x0 = 0 }
	if y0 < 0 { y0 = 0 }
	if x1 > int(iw) { x1 = int(iw) }
	if y1 > int(ih) { y1 = int(ih) }

	// Write cropped JPEG to temp file, run tesseract
	tmp, err := os.CreateTemp("", "pikvm-ocr-*.jpg")
	if err != nil {
		writeErr(w, err)
		return
	}
	defer os.Remove(tmp.Name())

	// Crop, upscale 4x lanczos, convert to greyscale, then adaptive threshold
	// to produce a clean black-on-white image — tesseract's sweet spot.
	// eq=contrast=1.5:brightness=0.05 boosts faint text before thresholding.
	vf := fmt.Sprintf(
		"crop=%d:%d:%d:%d,scale=iw*4:ih*4:flags=lanczos,hqdn3d=1.5:1.5:6:6,"+
			"eq=contrast=1.8:brightness=0.05,format=gray,"+
			"unsharp=5:5:2.0:5:5:0.0",
		x1-x0, y1-y0, x0, y0)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-loglevel", "quiet",
		"-i", "pipe:0",
		"-vf", vf,
		"-frames:v", "1", "-f", "image2", tmp.Name(), "-y")
	cmd.Stdin = bytes.NewReader(jpeg)
	if out, err := cmd.CombinedOutput(); err != nil {
		writeErr(w, fmt.Errorf("crop: %w — %s", err, string(out)))
		return
	}
	tmp.Close()

	// PaddleOCR path — significantly better accuracy on compressed/low-quality images.
	// Falls back to tesseract if PaddleOCR is not installed.
	if _, perr := exec.LookPath("paddleocr"); perr == nil {
		paddle := exec.CommandContext(ctx, "python3", "-c", fmt.Sprintf(`
import sys
from paddleocr import PaddleOCR
ocr = PaddleOCR(use_angle_cls=True, lang='en', use_gpu=False, show_log=False)
result = ocr.ocr(%q, cls=True)
lines = []
for block in (result or []):
    if block:
        for line in block:
            if line and len(line) > 1:
                lines.append(line[1][0])
print('\n'.join(lines))
`, tmp.Name()))
		if paddleOut, err := paddle.Output(); err == nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"text":    strings.TrimSpace(string(paddleOut)),
				"engine":  "paddleocr",
			})
			return
		}
	}

	// Tesseract fallback — single-block PSM works well for UI text regions
	tess := exec.CommandContext(ctx, "tesseract", tmp.Name(), "stdout", "--psm", "6")
	tessOut, err := tess.Output()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "tesseract not available — run: sudo apt-get install tesseract-ocr",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"text":    strings.TrimSpace(string(tessOut)),
		"engine":  "tesseract",
	})
}

// ocrCheck reports which OCR engine is available.
func (h *handlers) ocrCheck(w http.ResponseWriter, r *http.Request) {
	if _, err := exec.LookPath("paddleocr"); err == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"available": true, "engine": "paddleocr"})
		return
	}
	if _, err := exec.LookPath("tesseract"); err == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"available": true, "engine": "tesseract"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"available": false,
		"install":   "sudo apt-get install tesseract-ocr  # or: pip install paddlepaddle paddleocr",
	})
}

// mjpegStream serves a continuous MJPEG stream.
//
// The ?mode= query parameter selects the capture strategy:
//
//   - "tunnel" — SSH direct-tcpip to uStreamer (127.0.0.1:8080). Full frame rate,
//     no re-encoding. Returns 503 if SSH is not configured or the tunnel fails.
//   - "poll"   — Three-tier polling loop (HTTP → SHM → V4L2) at ~3 fps.
//   - ""       — Tunnel with automatic poll fallback (original default behaviour).
func (h *handlers) mjpegStream(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}

	mode := r.URL.Query().Get("mode")

	// SSH tunnel path
	if mode == "tunnel" || mode == "" {
		if kvm.HasSSH() {
			t0 := time.Now()
			if err := kvm.StreamViaSSHTunnel(r.Context(), w); err == nil {
				return
			} else if mode == "tunnel" {
				// Explicit tunnel mode — don't fall back, surface the error
				kvm.LogCapture("tunnel", false, 0, "SSH tunnel failed: "+err.Error(), time.Since(t0))
				http.Error(w, "SSH tunnel unavailable: "+err.Error(), http.StatusServiceUnavailable)
				return
			} else {
				kvm.LogCapture("tunnel", false, 0, "fallback to poll: "+err.Error(), time.Since(t0))
				h.log.Printf("SSH tunnel stream failed, falling back to poll: %v", err)
			}
		} else if mode == "tunnel" {
			http.Error(w, "SSH not configured", http.StatusServiceUnavailable)
			return
		}
	}

	// Polling MJPEG path (~3 fps, three-tier capture pipeline)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	const boundary = "pikvm_frame"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	for {
		if r.Context().Err() != nil {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		data, err := kvm.Screenshot(ctx)
		cancel()

		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		_, err = fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n",
			boundary, len(data))
		if err != nil {
			return
		}
		if _, err = w.Write(data); err != nil {
			return
		}
		fmt.Fprintf(w, "\r\n") //nolint:errcheck
		flusher.Flush()

		time.Sleep(333 * time.Millisecond)
	}
}

func (h *handlers) typeText(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}
	text := r.FormValue("text")
	keymap := r.FormValue("keymap")
	ctx, cancel := h.ctx()
	defer cancel()
	if err := kvm.TypeText(ctx, text, keymap); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, "text sent")
}

func (h *handlers) sendKey(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}
	key := r.FormValue("key")
	ctx, cancel := h.ctx()
	defer cancel()
	if err := kvm.SendKey(ctx, key); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, "key sent")
}

func (h *handlers) shortcut(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}
	keys := r.FormValue("keys")
	ctx, cancel := h.ctx()
	defer cancel()
	if err := kvm.SendShortcut(ctx, keys); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, "shortcut sent")
}

func (h *handlers) power(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}
	action := strings.ReplaceAll(r.FormValue("action"), "-", "_")
	ctx, cancel := h.ctx()
	defer cancel()
	if err := kvm.Power(ctx, action); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, "power action sent: "+action)
}

func (h *handlers) msdConnect(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	if err := kvm.MSDConnect(ctx, true); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, "MSD connected")
}

func (h *handlers) msdDisconnect(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	if err := kvm.MSDConnect(ctx, false); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, "MSD disconnected")
}

// bitrateGet reads the current h264_bitrate default from /etc/kvmd/override.yaml on the PiKVM.
func (h *handlers) bitrateGet(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	out, err := kvm.SSHRun(ctx, `python3 -c "
import yaml, sys
try:
    d = yaml.safe_load(open('/etc/kvmd/override.yaml')) or {}
    v = d.get('kvmd',{}).get('streamer',{}).get('h264_bitrate',{})
    print(v.get('default', 5000) if isinstance(v, dict) else v if isinstance(v, int) else 5000)
except: print(5000)
"`)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "bitrate": 5000, "note": "could not read override.yaml"})
		return
	}
	bitrate, _ := strconv.Atoi(strings.TrimSpace(out))
	if bitrate <= 0 {
		bitrate = 5000
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "bitrate": bitrate})
}

// bitrateSet writes h264_bitrate to /etc/kvmd/override.yaml on the PiKVM and restarts kvmd.
func (h *handlers) bitrateSet(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}
	bitrate, err := strconv.Atoi(r.FormValue("bitrate"))
	if err != nil || bitrate < 1000 || bitrate > 20000 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "bitrate must be 1000–20000"})
		return
	}

	// Python snippet: load override.yaml (creating if absent), set h264_bitrate, write back, restart kvmd
	script := fmt.Sprintf(`python3 -c "
import yaml, subprocess
path = '/etc/kvmd/override.yaml'
try:
    d = yaml.safe_load(open(path)) or {}
except: d = {}
d.setdefault('kvmd', {}).setdefault('streamer', {})['h264_bitrate'] = {'default': %d, 'max': 20000}
open(path, 'w').write(yaml.dump(d, default_flow_style=False))
subprocess.run(['systemctl', 'restart', 'kvmd'])
print('ok')
"`, bitrate)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := kvm.SSHRun(ctx, "rw && "+script+"; ro || true")
	if err != nil {
		writeErr(w, fmt.Errorf("SSH bitrate set: %w — %s", err, out))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "bitrate": bitrate, "message": fmt.Sprintf("H.264 bitrate set to %d kbps — kvmd restarted", bitrate)})
}

func (h *handlers) gpioPulse(w http.ResponseWriter, r *http.Request) {
	kvm := h.requireKVM(w)
	if kvm == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}
	channel := r.FormValue("channel")
	ctx, cancel := h.ctx()
	defer cancel()
	if err := kvm.GPIOPulse(ctx, channel); err != nil {
		writeErr(w, err)
		return
	}
	writeOK(w, "GPIO pulsed: "+channel)
}
