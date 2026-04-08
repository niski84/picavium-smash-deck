package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pikvm-key-cli/internal/client"
)

// ── Config ────────────────────────────────────────────────────────────────────

type RecordingConfig struct {
	Enabled         bool    `json:"enabled"`
	Mode            string  `json:"mode"`             // "jpeg" or "video"
	Days            []int   `json:"days"`             // 0=Sun, 1=Mon ... 6=Sat
	StartHour       int     `json:"start_hour"`
	EndHour         int     `json:"end_hour"`
	IntervalSeconds int     `json:"interval_seconds"`
	RetentionDays   int     `json:"retention_days"`
	MaxSizeMB       int     `json:"max_size_mb"`
	VideoCodec      string  `json:"video_codec"`      // "h264" (default, universal) or "h265" (smaller)
	VideoQuality    int     `json:"video_quality"`    // CRF value (lower = better), default 23 for h264
	VideoPreset     string  `json:"video_preset"`     // ffmpeg preset: ultrafast/fast/medium/slow
	SkipUnchanged   bool    `json:"skip_unchanged"`   // skip frames with no screen changes
	ChangeThreshold float64 `json:"change_threshold"` // 0.0–1.0 similarity threshold, default 0.002
}

func defaultRecordingConfig() RecordingConfig {
	return RecordingConfig{
		Enabled:         false,
		Mode:            "video",
		Days:            []int{1, 2, 3, 4, 5}, // Mon–Fri
		StartHour:       8,
		EndHour:         17,
		IntervalSeconds: 5,
		RetentionDays:   30,
		MaxSizeMB:       5000,
		VideoCodec:      "h264",
		VideoQuality:    23,
		VideoPreset:     "fast",
		SkipUnchanged:   true,
		ChangeThreshold: 0.002,
	}
}

// ── Video writer ──────────────────────────────────────────────────────────────

type videoWriter struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	outPath string
	hour    int
	date    string
}

// ── Recorder ──────────────────────────────────────────────────────────────────

type Recorder struct {
	mu           sync.RWMutex
	cfg          RecordingConfig
	cfgPath      string
	dataDir      string
	kvm          func() *client.Client // getter (kvm can be re-created on settings change)
	log          *log.Logger
	stopCh       chan struct{}
	lastCaptured bool // true if last capture attempt succeeded (signal present)
	vw           *videoWriter
	lastFrame    []byte // previous frame data for change detection

	// Health/stats (atomic for lock-free reads from status handler)
	lastCaptureTime  atomic.Int64 // unix millis
	lastCaptureBytes atomic.Int64
	framesWritten    atomic.Int64
	framesSkipped    atomic.Int64
	captureErrors    atomic.Int64
}

func NewRecorder(cfgPath, dataDir string, kvmGetter func() *client.Client, logger *log.Logger) *Recorder {
	r := &Recorder{
		cfgPath: cfgPath,
		dataDir: dataDir,
		kvm:     kvmGetter,
		log:     logger,
	}
	r.cfg = r.loadConfig()
	return r
}

func (r *Recorder) loadConfig() RecordingConfig {
	cfg := defaultRecordingConfig()
	b, err := os.ReadFile(r.cfgPath)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	if cfg.Mode == "" {
		cfg.Mode = "video"
	}
	if cfg.VideoCodec == "" {
		cfg.VideoCodec = "h264"
	}
	if cfg.VideoQuality == 0 {
		if cfg.VideoCodec == "h265" {
			cfg.VideoQuality = 28
		} else {
			cfg.VideoQuality = 23
		}
	}
	if cfg.VideoPreset == "" {
		cfg.VideoPreset = "fast"
	}
	if cfg.ChangeThreshold == 0 {
		cfg.ChangeThreshold = 0.002
	}
	return cfg
}

func (r *Recorder) saveConfig() error {
	b, err := json.MarshalIndent(r.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.cfgPath, b, 0o644)
}

// Start launches the capture and cleanup goroutines.
func (r *Recorder) Start() {
	r.mu.Lock()
	if r.stopCh != nil {
		close(r.stopCh)
	}
	r.stopCh = make(chan struct{})
	ch := r.stopCh
	r.mu.Unlock()

	go r.captureLoop(ch)
	go r.cleanupLoop(ch)
}

// Restart re-reads config and restarts goroutines.
func (r *Recorder) Restart() {
	r.mu.Lock()
	if r.stopCh != nil {
		close(r.stopCh)
	}
	r.stopCh = make(chan struct{})
	ch := r.stopCh
	r.mu.Unlock()

	go r.captureLoop(ch)
	go r.cleanupLoop(ch)
}

// ── Capture ───────────────────────────────────────────────────────────────────

func (r *Recorder) captureLoop(stop chan struct{}) {
	defer r.closeVideoWriter()

	for {
		r.mu.RLock()
		cfg := r.cfg
		r.mu.RUnlock()

		if cfg.Enabled && r.inSchedule(cfg) {
			ok := r.captureFrame(cfg)
			r.mu.Lock()
			r.lastCaptured = ok
			r.mu.Unlock()
		} else {
			// Outside schedule — close any open video writer to finalise the segment
			r.closeVideoWriter()
			r.mu.Lock()
			r.lastCaptured = false
			r.mu.Unlock()
		}

		interval := time.Duration(cfg.IntervalSeconds) * time.Second
		if interval < time.Second {
			interval = 5 * time.Second
		}
		select {
		case <-stop:
			return
		case <-time.After(interval):
		}
	}
}

func (r *Recorder) inSchedule(cfg RecordingConfig) bool {
	now := time.Now()
	weekday := int(now.Weekday()) // 0=Sun
	for _, d := range cfg.Days {
		if d == weekday {
			h := now.Hour()
			return h >= cfg.StartHour && h < cfg.EndHour
		}
	}
	return false
}

func (r *Recorder) captureFrame(cfg RecordingConfig) bool {
	kvm := r.kvm()
	if kvm == nil {
		r.captureErrors.Add(1)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	data, err := kvm.Screenshot(ctx)
	if err != nil {
		if err != client.ErrNoSignal {
			r.captureErrors.Add(1)
		}
		return false // no signal / power off / unreachable — silently skip
	}

	// Validate: must be a real JPEG (starts with FF D8) and > 1KB
	if len(data) < 1024 || data[0] != 0xFF || data[1] != 0xD8 {
		r.captureErrors.Add(1)
		return false
	}

	// Record capture health stats
	r.lastCaptureTime.Store(time.Now().UnixMilli())
	r.lastCaptureBytes.Store(int64(len(data)))

	// Skip unchanged frames if enabled
	if cfg.SkipUnchanged && r.lastFrame != nil {
		threshold := cfg.ChangeThreshold
		if threshold <= 0 {
			threshold = 0.002
		}
		if framesAreSimilar(r.lastFrame, data, threshold) {
			r.framesSkipped.Add(1)
			return true // "success" — just nothing to write
		}
	}
	r.lastFrame = data

	var ok bool
	if cfg.Mode == "video" {
		ok = r.writeVideoFrame(data)
	} else {
		ok = r.writeJPEGFrame(data)
	}
	if ok {
		r.framesWritten.Add(1)
	}
	return ok
}

// ── Frame comparison ──────────────────────────────────────────────────────────

// framesAreSimilar compares two JPEG frames by decoding them and sampling pixels.
// threshold is in [0,1] — 0 means identical, 0.002 is a good default for "no visible change".
func framesAreSimilar(a, b []byte, threshold float64) bool {
	imgA, errA := jpeg.Decode(bytes.NewReader(a))
	imgB, errB := jpeg.Decode(bytes.NewReader(b))
	if errA != nil || errB != nil {
		return false
	}

	boundsA := imgA.Bounds()
	boundsB := imgB.Bounds()
	if boundsA.Dx() != boundsB.Dx() || boundsA.Dy() != boundsB.Dy() {
		return false // resolution changed
	}

	return imageDiff(imgA, imgB) < threshold
}

// imageDiff returns a normalised 0–1 difference between two same-size images
// by sampling ~1000 evenly-spaced pixels.
func imageDiff(a, b image.Image) float64 {
	bounds := a.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	total := w * h
	const samples = 1000
	step := total / samples
	if step < 1 {
		step = 1
	}

	var sumDiff float64
	n := 0
	for i := 0; i < total; i += step {
		x := i%w + bounds.Min.X
		y := i/w + bounds.Min.Y
		if y >= bounds.Max.Y {
			break
		}

		r1, g1, b1, _ := a.At(x, y).RGBA()
		r2, g2, b2, _ := b.At(x, y).RGBA()

		dr := float64(r1>>8) - float64(r2>>8)
		dg := float64(g1>>8) - float64(g2>>8)
		db := float64(b1>>8) - float64(b2>>8)
		sumDiff += math.Sqrt((dr*dr + dg*dg + db*db) / 3)
		n++
	}
	if n == 0 {
		return 0
	}
	// Normalise: max per-pixel diff is 255
	return (sumDiff / float64(n)) / 255.0
}

// ── JPEG mode ─────────────────────────────────────────────────────────────────

func (r *Recorder) writeJPEGFrame(data []byte) bool {
	r.closeVideoWriter() // ensure no lingering ffmpeg process

	now := time.Now()
	dir := filepath.Join(r.dataDir, now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.log.Printf("recording: mkdir failed: %v", err)
		return false
	}

	fname := filepath.Join(dir, now.Format("15-04-05")+".jpg")
	if err := os.WriteFile(fname, data, 0o644); err != nil {
		r.log.Printf("recording: write failed: %v", err)
		return false
	}
	return true
}

// ── Video mode ────────────────────────────────────────────────────────────────

func (r *Recorder) writeVideoFrame(data []byte) bool {
	now := time.Now()
	if err := r.ensureVideoWriter(now); err != nil {
		r.log.Printf("recording: video writer: %v", err)
		return false
	}
	if r.vw == nil {
		return false
	}
	_, err := r.vw.stdin.Write(data)
	if err != nil {
		r.log.Printf("recording: ffmpeg write: %v", err)
		r.closeVideoWriter()
		return false
	}
	return true
}

func (r *Recorder) ensureVideoWriter(now time.Time) error {
	date := now.Format("2006-01-02")
	hour := now.Hour()

	if r.vw != nil && r.vw.date == date && r.vw.hour == hour {
		return nil // already writing to the right segment
	}

	// Hour or date changed — rotate
	r.closeVideoWriter()

	dir := filepath.Join(r.dataDir, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	r.mu.RLock()
	codec := r.cfg.VideoCodec
	quality := r.cfg.VideoQuality
	preset := r.cfg.VideoPreset
	r.mu.RUnlock()

	if codec == "" {
		codec = "h264"
	}
	if quality <= 0 {
		if codec == "h265" {
			quality = 28
		} else {
			quality = 23
		}
	}
	if preset == "" {
		preset = "fast"
	}

	outPath := filepath.Join(dir, fmt.Sprintf("%02d.mp4", hour))

	// Build ffmpeg args: fragmented MP4 so the file is playable while recording
	args := []string{"-y",
		"-framerate", "1",
		"-f", "image2pipe", "-vcodec", "mjpeg",
		"-i", "pipe:0",
	}

	if codec == "h265" {
		args = append(args,
			"-c:v", "libx265",
			"-tag:v", "hvc1",
			"-x265-params", "no-open-gop=1",
		)
	} else {
		args = append(args,
			"-c:v", "libx264",
			"-profile:v", "high",
			"-pix_fmt", "yuv420p",
			"-tune", "zerolatency", // no buffering — output each frame immediately
		)
	}

	args = append(args,
		"-crf", strconv.Itoa(quality),
		"-preset", preset,
		"-g", "1", // every frame is a keyframe (needed for frag_keyframe to flush per-frame)
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		// Fragmented MP4: playable immediately, grows as frames arrive
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-flush_packets", "1",
		outPath,
	)

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	r.vw = &videoWriter{
		cmd:     cmd,
		stdin:   stdin,
		outPath: outPath,
		hour:    hour,
		date:    date,
	}
	r.log.Printf("recording: started video segment %s/%02d.mp4", date, hour)
	return nil
}

func (r *Recorder) closeVideoWriter() {
	if r.vw == nil {
		return
	}
	vw := r.vw
	r.vw = nil
	_ = vw.stdin.Close()
	go func() {
		if err := vw.cmd.Wait(); err != nil {
			// ffmpeg exits non-zero when stdin closes mid-stream — this is normal
			r.log.Printf("recording: ffmpeg finished %s (exit: %v)", vw.outPath, err)
		} else {
			r.log.Printf("recording: finalised %s", vw.outPath)
		}
	}()
}

// ── Cleanup ───────────────────────────────────────────────────────────────────

func (r *Recorder) cleanupLoop(stop chan struct{}) {
	r.runCleanup() // run once at start
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			r.runCleanup()
		}
	}
}

func (r *Recorder) runCleanup() {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()

	// 1. Age-based: delete directories older than retention
	cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays)
	entries, err := os.ReadDir(r.dataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d, err := time.Parse("2006-01-02", e.Name())
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			path := filepath.Join(r.dataDir, e.Name())
			_ = os.RemoveAll(path)
			r.log.Printf("recording: cleanup removed %s (older than %d days)", e.Name(), cfg.RetentionDays)
		}
	}

	// 2. Size-based: if total > max, delete oldest days
	if cfg.MaxSizeMB > 0 {
		for {
			total := r.totalSizeBytes()
			if total <= int64(cfg.MaxSizeMB)*1024*1024 {
				break
			}
			oldest := r.oldestDay()
			if oldest == "" {
				break
			}
			_ = os.RemoveAll(filepath.Join(r.dataDir, oldest))
			r.log.Printf("recording: cleanup removed %s (total size exceeded %d MB)", oldest, cfg.MaxSizeMB)
		}
	}
}

func (r *Recorder) totalSizeBytes() int64 {
	var total int64
	_ = filepath.WalkDir(r.dataDir, func(_ string, d fs.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func (r *Recorder) oldestDay() string {
	entries, err := os.ReadDir(r.dataDir)
	if err != nil {
		return ""
	}
	var days []string
	for _, e := range entries {
		if e.IsDir() {
			days = append(days, e.Name())
		}
	}
	sort.Strings(days)
	if len(days) == 0 {
		return ""
	}
	return days[0]
}

// ── Stats ─────────────────────────────────────────────────────────────────────

type RecordingStatus struct {
	Recording      bool   `json:"recording"`        // actually writing frames right now
	Scheduled      bool   `json:"scheduled"`        // within schedule window
	Enabled        bool   `json:"enabled"`          // config enabled
	NoSignal       bool   `json:"no_signal"`        // scheduled but no HDMI signal
	Mode           string `json:"mode"`             // "jpeg" or "video"
	TodayFrames    int    `json:"today_frames"`     // JPEG mode
	TodaySegs      int    `json:"today_segs"`       // video mode: segments written today
	TotalSize      string `json:"total_size"`
	TotalDays      int    `json:"total_days"`
	// Stream health
	LastCaptureAgo string `json:"last_capture_ago"` // "3s ago", "never"
	LastCaptureKB  int    `json:"last_capture_kb"`  // size of last captured frame
	FramesWritten  int64  `json:"frames_written"`
	FramesSkipped  int64  `json:"frames_skipped"`
	CaptureErrors  int64  `json:"capture_errors"`
}

func (r *Recorder) Status() RecordingStatus {
	r.mu.RLock()
	cfg := r.cfg
	lastCaptured := r.lastCaptured
	r.mu.RUnlock()

	scheduled := cfg.Enabled && r.inSchedule(cfg)
	recording := scheduled && lastCaptured

	today := time.Now().Format("2006-01-02")
	todayDir := filepath.Join(r.dataDir, today)

	todayFrames := 0
	todaySegs := 0
	if entries, err := os.ReadDir(todayDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".jpg") {
				todayFrames++
			} else if strings.HasSuffix(e.Name(), ".mp4") {
				todaySegs++
			}
		}
	}

	totalBytes := r.totalSizeBytes()
	totalSize := formatBytes(totalBytes)

	totalDays := 0
	if entries, err := os.ReadDir(r.dataDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				totalDays++
			}
		}
	}

	// Stream health
	lastCapMs := r.lastCaptureTime.Load()
	var lastCaptureAgo string
	if lastCapMs == 0 {
		lastCaptureAgo = "never"
	} else {
		ago := time.Since(time.UnixMilli(lastCapMs)).Truncate(time.Second)
		if ago < time.Second {
			lastCaptureAgo = "just now"
		} else {
			lastCaptureAgo = ago.String() + " ago"
		}
	}
	lastCapKB := int(r.lastCaptureBytes.Load() / 1024)

	return RecordingStatus{
		Recording:      recording,
		Scheduled:      scheduled,
		Enabled:        cfg.Enabled,
		NoSignal:       scheduled && !lastCaptured,
		Mode:           cfg.Mode,
		TodayFrames:    todayFrames,
		TodaySegs:      todaySegs,
		TotalSize:      totalSize,
		TotalDays:      totalDays,
		LastCaptureAgo: lastCaptureAgo,
		LastCaptureKB:  lastCapKB,
		FramesWritten:  r.framesWritten.Load(),
		FramesSkipped:  r.framesSkipped.Load(),
		CaptureErrors:  r.captureErrors.Load(),
	}
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ── HTTP Handlers ─────────────────────────────────────────────────────────────

func (r *Recorder) HandleConfig(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet {
		r.mu.RLock()
		cfg := r.cfg
		r.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": cfg})
		return
	}

	// POST — update config
	var cfg RecordingConfig
	if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if cfg.Mode == "" {
		cfg.Mode = "video"
	}
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	if err := r.saveConfig(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	r.Restart()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "config saved"})
}

func (r *Recorder) HandleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": r.Status()})
}

func (r *Recorder) HandleDays(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(r.dataDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": []string{}})
		return
	}
	var days []string
	for _, e := range entries {
		if e.IsDir() {
			days = append(days, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": days})
}

func (r *Recorder) HandleFrames(w http.ResponseWriter, req *http.Request) {
	date := req.URL.Query().Get("date")
	if date == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "date required"})
		return
	}
	// Sanitize
	date = filepath.Base(date)

	dir := filepath.Join(r.dataDir, date)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": []string{}})
		return
	}
	var frames []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jpg") {
			frames = append(frames, strings.TrimSuffix(e.Name(), ".jpg"))
		}
	}
	sort.Strings(frames)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": frames})
}

func (r *Recorder) HandleFrame(w http.ResponseWriter, req *http.Request) {
	date := filepath.Base(req.URL.Query().Get("date"))
	frame := filepath.Base(req.URL.Query().Get("time"))

	path := filepath.Join(r.dataDir, date, frame+".jpg")
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, req, path)
}

// HandleSegments returns the list of video segment hours for a given date.
// Response: {"success":true,"data":["08","09","10",...]}
func (r *Recorder) HandleSegments(w http.ResponseWriter, req *http.Request) {
	date := filepath.Base(req.URL.Query().Get("date"))
	if date == "" || date == "." {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "date required"})
		return
	}

	dir := filepath.Join(r.dataDir, date)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": []string{}})
		return
	}

	var segs []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".mp4") {
			segs = append(segs, strings.TrimSuffix(e.Name(), ".mp4"))
		}
	}
	sort.Strings(segs)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": segs})
}

// HandleVideoFile serves a specific MP4 segment file.
// Query params: date=YYYY-MM-DD&segment=HH
func (r *Recorder) HandleVideoFile(w http.ResponseWriter, req *http.Request) {
	date := filepath.Base(req.URL.Query().Get("date"))
	seg := filepath.Base(req.URL.Query().Get("segment"))

	path := filepath.Join(r.dataDir, date, seg+".mp4")
	if _, err := os.Stat(path); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, req, path)
}
