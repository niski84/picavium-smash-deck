// Package client provides a Go client for the PiKVM HTTP API.
// All requests skip TLS verification (PiKVM uses self-signed certs by default).
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client talks to a PiKVM device over HTTPS.
type Client struct {
	base    string
	user    string
	pass    string
	http    *http.Client
	sshHost string
	sshCfg  *ssh.ClientConfig // nil if SSH not configured

	// Stale frame detection: track the MD5 of the last SHM frame
	lastSHMHash [16]byte
	shmStale    bool // true if last SHM frame was identical to the one before

	// Cached V4L2 format — queried on first use, reset on failure
	v4l2FmtOnce sync.Once
	v4l2FmtOK   bool   // true if format was successfully queried
	v4l2W       int
	v4l2H       int
	v4l2Stride  int    // bytesperline
	v4l2PixFmt  string // ffmpeg pixel_format name

	// Cached ATX / signal state — polled cheaply before the screenshot cascade
	signalMu      sync.Mutex
	signalOK      bool      // true = power on + HDMI signal present
	signalChecked time.Time // zero = never checked

	// Capture log ring buffer
	logMu        sync.Mutex
	logBuf       []CaptureLogEntry
	logSize      int
	logMuteUntil time.Time
}

// ErrNoSignal is returned by Screenshot when the ATX power is off or HDMI is absent.
var ErrNoSignal = fmt.Errorf("no signal — machine powered off or HDMI disconnected")

// CaptureLogEntry records a single capture attempt for debugging.
type CaptureLogEntry struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"` // "http", "shm", "v4l2"
	OK     bool      `json:"ok"`
	Size   int       `json:"size"` // bytes
	Msg    string    `json:"msg"`
	Ms     int64     `json:"ms"` // duration in millis
}

// New creates a PiKVM client. host should be "https://192.168.x.y" or "https://pikvm".
func New(host, user, pass string) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &Client{
		base:    strings.TrimRight(host, "/"),
		user:    user,
		pass:    pass,
		// No hard client-level timeout — callers control deadlines via context.
		// This allows long-running operations (human-mode typing) without HTTP timeout.
		http: &http.Client{Transport: transport},
		logSize: 200,
	}
}

// checkSignal returns true if ATX power is on AND an HDMI signal is present.
// The result is cached for 10 seconds so rapid calls are cheap.
func (c *Client) checkSignal(ctx context.Context) bool {
	c.signalMu.Lock()
	defer c.signalMu.Unlock()
	if time.Since(c.signalChecked) < 10*time.Second {
		return c.signalOK
	}
	data, err := c.get(ctx, "/api/state")
	if err != nil {
		// If we can't reach the API at all, assume signal unknown → try anyway
		return true
	}
	var state struct {
		OK     bool `json:"ok"`
		Result struct {
			ATX struct {
				Result *struct {
					LEDs struct {
						Power bool `json:"power"`
					} `json:"leds"`
				} `json:"result"`
			} `json:"atx"`
			Streamer struct {
				Result *struct {
					Streamer *struct {
						Source *struct {
							Online bool `json:"online"`
						} `json:"source"`
					} `json:"streamer"`
				} `json:"result"`
			} `json:"streamer"`
		} `json:"result"`
	}
	// /api/state wraps data under "result" not "data"
	var wrapper struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return true
	}
	if err := json.Unmarshal(wrapper.Data, &state.Result); err != nil {
		return true
	}
	power := state.Result.ATX.Result != nil && state.Result.ATX.Result.LEDs.Power
	streamerResult := state.Result.Streamer.Result
	hdmi := streamerResult != nil && streamerResult.Streamer != nil &&
		streamerResult.Streamer.Source != nil && streamerResult.Streamer.Source.Online
	c.signalOK = power || hdmi // either power on OR signal present counts
	c.signalChecked = time.Now()
	return c.signalOK
}

// SignalOK returns the last cached signal state (power on or HDMI present).
// Returns true if the cache is empty (unknown → optimistic).
func (c *Client) SignalOK() bool {
	c.signalMu.Lock()
	defer c.signalMu.Unlock()
	if c.signalChecked.IsZero() {
		return true // not yet checked
	}
	return c.signalOK
}

// SetSSH configures SSH credentials for fallback operations (e.g. SHM snapshot).
// sshHost is host:port (port defaults to 22). keyPath may be "" to use password auth.
func (c *Client) SetSSH(sshHost, sshUser, sshPass, keyPath string) {
	// Strip any URL scheme (e.g. "https://") that may have been passed in
	if i := strings.Index(sshHost, "://"); i >= 0 {
		sshHost = sshHost[i+3:]
	}
	if !strings.Contains(sshHost, ":") {
		sshHost += ":22"
	}
	authMethods := []ssh.AuthMethod{}
	if keyPath != "" {
		if key, err := os.ReadFile(keyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(key); err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}
	if sshPass != "" {
		authMethods = append(authMethods, ssh.Password(sshPass))
	}
	c.sshHost = sshHost
	c.sshCfg = &ssh.ClientConfig{
		User:            sshUser,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
}

// captureLog appends an entry to the ring buffer.
func (c *Client) captureLog(method string, ok bool, size int, msg string, dur time.Duration) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	entry := CaptureLogEntry{
		Time:   time.Now(),
		Method: method,
		OK:     ok,
		Size:   size,
		Msg:    msg,
		Ms:     dur.Milliseconds(),
	}
	if len(c.logBuf) >= c.logSize {
		c.logBuf = c.logBuf[1:]
	}
	c.logBuf = append(c.logBuf, entry)
}

// LogCapture appends an entry to the capture log ring buffer.
func (c *Client) LogCapture(method string, ok bool, size int, msg string, dur time.Duration) {
	c.captureLog(method, ok, size, msg, dur)
}

// CaptureLog returns a copy of the capture log entries.
func (c *Client) CaptureLog() []CaptureLogEntry {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	out := make([]CaptureLogEntry, len(c.logBuf))
	copy(out, c.logBuf)
	return out
}

// screenshotViaSHM SSHes into the PiKVM and reads the JPEG shared-memory sink
// that uStreamer writes to. The sink persists even when uStreamer is not running,
// so this always returns the most recently captured frame (which may be stale).
func (c *Client) screenshotViaSHM(ctx context.Context) ([]byte, error) {
	if c.sshCfg == nil {
		return nil, fmt.Errorf("SSH not configured")
	}
	conn, err := ssh.Dial("tcp", c.sshHost, c.sshCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH dial: %w", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("SSH session: %w", err)
	}
	defer sess.Close()

	out, err := sess.Output("ustreamer-dump --sink 'kvmd::ustreamer::jpeg' --output - --count 1 --sink-timeout 3 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("ustreamer-dump: %w", err)
	}

	// Check for stale data
	hash := md5.Sum(out)
	if hash == c.lastSHMHash {
		c.shmStale = true
	} else {
		c.shmStale = false
	}
	c.lastSHMHash = hash

	return out, nil
}

// snapshotViaGo2rtcRTSP grabs a single JPEG frame from go2rtc's internal RTSP
// server via SSH. go2rtc exposes streams by name at rtsp://127.0.0.1:8554/{name}.
// This works when go2rtc holds /dev/video0 (common on PiKVM v3+), making it the
// primary fallback when KVMD's uStreamer is not running.
func (c *Client) snapshotViaGo2rtcRTSP(ctx context.Context) ([]byte, error) {
	if c.sshCfg == nil {
		return nil, fmt.Errorf("SSH not configured")
	}
	conn, err := ssh.Dial("tcp", c.sshHost, c.sshCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH dial: %w", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("SSH session: %w", err)
	}
	defer sess.Close()

	var buf bytes.Buffer
	sess.Stdout = &buf

	// Grab one H264 frame from go2rtc's RTSP server and decode to JPEG.
	// -frames:v 1 stops after the first complete frame.
	// -f image2pipe -vcodec mjpeg writes a raw JPEG to stdout.
	err = sess.Run("ffmpeg -loglevel quiet -rtsp_transport tcp" +
		" -i rtsp://127.0.0.1:8554/kvm" +
		" -frames:v 1 -f image2pipe -vcodec mjpeg -q:v 2 - 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("go2rtc RTSP: ffmpeg: %w", err)
	}
	if buf.Len() < 1000 {
		return nil, fmt.Errorf("go2rtc RTSP: frame too small (%d bytes)", buf.Len())
	}
	return buf.Bytes(), nil
}

// snapshotViaSSHTunnel grabs a single JPEG frame from uStreamer's internal
// HTTP server (127.0.0.1:8080) via an SSH direct-tcpip channel.
// This works even when the PiKVM KVMD API returns 503 — as long as uStreamer
// is running and has HDMI signal, its internal listener serves snapshots.
func (c *Client) snapshotViaSSHTunnel(ctx context.Context) ([]byte, error) {
	if c.sshCfg == nil {
		return nil, fmt.Errorf("SSH not configured")
	}
	conn, err := ssh.Dial("tcp", c.sshHost, c.sshCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH dial: %w", err)
	}
	defer conn.Close()

	remote, err := conn.Dial("tcp", "127.0.0.1:8080")
	if err != nil {
		return nil, fmt.Errorf("tunnel to uStreamer: %w", err)
	}
	defer remote.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080/?action=snapshot", nil)
	if err != nil {
		return nil, err
	}
	if err := req.Write(remote); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(remote), req)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("uStreamer snapshot: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) < 1000 {
		return nil, fmt.Errorf("uStreamer snapshot too small (%d bytes)", len(data))
	}
	return data, nil
}

// HasSSH reports whether SSH credentials have been configured.
func (c *Client) HasSSH() bool { return c.sshCfg != nil }

// SSHRun executes a shell command on the PiKVM via SSH and returns combined output.
func (c *Client) SSHRun(ctx context.Context, cmd string) (string, error) {
	if c.sshCfg == nil {
		return "", fmt.Errorf("SSH not configured")
	}
	conn, err := ssh.Dial("tcp", c.sshHost, c.sshCfg)
	if err != nil {
		return "", fmt.Errorf("SSH dial: %w", err)
	}
	defer conn.Close()
	sess, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("SSH session: %w", err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	return strings.TrimSpace(string(out)), err
}

// StreamViaSSHTunnel proxies uStreamer's native MJPEG stream through an SSH
// direct-tcpip channel to the browser. uStreamer listens on 127.0.0.1:8080
// inside the PiKVM and serves a standard multipart/x-mixed-replace MJPEG stream
// at its full configured frame rate. This bypasses the polling loop entirely —
// no re-encoding, no 3 fps cap.
//
// Returns nil when the client disconnects (ctx cancelled or write error).
// Returns a non-nil error only when the tunnel cannot be established at all,
// so the caller can fall back without having written any response headers yet.
func (c *Client) StreamViaSSHTunnel(ctx context.Context, w http.ResponseWriter) error {
	if c.sshCfg == nil {
		return fmt.Errorf("SSH not configured")
	}
	conn, err := ssh.Dial("tcp", c.sshHost, c.sshCfg)
	if err != nil {
		return fmt.Errorf("SSH dial: %w", err)
	}
	defer conn.Close()

	// direct-tcpip to uStreamer's internal HTTP listener (PiKVM default: 127.0.0.1:8080)
	remote, err := conn.Dial("tcp", "127.0.0.1:8080")
	if err != nil {
		return fmt.Errorf("uStreamer tunnel: %w", err)
	}
	defer remote.Close()

	// Send HTTP request for the MJPEG stream
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080/?action=stream", nil)
	if err != nil {
		return err
	}
	if err := req.Write(remote); err != nil {
		return fmt.Errorf("HTTP request write: %w", err)
	}

	// Read response headers through the tunnel
	resp, err := http.ReadResponse(bufio.NewReader(remote), req)
	if err != nil {
		return fmt.Errorf("HTTP response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("uStreamer: HTTP %d", resp.StatusCode)
	}

	// Forward headers verbatim so the browser sees the correct multipart boundary
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return nil
		}
	}
}

// queryV4L2Format uses v4l2-ctl to read the actual device format (width, height,
// bytes-per-line, pixel format). Called once and cached on c.v4l2*.
// Before reading, it attempts to set the format to 1920×1080 for maximum
// resolution; the device will silently clamp to its highest supported mode if
// 1080p is not available.
func (c *Client) queryV4L2Format(conn *ssh.Client) {
	// Attempt to push 1080p; ignore errors (device may not support it)
	if setSess, err := conn.NewSession(); err == nil {
		_ = setSess.Run("v4l2-ctl --device /dev/video0 --set-fmt-video=width=1920,height=1080,pixelformat=YUYV 2>/dev/null")
		setSess.Close()
	}

	sess, err := conn.NewSession()
	if err != nil {
		return
	}
	defer sess.Close()
	out, err := sess.Output("v4l2-ctl --device /dev/video0 --get-fmt-video 2>&1")
	if err != nil {
		return
	}
	text := string(out)

	// Parse "Width/Height      : 1280/720"
	if i := strings.Index(text, "Width/Height"); i >= 0 {
		line := text[i:]
		if j := strings.Index(line, ":"); j >= 0 {
			wh := strings.TrimSpace(line[j+1:])
			if k := strings.IndexByte(wh, '\n'); k >= 0 {
				wh = wh[:k]
			}
			parts := strings.SplitN(wh, "/", 2)
			if len(parts) == 2 {
				c.v4l2W, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
				c.v4l2H, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
			}
		}
	}

	// Parse "Bytes per Line    : 2560"
	if i := strings.Index(text, "Bytes per Line"); i >= 0 {
		line := text[i:]
		if j := strings.Index(line, ":"); j >= 0 {
			val := strings.TrimSpace(line[j+1:])
			if k := strings.IndexByte(val, '\n'); k >= 0 {
				val = val[:k]
			}
			c.v4l2Stride, _ = strconv.Atoi(strings.TrimSpace(val))
		}
	}

	// Parse pixel format: 'YUYV' or 'UYVY'
	if strings.Contains(text, "'YUYV'") {
		c.v4l2PixFmt = "yuyv422"
	} else if strings.Contains(text, "'UYVY'") {
		c.v4l2PixFmt = "uyvy422"
	}

	// Fallbacks
	if c.v4l2W == 0 {
		c.v4l2W = 1280
	}
	if c.v4l2H == 0 {
		c.v4l2H = 720
	}
	if c.v4l2Stride == 0 {
		c.v4l2Stride = c.v4l2W * 2
	}
	if c.v4l2PixFmt == "" {
		c.v4l2PixFmt = "yuyv422" // YUYV is more common on Pi unicam
	}
	c.v4l2FmtOK = true
}

// screenshotViaV4L2 grabs a frame directly from the HDMI capture device over SSH.
// The raw data is piped to local ffmpeg for JPEG conversion.
// This works even when uStreamer is offline and the SHM sink is stale.
func (c *Client) screenshotViaV4L2(ctx context.Context) ([]byte, error) {
	if c.sshCfg == nil {
		return nil, fmt.Errorf("SSH not configured")
	}

	conn, err := ssh.Dial("tcp", c.sshHost, c.sshCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH dial: %w", err)
	}
	defer conn.Close()

	// Query V4L2 format on first successful call (re-queries if previous attempt failed)
	c.v4l2FmtOnce.Do(func() { c.queryV4L2Format(conn) })

	// Grab raw frame
	sess, err := conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("SSH session: %w", err)
	}
	defer sess.Close()

	// --stream-skip=2: discard 2 buffers (avoids partial scans and stale queued
	// frames), then capture the next complete one.
	// --stream-to=-: write raw frame to stdout.
	rawData, err := sess.Output("v4l2-ctl --device /dev/video0 --stream-mmap --stream-skip=2 --stream-count=1 --stream-to=- 2>/dev/null")
	if err != nil {
		// Reset format cache so next attempt re-queries (resolution may have changed)
		c.v4l2FmtOnce = sync.Once{}
		return nil, fmt.Errorf("v4l2-ctl: %w", err)
	}

	stride := c.v4l2Stride
	contentW := c.v4l2W
	contentH := c.v4l2H
	pixFmt := c.v4l2PixFmt
	frameSize := stride * contentH

	if len(rawData) < frameSize {
		// Reset format cache — resolution or pixel format may have changed
		c.v4l2FmtOnce = sync.Once{}
		if len(rawData) == 0 {
			return nil, fmt.Errorf("v4l2 returned no data — device may be busy (uStreamer) or no HDMI signal")
		}
		return nil, fmt.Errorf("v4l2: got %d bytes, expected %d (%dx%d %s) — resolution may have changed", len(rawData), frameSize, contentW, contentH, pixFmt)
	}

	frame := rawData[:frameSize]

	args := []string{
		"-loglevel", "quiet",
		"-f", "rawvideo",
		"-pixel_format", pixFmt,
		"-video_size", fmt.Sprintf("%dx%d", contentW, contentH),
		"-i", "pipe:0",
		"-frames:v", "1",
		"-vf", "unsharp=5:5:1.0:5:5:0.5",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", "1",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = bytes.NewReader(frame)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg v4l2→jpeg (%dx%d): %w", contentW, contentH, err)
	}
	if outBuf.Len() < 1000 {
		return nil, fmt.Errorf("ffmpeg produced tiny output (%d bytes)", outBuf.Len())
	}
	return outBuf.Bytes(), nil
}


func (c *Client) req(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-KVMD-User", c.user)
	req.Header.Set("X-KVMD-Passwd", c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.http.Do(req)
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.req(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// postQuery sends a POST with params as URL query string (not body).
// The PiKVM API reads all action params from the query string, not the request body.
func (c *Client) postQuery(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	if len(params) > 0 {
		q := make([]string, 0, len(params))
		for k, v := range params {
			q = append(q, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
		path = path + "?" + strings.Join(q, "&")
	}
	resp, err := c.req(ctx, http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// --- Info ---

// Info returns the raw /api/info JSON.
func (c *Client) Info(ctx context.Context) (map[string]interface{}, error) {
	b, err := c.get(ctx, "/api/info")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}

// --- HID ---

// TypeText sends text to the target via /api/hid/print.
func (c *Client) TypeText(ctx context.Context, text, keymap string) error {
	path := "/api/hid/print"
	if keymap != "" {
		path += "?keymap=" + keymap
	}
	resp, err := c.req(ctx, http.MethodPost, path, strings.NewReader(text))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// TypeTextWithDelay sends text with a per-keystroke delay in seconds.
// PiKVM's /api/hid/print?delay= is in seconds (e.g. 0.08 = 80ms). Max is 5.0.
func (c *Client) TypeTextWithDelay(ctx context.Context, text, keymap string, delaySec float64) error {
	path := "/api/hid/print"
	sep := "?"
	if keymap != "" {
		path += sep + "keymap=" + url.QueryEscape(keymap)
		sep = "&"
	}
	if delaySec > 0 {
		path += sep + fmt.Sprintf("delay=%.3f", delaySec)
	}
	resp, err := c.req(ctx, http.MethodPost, path, strings.NewReader(text))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// SendKey sends a single key press+release.
func (c *Client) SendKey(ctx context.Context, key string) error {
	// Press
	if _, err := c.postQuery(ctx, "/api/hid/events/send_key", map[string]string{
		"key": key, "state": "1",
	}); err != nil {
		return err
	}
	// Release
	_, err := c.postQuery(ctx, "/api/hid/events/send_key", map[string]string{
		"key": key, "state": "0",
	})
	return err
}

// SendShortcut sends a key combo (e.g., "ControlLeft,AltLeft,Delete").
func (c *Client) SendShortcut(ctx context.Context, keys string) error {
	_, err := c.postQuery(ctx, "/api/hid/events/send_shortcut", map[string]string{
		"keys": keys,
	})
	return err
}

// HIDState returns the raw /api/hid JSON.
func (c *Client) HIDState(ctx context.Context) (map[string]interface{}, error) {
	b, err := c.get(ctx, "/api/hid")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}

// --- ATX Power ---

// Power sends an ATX power action. action: on | off | off_hard | reset_hard.
func (c *Client) Power(ctx context.Context, action string) error {
	_, err := c.postQuery(ctx, "/api/atx/power", map[string]string{"action": action})
	return err
}

// ATXState returns the raw /api/atx JSON.
func (c *Client) ATXState(ctx context.Context) (map[string]interface{}, error) {
	b, err := c.get(ctx, "/api/atx")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}

// --- Screenshot ---

// WakeStreamer sends a reset to start the uStreamer pipeline.
func (c *Client) WakeStreamer(ctx context.Context) error {
	resp, err := c.req(ctx, http.MethodPost, "/api/streamer/reset", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) //nolint:errcheck
	return nil
}

// Screenshot downloads a JPEG snapshot using a four-tier fallback cascade:
//  1. HTTP /api/streamer/snapshot — fast path when KVMD's uStreamer is active
//  2. go2rtc MJPEG — direct HTTP to :1984 when go2rtc holds the device (PiKVM v3+)
//  3. SSH → ustreamer-dump SHM sink — stale-checked, avoids device contention
//  4. SSH → uStreamer direct (127.0.0.1:8080 tunnel)
//
// On persistent failure all intermediate log entries are muted for 60s to
// prevent log flooding when the recording loop fires every few seconds.
func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	muted := time.Now().Before(c.logMuteUntil)

	logf := func(method string, ok bool, size int, msg string, dur time.Duration) {
		if !muted || ok {
			c.captureLog(method, ok, size, msg, dur)
		}
	}

	// Tier 1: HTTP snapshot via KVMD API
	t0 := time.Now()
	data, err := c.doSnapshot(ctx)
	if err == nil {
		c.logMuteUntil = time.Time{} // clear mute on success
		c.shmStale = false
		logf("http", true, len(data), "OK (streamer active)", time.Since(t0))
		return data, nil
	}
	httpErr := err.Error()
	logMsg := httpErr
	if len(logMsg) > 80 {
		logMsg = logMsg[:80] + "..."
	}
	logf("http", false, 0, logMsg, time.Since(t0))

	// Fast exit: if ATX says machine is powered off and no HDMI signal, skip SSH tiers.
	// checkSignal is cached for 10s so this is a cheap poll, not a per-screenshot call.
	if !c.checkSignal(ctx) {
		// Update mute to avoid flooding the log with "no signal" entries
		if time.Now().After(c.logMuteUntil) {
			c.captureLog("state", false, 0, "skipping capture — machine off or no HDMI signal", 0)
		}
		c.logMuteUntil = time.Now().Add(60 * time.Second)
		return nil, ErrNoSignal
	}

	// Tier 2: go2rtc RTSP — SSH into PiKVM and grab one frame from go2rtc's internal
	// RTSP server (rtsp://127.0.0.1:8554/kvm). Works when go2rtc holds /dev/video0.
	if c.sshCfg != nil {
		t2 := time.Now()
		data, err = c.snapshotViaGo2rtcRTSP(ctx)
		if err == nil {
			c.logMuteUntil = time.Time{}
			logf("go2rtc", true, len(data), "OK (go2rtc RTSP)", time.Since(t2))
			return data, nil
		}
		logf("go2rtc", false, 0, err.Error(), time.Since(t2))
	}

	if c.sshCfg == nil {
		c.logMuteUntil = time.Now().Add(60 * time.Second)
		return nil, fmt.Errorf("no capture source (KVMD 503, go2rtc unreachable, SSH not configured)")
	}

	// Tier 3: SHM sink — skip if stale
	if !c.shmStale {
		t3 := time.Now()
		data, err = c.screenshotViaSHM(ctx)
		if err == nil && !c.shmStale {
			c.logMuteUntil = time.Time{}
			logf("shm", true, len(data), "OK (fresh)", time.Since(t3))
			return data, nil
		}
		if err != nil {
			logf("shm", false, 0, err.Error(), time.Since(t3))
		} else {
			logf("shm", false, len(data), "stale", time.Since(t3))
		}
	}

	// Tier 4: SSH tunnel to uStreamer at 127.0.0.1:8080
	{
		t4 := time.Now()
		data, err = c.snapshotViaSSHTunnel(ctx)
		if err == nil {
			c.logMuteUntil = time.Time{}
			logf("ssh-snap", true, len(data), "OK (uStreamer direct)", time.Since(t4))
			return data, nil
		}
		logf("ssh-snap", false, 0, err.Error(), time.Since(t4))
	}

	// All tiers failed — mute repeated failures for 60s to prevent log flooding
	if time.Now().After(c.logMuteUntil) {
		c.captureLog("all", false, 0, "all capture tiers failed — suppressing for 60s", 0)
	}
	c.logMuteUntil = time.Now().Add(60 * time.Second)

	if strings.Contains(httpErr, "503") {
		return nil, fmt.Errorf("no capture source — KVMD uStreamer offline, go2rtc unreachable or no active stream")
	}
	return nil, fmt.Errorf("screenshot failed: %s", logMsg)
}

func (c *Client) doSnapshot(ctx context.Context) ([]byte, error) {
	// No quality parameter here — the snapshot returns at the streamer's current
	// configured quality. We push quality=100 once at startup via SetStreamerQuality.
	resp, err := c.req(ctx, http.MethodGet, "/api/streamer/snapshot", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(resp.Body)
}

// SetStreamerQuality sets the uStreamer JPEG quality (0–100) via the PiKVM API.
// Call once after connecting; the value persists for the session.
func (c *Client) SetStreamerQuality(ctx context.Context, quality int) error {
	_, err := c.postQuery(ctx, "/api/streamer/set_params", map[string]string{
		"quality": strconv.Itoa(quality),
	})
	return err
}

// --- MSD ---

// MSDState returns the raw /api/msd JSON.
func (c *Client) StreamerState(ctx context.Context) (map[string]interface{}, error) {
	b, err := c.get(ctx, "/api/streamer")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}

func (c *Client) MSDState(ctx context.Context) (map[string]interface{}, error) {
	b, err := c.get(ctx, "/api/msd")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}

// MSDConnect connects (1) or disconnects (0) the MSD to the target host.
func (c *Client) MSDConnect(ctx context.Context, connected bool) error {
	val := "0"
	if connected {
		val = "1"
	}
	_, err := c.postQuery(ctx, "/api/msd/set_connected", map[string]string{"connected": val})
	return err
}

// MSDSetImage selects the active MSD image.
func (c *Client) MSDSetImage(ctx context.Context, image string, cdrom bool) error {
	params := map[string]string{"image": image}
	if cdrom {
		params["cdrom"] = "1"
	}
	_, err := c.postQuery(ctx, "/api/msd/set_params", params)
	return err
}

// --- GPIO ---

// GPIOState returns the raw /api/gpio JSON.
func (c *Client) GPIOState(ctx context.Context) (map[string]interface{}, error) {
	b, err := c.get(ctx, "/api/gpio")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	return m, json.Unmarshal(b, &m)
}

// GPIOPulse pulses a GPIO output channel (e.g., "__v3_usb_breaker__").
func (c *Client) GPIOPulse(ctx context.Context, channel string) error {
	_, err := c.postQuery(ctx, "/api/gpio/pulse", map[string]string{"channel": channel})
	return err
}

// GPIOSwitch sets a GPIO output channel state.
func (c *Client) GPIOSwitch(ctx context.Context, channel string, state bool) error {
	val := "0"
	if state {
		val = "1"
	}
	_, err := c.postQuery(ctx, "/api/gpio/switch", map[string]string{
		"channel": channel, "state": val,
	})
	return err
}
