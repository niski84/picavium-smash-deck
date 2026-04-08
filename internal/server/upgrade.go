package server

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const upgradeCmd = `pikvm-update --no-reboot 2>&1; echo "=== UPGRADE COMPLETE ==="`

// ── Job state ─────────────────────────────────────────────────────────────────

type upgradeJob struct {
	logPath string
	done    chan struct{}
}

var (
	jobMu  sync.Mutex
	curJob *upgradeJob
)

// ── sshConfig builds an SSH client config, key auth preferred over password ──

func sshConfig(user, pass, keyPath string) (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod

	if keyPath != "" {
		raw, err := os.ReadFile(keyPath)
		if err == nil {
			signer, err := ssh.ParsePrivateKey(raw)
			if err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}

	if pass != "" {
		authMethods = append(authMethods, ssh.Password(pass))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no SSH auth available (no key or password configured)")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         15 * time.Second,
	}, nil
}

// ── sshRun runs a single command and returns combined output ──────────────────

func sshRun(host, user, pass, keyPath, cmd string) (string, error) {
	cfg, err := sshConfig(user, pass, keyPath)
	if err != nil {
		return "", err
	}
	conn, err := ssh.Dial("tcp", host+":22", cfg)
	if err != nil {
		return "", fmt.Errorf("SSH dial %s: %w", host, err)
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

// ── upgradeCheck: list pending packages ──────────────────────────────────────

func (h *handlers) upgradeCheck(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	host, sshUser, sshPass, sshKeyPath := h.host, h.sshUser, h.sshPass, h.sshKeyPath
	h.mu.RUnlock()

	if host == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"success": false, "error": "no PiKVM host configured",
		})
		return
	}

	// pacman -Qu exits 1 when nothing needs updating — not an error.
	// Wrap with `|| true` so sshRun never sees a non-zero exit.
	out, err := sshRun(stripScheme(host), sshUser, sshPass, sshKeyPath,
		"pacman -Sy --noconfirm >/dev/null 2>&1; pacman -Qu 2>&1 || true")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("SSH error: %v", err),
		})
		return
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	count := 0
	var pkgs []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			count++
			pkgs = append(pkgs, l)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true, "pending": count, "packages": pkgs,
	})
}

// ── upgradeRun: start async job, return immediately ───────────────────────────

func (h *handlers) upgradeRun(w http.ResponseWriter, r *http.Request) {
	jobMu.Lock()
	if curJob != nil {
		select {
		case <-curJob.done: // previous finished, OK
		default:
			jobMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]interface{}{
				"success": false, "error": "upgrade already running",
			})
			return
		}
	}
	logPath := fmt.Sprintf("/tmp/pikvm-upgrade-%d.log", time.Now().UnixNano())
	job := &upgradeJob{logPath: logPath, done: make(chan struct{})}
	curJob = job
	jobMu.Unlock()

	go h.runUpgradeAsync(job)

	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// ── upgradeStream: SSE that tails the log file ────────────────────────────────

func (h *handlers) upgradeStream(w http.ResponseWriter, r *http.Request) {
	jobMu.Lock()
	job := curJob
	jobMu.Unlock()

	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "error": "no upgrade running or started",
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sendLine := func(line string) {
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", "↵"))
		flusher.Flush()
	}

	var offset int64
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}

		f, err := os.Open(job.logPath)
		if err != nil {
			// File not created yet — wait
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			offset += int64(len(line)) + 1 // +1 for newline
			sendLine(line)
		}
		f.Close()

		// Check if job finished
		select {
		case <-job.done:
			// Drain any remaining lines (flush above already read them)
			fmt.Fprintf(w, "event: done\ndata: ok\n\n")
			flusher.Flush()
			return
		default:
		}
	}
}

// ── runUpgradeAsync: SSH + write to log file ──────────────────────────────────

func (h *handlers) runUpgradeAsync(job *upgradeJob) {
	defer close(job.done)

	f, err := os.Create(job.logPath)
	if err != nil {
		return
	}
	defer f.Close()

	writeLine := func(s string) {
		fmt.Fprintln(f, s)
		_ = f.Sync()
	}

	h.mu.RLock()
	host, sshUser, sshPass, sshKeyPath := h.host, h.sshUser, h.sshPass, h.sshKeyPath
	h.mu.RUnlock()

	if host == "" {
		writeLine("ERROR: no PiKVM host configured")
		return
	}

	writeLine("Connecting to " + stripScheme(host) + " via SSH...")

	cfg, err := sshConfig(sshUser, sshPass, sshKeyPath)
	if err != nil {
		writeLine("ERROR: " + err.Error())
		return
	}

	conn, err := ssh.Dial("tcp", stripScheme(host)+":22", cfg)
	if err != nil {
		writeLine("ERROR: SSH connect failed — " + err.Error())
		return
	}
	defer conn.Close()

	writeLine("Connected. Running pikvm-update --no-reboot ...")
	writeLine("")

	session, err := conn.NewSession()
	if err != nil {
		writeLine("ERROR: " + err.Error())
		return
	}
	defer session.Close()

	pr, pw := io.Pipe()
	session.Stdout = pw
	session.Stderr = pw

	if err := session.Start(upgradeCmd); err != nil {
		writeLine("ERROR: " + err.Error())
		return
	}

	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- session.Wait()
		pw.Close()
	}()

	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		writeLine(scanner.Text())
	}

	if err := <-cmdDone; err != nil {
		writeLine("ERROR: " + err.Error())
	}
}

func stripScheme(host string) string {
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	return host
}
