package server

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

func (h *handlers) launchViewer(w http.ResponseWriter, r *http.Request) {
	// Resolve pikvm-viewer relative to this binary's location, then try common paths
	candidates := []string{
		filepath.Join(filepath.Dir(os.Args[0]), "..", "pikvm-viewer"),
		"/home/nick/goprojects/pikvm-viewer",
	}

	var viewerDir string
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "package.json")); err == nil {
			viewerDir = c
			break
		}
	}

	if viewerDir == "" {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false, "error": "pikvm-viewer directory not found",
		})
		return
	}

	cmd := exec.Command("npm", "start")
	cmd.Dir = viewerDir
	cmd.Env = append(os.Environ(), "DISPLAY=:0")

	if err := cmd.Start(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false, "error": "failed to launch viewer: " + err.Error(),
		})
		return
	}

	// Detach — don't wait for it
	go func() { _ = cmd.Wait() }()

	writeOK(w, "viewer launched")
}
