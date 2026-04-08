package server

import (
	"net/http"
	"strings"
)

// diagCmd runs several probes in one SSH round-trip, each line prefixed with a key.
const diagCmd = `
echo "KVMD:$(pacman -Q kvmd 2>/dev/null | awk '{print $2}')" &&
echo "KERNEL:$(uname -r)" &&
echo "UPTIME:$(uptime -p)" &&
echo "DISK:$(df -h / | tail -1 | awk '{print $3"/"$2" ("$5" used)"}')" &&
echo "TEMP:$(vcgencmd measure_temp 2>/dev/null | cut -d= -f2 || echo n/a)" &&
echo "MEM:$(free -h | awk '/^Mem/{print $3"/"$2}')" &&
echo "SVC_KVMD:$(systemctl is-active kvmd 2>/dev/null)" &&
echo "SVC_OLED:$(systemctl is-active kvmd-oled 2>/dev/null)" &&
echo "SVC_NGINX:$(systemctl is-active kvmd-nginx 2>/dev/null)" &&
echo "SVC_VNC:$(systemctl is-active kvmd-vnc 2>/dev/null)" &&
echo "SVC_WEBTERM:$(systemctl is-active kvmd-webterm 2>/dev/null)" &&
echo "SVC_WPA:$(systemctl is-active wpa_supplicant@wlan0 2>/dev/null)" &&
echo "NET_ETH:$(ip -br addr show eth0 2>/dev/null | awk '{print $3}' || echo n/a)" &&
echo "NET_WLAN:$(ip -br addr show wlan0 2>/dev/null | awk '{print $3}' || echo n/a)" &&
echo "WIFI_SSID:$(wpa_cli -i wlan0 status 2>/dev/null | grep '^ssid=' | cut -d= -f2 || echo n/a)"
`

type diagResult struct {
	KVMDVersion string            `json:"kvmd_version"`
	Kernel      string            `json:"kernel"`
	Uptime      string            `json:"uptime"`
	DiskRoot    string            `json:"disk_root"`
	CPUTemp     string            `json:"cpu_temp"`
	Memory      string            `json:"memory"`
	Services    map[string]string `json:"services"`
	Network     map[string]string `json:"network"`
	WifiSSID    string            `json:"wifi_ssid"`
}

func (h *handlers) diag(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	host := h.host
	sshUser := h.sshUser
	sshPass := h.sshPass
	sshKeyPath := h.sshKeyPath
	h.mu.RUnlock()

	if host == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"success": false, "error": "no PiKVM host configured",
		})
		return
	}

	out, err := sshRun(stripScheme(host), sshUser, sshPass, sshKeyPath, diagCmd)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": false, "error": err.Error(),
		})
		return
	}

	result := diagResult{
		Services: map[string]string{},
		Network:  map[string]string{},
	}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := line[:idx]
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "KVMD":
			result.KVMDVersion = val
		case "KERNEL":
			result.Kernel = val
		case "UPTIME":
			result.Uptime = val
		case "DISK":
			result.DiskRoot = val
		case "TEMP":
			result.CPUTemp = val
		case "MEM":
			result.Memory = val
		case "NET_ETH":
			result.Network["eth0"] = val
		case "NET_WLAN":
			result.Network["wlan0"] = val
		case "WIFI_SSID":
			result.WifiSSID = val
		default:
			if strings.HasPrefix(key, "SVC_") {
				result.Services[strings.ToLower(strings.TrimPrefix(key, "SVC_"))] = val
		}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data":    result,
	})
}
