package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"pikvm-key-cli/internal/client"
	"pikvm-key-cli/internal/server/views"
	"pikvm-key-cli/web"
)

// Run starts the web dashboard server.
func Run(port, host, user, pass string) error {
	logger := log.New(os.Stdout, "[pikvm-ui] ", log.LstdFlags)

	// Resolve project root for .env persistence
	exe, _ := os.Executable()
	envPath := filepath.Join(filepath.Dir(exe), ".env")
	if _, err := os.Stat(envPath); err != nil {
		// Fallback: walk up from cwd
		if cwd, err2 := os.Getwd(); err2 == nil {
			envPath = filepath.Join(cwd, ".env")
		}
	}

	sshUser := os.Getenv("PIKVM_SSH_USER")
	if sshUser == "" {
		sshUser = "root"
	}
	sshPass := os.Getenv("PIKVM_SSH_PASS")
	if sshPass == "" {
		sshPass = pass // fall back to web password
	}
	sshKeyPath := os.Getenv("PIKVM_SSH_KEY")

	h := &handlers{
		host:       host,
		user:       user,
		pass:       pass,
		sshUser:    sshUser,
		sshPass:    sshPass,
		sshKeyPath: sshKeyPath,
		envPath:    envPath,
		log:        logger,
	}

	if host != "" {
		url := host
		if !strings.HasPrefix(url, "http") {
			url = "https://" + url
		}
		h.kvm = client.New(url, user, pass)
		h.kvm.SetSSH(host, sshUser, sshPass, sshKeyPath)
		logger.Printf("PiKVM target: %s (user: %s)", host, user)

		// Push quality=100 to uStreamer so HTTP snapshots are full quality.
		// Best-effort: ignore errors (streamer may not be running yet).
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.kvm.SetStreamerQuality(ctx, 100); err != nil {
				logger.Printf("note: SetStreamerQuality: %v", err)
			}
		}()
	} else {
		logger.Println("WARNING: no PIKVM_HOST set — device actions will fail")
	}

	staticFS, err := fs.Sub(web.FS, "pikvm/static")
	if err != nil {
		return err
	}

	mux := http.NewServeMux()

	// Templ-rendered SPA page
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		views.IndexPage().Render(r.Context(), w)
	})

	// Static assets (CSS, JS, images)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Device API proxies
	mux.HandleFunc("GET /api/health", h.health)
	mux.HandleFunc("GET /api/info", h.info)
	mux.HandleFunc("GET /api/state", h.state)
	mux.HandleFunc("GET /api/screenshot", h.screenshot)
	mux.HandleFunc("GET /api/stream", h.mjpegStream)
	mux.HandleFunc("GET /api/ocr", h.ocr)
	mux.HandleFunc("GET /api/ocr/check", h.ocrCheck)
	mux.HandleFunc("GET /api/bitrate", h.bitrateGet)
	mux.HandleFunc("POST /api/bitrate", h.bitrateSet)
	mux.HandleFunc("POST /api/type", h.typeText)
	mux.HandleFunc("POST /api/type-macro", h.typeMacro)
	mux.HandleFunc("POST /api/key", h.sendKey)
	mux.HandleFunc("POST /api/shortcut", h.shortcut)
	mux.HandleFunc("POST /api/power", h.power)
	mux.HandleFunc("POST /api/msd/connect", h.msdConnect)
	mux.HandleFunc("POST /api/msd/disconnect", h.msdDisconnect)
	mux.HandleFunc("POST /api/gpio/pulse", h.gpioPulse)

	// Diagnostics
	mux.HandleFunc("GET /api/diag", h.diag)
	mux.HandleFunc("GET /api/capture-log", h.captureLog)

	// Viewer launcher
	mux.HandleFunc("POST /api/viewer/launch", h.launchViewer)

	// EDID management (force 1080p on 4K source)
	mux.HandleFunc("GET /api/edid/status", h.edidStatus)
	mux.HandleFunc("POST /api/edid/set", h.edidSet)
	mux.HandleFunc("POST /api/edid/restore", h.edidRestore)

	// go2rtc WHEP streaming + background health watcher
	watcher := newGo2rtcWatcher(h, logger)
	mux.HandleFunc("GET /api/go2rtc/status", h.go2rtcStatus)
	mux.HandleFunc("GET /api/go2rtc/events", watcher.HandleEvents)
	mux.HandleFunc("POST /api/go2rtc/install", h.go2rtcInstall)
	mux.HandleFunc("POST /api/whep", h.whepProxy)
	mux.HandleFunc("OPTIONS /api/whep", h.whepProxy)

	// Upgrade
	mux.HandleFunc("GET /api/upgrade/check", h.upgradeCheck)
	mux.HandleFunc("POST /api/upgrade/run", h.upgradeRun)
	mux.HandleFunc("GET /api/upgrade/stream", h.upgradeStream)

	// Settings
	mux.HandleFunc("GET /api/settings", h.settingsGet)
	mux.HandleFunc("POST /api/settings", h.settingsSave)

	// Recording
	projectDir := filepath.Dir(envPath)
	recDataDir := filepath.Join(projectDir, "data", "recordings")
	_ = os.MkdirAll(recDataDir, 0o755)
	rec := NewRecorder(
		filepath.Join(projectDir, "data", "recording-config.json"),
		recDataDir,
		func() *client.Client {
			h.mu.RLock()
			defer h.mu.RUnlock()
			return h.kvm
		},
		logger,
	)
	rec.Start()

	mux.HandleFunc("GET /api/recording/config", rec.HandleConfig)
	mux.HandleFunc("POST /api/recording/config", rec.HandleConfig)
	mux.HandleFunc("GET /api/recording/status", rec.HandleStatus)
	mux.HandleFunc("GET /api/recording/days", rec.HandleDays)
	mux.HandleFunc("GET /api/recording/frames", rec.HandleFrames)
	mux.HandleFunc("GET /api/recording/frame", rec.HandleFrame)
	mux.HandleFunc("GET /api/recording/segments", rec.HandleSegments)
	mux.HandleFunc("GET /api/recording/videofile", rec.HandleVideoFile)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if host != "" {
		watcher.Start(ctx)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Println("shutting down...")
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Printf("dashboard at http://127.0.0.1:%s/", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-ctx.Done()
	return nil
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": msg})
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": fmt.Sprintf("%v", err)})
}
