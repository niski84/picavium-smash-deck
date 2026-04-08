package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// watchEvent is a single status update broadcast to SSE subscribers.
type watchEvent struct {
	TS  string `json:"ts"`
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
}

// go2rtcWatcher polls go2rtc health in the background, auto-restarts via SSH
// when it goes down, and streams events to browser clients via SSE.
type go2rtcWatcher struct {
	h   *handlers
	log *log.Logger

	mu        sync.Mutex
	subs      map[chan watchEvent]struct{}
	lastEvent *watchEvent // most recent event, sent to new subscribers immediately
}

func newGo2rtcWatcher(h *handlers, logger *log.Logger) *go2rtcWatcher {
	return &go2rtcWatcher{
		h:    h,
		log:  logger,
		subs: make(map[chan watchEvent]struct{}),
	}
}

// Start launches the watcher goroutine; ctx controls shutdown.
func (w *go2rtcWatcher) Start(ctx context.Context) {
	go w.loop(ctx)
}

func (w *go2rtcWatcher) subscribe() chan watchEvent {
	ch := make(chan watchEvent, 30)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	last := w.lastEvent
	w.mu.Unlock()
	if last != nil {
		ch <- *last // send current state immediately
	}
	return ch
}

func (w *go2rtcWatcher) unsubscribe(ch chan watchEvent) {
	w.mu.Lock()
	delete(w.subs, ch)
	w.mu.Unlock()
}

func (w *go2rtcWatcher) broadcast(ev watchEvent) {
	w.mu.Lock()
	w.lastEvent = &ev
	subs := make([]chan watchEvent, 0, len(w.subs))
	for ch := range w.subs {
		subs = append(subs, ch)
	}
	w.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // drop if subscriber is slow
		}
	}
}

func (w *go2rtcWatcher) emit(ok bool, msg string) {
	ev := watchEvent{TS: time.Now().Format("15:04:05"), OK: ok, Msg: msg}
	w.log.Printf("[go2rtc] %s", msg)
	w.broadcast(ev)
}

func (w *go2rtcWatcher) loop(ctx context.Context) {
	const normalInterval = 30 * time.Second
	const fastInterval = 5 * time.Second

	fails := 0
	checks := 0
	timer := time.NewTimer(0) // fire immediately on start
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		ok := w.poll()
		checks++

		if ok {
			wasDown := fails > 0
			fails = 0
			if wasDown {
				w.emit(true, "go2rtc recovered")
			} else if checks == 1 || checks%10 == 0 {
				// heartbeat on first check and every ~5 min
				w.emit(true, "go2rtc healthy")
			}
			timer.Reset(normalInterval)
		} else {
			fails++
			switch {
			case fails == 1:
				w.emit(false, "go2rtc unreachable — switching to 5s poll")
			case fails == 2:
				w.emit(false, fmt.Sprintf("go2rtc still down (attempt %d) — restarting via SSH…", fails))
				w.tryRestart(ctx)
			case fails%6 == 0:
				// retry restart every 30s after initial
				w.emit(false, fmt.Sprintf("go2rtc still down (attempt %d) — retrying SSH restart…", fails))
				w.tryRestart(ctx)
			default:
				w.emit(false, fmt.Sprintf("go2rtc down (attempt %d)", fails))
			}
			timer.Reset(fastInterval)
		}
	}
}

func (w *go2rtcWatcher) poll() bool {
	w.h.mu.RLock()
	host := w.h.host
	w.h.mu.RUnlock()
	if host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.h.go2rtcBase()+"/api/streams", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (w *go2rtcWatcher) tryRestart(ctx context.Context) {
	w.h.mu.RLock()
	kvm := w.h.kvm
	w.h.mu.RUnlock()
	if kvm == nil || !kvm.HasSSH() {
		w.emit(false, "auto-restart skipped — SSH not configured")
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := kvm.SSHRun(rctx, "systemctl restart go2rtc 2>&1; systemctl is-active go2rtc || true")
	if err != nil {
		w.emit(false, "SSH restart failed: "+err.Error())
		return
	}
	w.emit(false, "SSH restart → "+strings.TrimSpace(out))
}

// HandleEvents serves the SSE stream: GET /api/go2rtc/events
func (w *go2rtcWatcher) HandleEvents(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied

	ch := w.subscribe()
	defer w.unsubscribe(ch)

	flush := func() {
		if f, ok := rw.(http.Flusher); ok {
			f.Flush()
		}
	}
	flush()

	// keepalive comment every 25s so the browser's EventSource doesn't time out
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(rw, ": keepalive\n\n")
			flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(rw, "data: %s\n\n", b)
			flush()
		}
	}
}
