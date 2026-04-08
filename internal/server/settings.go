package server

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"

	"pikvm-key-cli/internal/client"
)

func (h *handlers) settingsGet(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":          true,
		"host":             h.host,
		"user":             h.user,
		"ssh_user":         h.sshUser,
		"ssh_pass_set":     h.sshPass != "",
		"ssh_key_path":     h.sshKeyPath,
		// Passwords are never sent back to the client
	})
}

func (h *handlers) settingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, err)
		return
	}

	host := strings.TrimSpace(r.FormValue("host"))
	user := strings.TrimSpace(r.FormValue("user"))
	pass := r.FormValue("pass")
	sshUser := strings.TrimSpace(r.FormValue("ssh_user"))
	sshPass := r.FormValue("ssh_pass")
	sshKeyPath := strings.TrimSpace(r.FormValue("ssh_key_path"))

	h.mu.Lock()
	if host != "" {
		h.host = host
	}
	if user != "" {
		h.user = user
	}
	if pass != "" {
		h.pass = pass
	}
	if sshUser != "" {
		h.sshUser = sshUser
	}
	if sshPass != "" {
		h.sshPass = sshPass
	}
	if sshKeyPath != "" {
		h.sshKeyPath = sshKeyPath
	}

	// Rebuild KVM client with updated credentials
	newHost := h.host
	newUser := h.user
	newPass := h.pass
	newSSHUser := h.sshUser
	newSSHPass := h.sshPass
	newSSHKey := h.sshKeyPath
	h.mu.Unlock()

	if newHost != "" {
		url := newHost
		if !strings.HasPrefix(url, "http") {
			url = "https://" + url
		}
		h.mu.Lock()
		h.kvm = client.New(url, newUser, newPass)
		h.kvm.SetSSH(newHost, newSSHUser, newSSHPass, newSSHKey)
		h.mu.Unlock()
	}

	// Persist non-empty values to .env
	if h.envPath != "" {
		updates := map[string]string{}
		if host != "" {
			updates["PIKVM_HOST"] = host
		}
		if user != "" {
			updates["PIKVM_USER"] = user
		}
		if pass != "" {
			updates["PIKVM_PASS"] = pass
		}
		if len(updates) > 0 {
			if err := updateEnvFile(h.envPath, updates); err != nil {
				h.log.Printf("warning: failed to save .env: %v", err)
			}
		}
	}

	writeOK(w, "settings saved")
}

// updateEnvFile rewrites only the specified keys in an .env file, preserving all other lines.
func updateEnvFile(path string, updates map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var lines []string
	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if idx := strings.Index(line, "="); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			if val, ok := updates[key]; ok {
				lines = append(lines, fmt.Sprintf(`%s="%s"`, key, val))
				seen[key] = true
				continue
			}
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return err
	}

	// Append any keys not already present
	for k, v := range updates {
		if !seen[k] {
			lines = append(lines, fmt.Sprintf(`%s="%s"`, k, v))
		}
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
