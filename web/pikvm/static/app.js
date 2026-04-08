// PiKVM Control — Alpine.js application module
// Extracted from monolithic index.html into proper Smash Deck stack.

function app() {
  return {
    theme: 'dark',
    toasts: [],
    _toastId: 0,
    activeTab: 'dashboard',

    // Dashboard state
    state: null,
    loading: false,
    powerOn: false,
    hdmiSignal: false,
    hdmiRes: '',
    hdmiFps: 0,
    hidKbOnline: null,   // null = unknown, true/false from /api/state hid.result.keyboard.online
    hidMouseOnline: null,
    msdState: null,
    msdSummary: '',
    typeText: '',
    typeKeymap: '',
    customShortcut: '',
    // Live view stream state
    streamMode: localStorage.getItem('streamMode') || 'auto',
    streamKey: Date.now(),
    streamErr: false,

    screenshotSrc: '',
    screenshotTs: '',
    screenshotLoading: false,
    autoRefresh: false,
    _refreshTimer: null,
    sysInfo: null,
    gpioChannel: '',

    // Diagnostics state
    diag: null,
    diagLoading: false,
    diagError: '',

    // Upgrade state
    upgradeVersion: null,
    upgradeChecking: false,
    upgradeCheckResult: null,
    upgradeRunning: false,
    upgradeLog: [],

    // Recording state
    recCfg: { enabled: false, mode: 'video', days: [1,2,3,4,5], start_hour: 8, end_hour: 17, interval_seconds: 5, retention_days: 30, max_size_mb: 5000, video_codec: 'h264', video_quality: 23, video_preset: 'fast', skip_unchanged: true, change_threshold: 0.002 },
    recStatus: null,
    recDays: [],
    recPreviewSrc: '',
    recPreviewMode: localStorage.getItem('recPreviewMode') || 'stream',
    recStreamKey: Date.now(),
    recPreviewTs: '',
    _recPreviewTimer: null,
    // go2rtc / WHEP state
    whepPc: null,
    whepConnecting: false,
    whepStatus: '',
    go2rtcRunning: false,
    go2rtcInstalling: false,
    go2rtcInstallLog: '',
    // go2rtc watcher SSE
    go2rtcEvents: [],   // last 20 events [{ts, ok, msg}]
    _go2rtcEs: null,    // EventSource instance
    playbackDate: '',
    playbackFrames: [],
    playbackIdx: 0,
    playbackPlaying: false,
    playbackSpeed: 30,
    _playTimer: null,
    // Video mode playback
    videoSegments: [],
    videoSegment: '',
    // Capture debug log
    showCaptureLog: false,
    captureLogEntries: [],

    // Preview watchdog
    previewStale: false,
    _previewLastSuccess: 0,
    _previewWatchdog: null,

    // FPS counters
    streamFps: null,
    recFps: null,
    _stopDashFps: null,
    _stopRecFps: null,

    // OCR
    ocrRegion: null,
    ocrDragging: false,
    _ocrStart: null,
    ocrText: null,
    ocrAvailable: true,
    ocrEngine: 'tesseract',
    bitrateValue: 5000,
    bitrateLoading: false,
    bitrateMsg: '',
    bitrateErr: '',
    ocrLoading: false,
    edidLoading: false,
    edidLog: '',
    edidInputRes: '',

    // Macro state
    macroText: '',
    macroTokens: [],
    macroHumanMode: false,
    macroSending: false,

    // Settings state
    settingsLoaded: false,
    settingsSaving: false,
    settings: { host: '', user: '', pass: '', sshUser: '', sshPass: '', sshPassSet: false, sshKeyPath: '' },

    init() {
      const saved = localStorage.getItem('theme') || 'dark';
      this.theme = saved;
      document.documentElement.classList.remove('light','dark');
      document.documentElement.classList.add(saved);
      document.documentElement.setAttribute('data-theme', saved);

      // Restore active tab from localStorage
      const savedTab = localStorage.getItem('activeTab');
      if (savedTab && ['dashboard','diag','recording','upgrade','settings'].includes(savedTab)) {
        this.activeTab = savedTab;
      }

      this.loadState();
      this.loadSettings();
      fetch('/api/ocr/check').then(r=>r.json()).then(d=>{ this.ocrAvailable = d.available !== false; this.ocrEngine = d.engine || 'tesseract'; }).catch(()=>{});

      // If restoring to recording tab, load its data
      if (this.activeTab === 'recording') this.loadRecording();

      // If restoring to settings tab, start the watcher stream
      if (this.activeTab === 'settings') this.startGo2rtcEvents();

      // Watch tab changes to persist + auto-load data
      this.$watch('activeTab', (tab) => {
        localStorage.setItem('activeTab', tab);
        if (tab === 'recording') {
          this.loadRecording();
        } else {
          this.stopRecPreview();
          this.whepStop();
        }
        if (tab === 'settings') {
          this.startGo2rtcEvents();
        } else {
          this.stopGo2rtcEvents();
        }
      });

      this.$nextTick(() => this._startDashFps());
    },

    toggleTheme() {
      this.theme = this.theme === 'dark' ? 'light' : 'dark';
      document.documentElement.classList.remove('light','dark');
      document.documentElement.classList.add(this.theme);
      document.documentElement.setAttribute('data-theme', this.theme);
      localStorage.setItem('theme', this.theme);
    },

    toast(msg, ok = true) {
      const id = ++this._toastId;
      this.toasts.push({ id, msg, ok });
      setTimeout(() => { this.toasts = this.toasts.filter(t => t.id !== id); }, 3500);
    },

    async api(method, path, body) {
      const opts = { method };
      if (body) {
        opts.headers = { 'Content-Type': 'application/x-www-form-urlencoded' };
        opts.body = new URLSearchParams(body).toString();
      }
      const r = await fetch(path, opts);
      return r.json();
    },

    // ── Dashboard ──────────────────────────────────────────────────────────────

    async loadState() {
      this.loading = true;
      try {
        const d = await this.api('GET', '/api/state');
        this.state = d.data;
        const src = d.data?.streamer?.result?.streamer?.source;
        this.hdmiSignal = !!(src?.online);
        if (src?.online) {
          const w = src.resolution?.width || 0, h = src.resolution?.height || 0;
          this.hdmiRes = (w >= 3840 || h >= 2160) ? '4K'
                       : (w >= 1920 || h >= 1080) ? '1080p'
                       : (w >= 1280 || h >= 720)  ? '720p'
                       : w > 0 ? `${w}×${h}` : '';
          this.hdmiFps = Math.round(src.captured_fps || 0);
        }
        // Power: ATX LED OR HDMI signal present (ATX LED may not be wired on all boards)
        const atxPower = !!(d.data?.atx?.result?.leds?.power ?? d.data?.atx?.result?.power);
        this.powerOn = atxPower || this.hdmiSignal;
        if (d.data?.hid?.result) {
          this.hidKbOnline = d.data.hid.result.keyboard?.online ?? null;
          this.hidMouseOnline = d.data.hid.result.mouse?.online ?? null;
        }
        if (d.data?.msd?.result) {
          const m = d.data.msd.result;
          this.msdState = m;
          this.msdSummary = `drive: ${m.drive?.image || 'none'} | connected: ${m.drive?.connected ? 'yes' : 'no'}`;
        }
      } catch(e) {
        console.warn('[pikvm] state error', e);
      } finally {
        this.loading = false;
      }
    },

    async loadInfo() {
      try {
        const d = await this.api('GET', '/api/info');
        this.sysInfo = d.result || d;
      } catch(e) {
        this.toast('Failed to load info', false);
      }
    },

    async loadScreenshot() {
      this.screenshotLoading = true;
      try {
        const r = await fetch('/api/screenshot');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const blob = await r.blob();
        if (this.screenshotSrc) URL.revokeObjectURL(this.screenshotSrc);
        this.screenshotSrc = URL.createObjectURL(blob);
        this.screenshotTs = new Date().toLocaleTimeString();
      } catch(e) {
        this.toast('Screenshot failed: ' + e.message, false);
      } finally {
        this.screenshotLoading = false;
      }
    },

    toggleAutoRefresh() {
      clearInterval(this._refreshTimer);
      if (this.autoRefresh) {
        this.loadScreenshot();
        this._refreshTimer = setInterval(() => this.loadScreenshot(), 3000);
      }
    },

    onStreamModeChange() {
      localStorage.setItem('streamMode', this.streamMode);
      this.streamErr = false;
      this.streamKey = Date.now();
      if (this.streamMode !== 'screenshot') {
        this.autoRefresh = false;
        clearInterval(this._refreshTimer);
      }
      this._startDashFps();
    },

    restartStream() {
      this.streamErr = false;
      this.streamKey = Date.now();
      this._startDashFps();
    },

    async doPower(action) {
      const d = await this.api('POST', '/api/power', { action });
      this.toast(d.message || d.error, d.success !== false);
      if (d.success !== false) setTimeout(() => this.loadState(), 1500);
    },

    async doType() {
      if (this.hidKbOnline === false) {
        this.toast('Keyboard HID offline — check USB OTG cable to target', false);
        return;
      }
      const d = await this.api('POST', '/api/type', { text: this.typeText, keymap: this.typeKeymap });
      this.toast(d.message || d.error, d.success !== false);
    },

    // ── Text Macro ────────────────────────────────────────────────────────────

    macroResolve() {
      let text = this.macroText;
      for (const tok of this.macroTokens) {
        if (tok.key) {
          text = text.replaceAll('{{' + tok.key + '}}', tok.val || '');
        }
      }
      return text;
    },

    macroCharCount() {
      return this.macroResolve().length;
    },

    async sendMacro() {
      if (this.macroSending || !this.macroText.trim()) return;
      if (this.hidKbOnline === false) {
        this.toast('Keyboard HID offline — check USB OTG cable to target', false);
        return;
      }
      this.macroSending = true;
      try {
        const resolved = this.macroResolve();
        const d = await this.api('POST', '/api/type-macro', {
          text: resolved,
          human: this.macroHumanMode ? '1' : '0',
        });
        this.toast(d.message || d.error, d.success !== false);
      } catch(e) {
        this.toast('Macro failed: ' + e.message, false);
      } finally {
        this.macroSending = false;
      }
    },

    async doKey(key) {
      const d = await this.api('POST', '/api/key', { key });
      this.toast(d.message || d.error, d.success !== false);
    },

    async doShortcut(keys) {
      if (!keys?.trim()) return;
      const d = await this.api('POST', '/api/shortcut', { keys });
      this.toast(d.message || d.error, d.success !== false);
    },

    async doMSD(connect) {
      const path = connect ? '/api/msd/connect' : '/api/msd/disconnect';
      const d = await this.api('POST', path);
      this.toast(d.message || d.error, d.success !== false);
      if (d.success !== false) setTimeout(() => this.loadState(), 1000);
    },

    async doGPIOPulse(channel) {
      if (!channel?.trim()) return;
      const d = await this.api('POST', '/api/gpio/pulse', { channel });
      this.toast(d.message || d.error, d.success !== false);
    },

    // ── Viewer ────────────────────────────────────────────────────────────────

    async launchViewer() {
      const d = await this.api('POST', '/api/viewer/launch');
      this.toast(d.message || d.error, d.success !== false);
    },

    // ── Diagnostics ───────────────────────────────────────────────────────────

    async loadDiag() {
      if (this.diagLoading) return;
      this.diagLoading = true;
      this.diagError = '';
      try {
        const d = await this.api('GET', '/api/diag');
        if (d.success) {
          this.diag = d.data;
        } else {
          this.diagError = 'Error: ' + (d.error || 'unknown');
        }
      } catch(e) {
        this.diagError = 'Failed: ' + e.message;
      } finally {
        this.diagLoading = false;
      }
    },

    parseCelsius(s) {
      if (!s) return 0;
      const m = s.match(/([\d.]+)/);
      return m ? parseFloat(m[1]) : 0;
    },
    toFahrenheit(c) { return Math.round(c * 9 / 5 + 32); },

    // ── Upgrade ────────────────────────────────────────────────────────────────

    async loadVersion() {
      try {
        const d = await this.api('GET', '/api/info');
        const r = d.result || {};
        this.upgradeVersion = {
          kvmd:     r.system?.kvmd?.version || '?',
          streamer: r.system?.streamer?.version ? `${r.system.streamer.app} ${r.system.streamer.version}` : '?',
          kernel:   r.system?.kernel?.release || '?',
          platform: r.hw?.platform?.base || '?',
        };
      } catch(e) {
        this.toast('Failed to load version', false);
      }
    },

    async checkUpdates() {
      this.upgradeChecking = true;
      this.upgradeCheckResult = null;
      try {
        const d = await this.api('GET', '/api/upgrade/check');
        this.upgradeCheckResult = d;
        if (!d.success) {
          this.toast(d.error || 'Check failed', false);
        }
      } catch(e) {
        this.toast('Update check failed: ' + e.message, false);
        this.upgradeCheckResult = { success: false, error: e.message };
      } finally {
        this.upgradeChecking = false;
      }
    },

    lineStyle(line) {
      const s = line || '';
      if (s.startsWith('ERROR') || s.includes('error:') || s.includes('failed'))
        return 'color:#ff4444; text-shadow:0 0 6px #ff4444';
      if (s.startsWith('warning') || s.startsWith('WARNING') || s.startsWith('warn'))
        return 'color:#ffff00; text-shadow:0 0 4px #aaaa00';
      if (s.includes('UPGRADE COMPLETE') || s.includes('COMPLETE'))
        return 'color:#00ff41; font-weight:bold; text-shadow:0 0 10px #00ff41';
      if (s.startsWith('::') || s.startsWith('==>') || s.startsWith('Connecting') || s.startsWith('Connected'))
        return 'color:#00ff41; text-shadow:0 0 5px #00aa22';
      if (s.startsWith('(') || s.startsWith('upgrading') || s.startsWith('installing') || s.startsWith('removing'))
        return 'color:#00cc33';
      return 'color:#008822';
    },

    upgradeClear() {
      this.upgradeLog = [];
    },

    async runUpgrade() {
      if (this.upgradeRunning) return;
      this.upgradeRunning = true;
      this.upgradeLog = [];

      let started;
      try {
        started = await this.api('POST', '/api/upgrade/run');
      } catch(e) {
        this.toast('Failed to start upgrade: ' + e.message, false);
        this.upgradeRunning = false;
        return;
      }
      if (!started.success) {
        this.toast(started.error || 'Failed to start upgrade', false);
        this.upgradeRunning = false;
        return;
      }

      const es = new EventSource('/api/upgrade/stream');

      es.onmessage = (e) => {
        if (e.data !== undefined) {
          this.upgradeLog.push(e.data);
          this.$nextTick(() => {
            const box = this.$refs.upgradeLogBox;
            if (box) box.scrollTop = box.scrollHeight;
          });
        }
      };

      es.addEventListener('done', () => {
        es.close();
        this.upgradeRunning = false;
        const last = this.upgradeLog[this.upgradeLog.length - 1] || '';
        if (last.includes('ERROR') || last.includes('error')) {
          this.toast('Upgrade failed — see terminal', false);
        } else {
          this.toast('Upgrade complete!', true);
          this.loadVersion();
        }
      });

      es.onerror = () => {
        // Don't close — reconnect is automatic with EventSource.
      };
    },

    // ── Recording ──────────────────────────────────────────────────────────────

    formatHour12(h) {
      if (h === 0 || h === 24) return '12:00 AM';
      if (h === 12) return '12:00 PM';
      return h < 12 ? h + ':00 AM' : (h - 12) + ':00 PM';
    },

    formatFrameTime12(frame) {
      if (!frame) return '';
      const [hh, mm, ss] = frame.split('-').map(Number);
      const ampm = hh >= 12 ? 'PM' : 'AM';
      const h12 = hh === 0 ? 12 : hh > 12 ? hh - 12 : hh;
      return `${h12}:${String(mm).padStart(2,'0')}:${String(ss).padStart(2,'0')} ${ampm}`;
    },

    formatDate(d) {
      if (!d) return '';
      const dt = new Date(d + 'T12:00:00');
      return dt.toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' });
    },

    async loadRecording() {
      try {
        const [cfgRes, statusRes, daysRes] = await Promise.all([
          this.api('GET', '/api/recording/config'),
          this.api('GET', '/api/recording/status'),
          this.api('GET', '/api/recording/days'),
        ]);
        if (cfgRes.success) this.recCfg = cfgRes.data;
        if (statusRes.success) this.recStatus = statusRes.data;
        if (daysRes.success) this.recDays = daysRes.data || [];
        if (this.recPreviewMode === 'screenshot') {
          this.startRecPreview();
        } else if (this.recPreviewMode === 'webrtc') {
          this.$nextTick(() => this.whepConnect());
        } else {
          this.recStreamKey = Date.now();
          this._startRecFps();
        }
      } catch(e) {
        this.toast('Failed to load recording data', false);
      }
    },

    onRecPreviewModeChange() {
      localStorage.setItem('recPreviewMode', this.recPreviewMode);
      this.stopRecPreview();
      this.whepStop();
      if (this.recPreviewMode === 'webrtc') {
        this.$nextTick(() => this.whepConnect());
      } else if (this.recPreviewMode === 'screenshot') {
        this.startRecPreview();
      } else {
        this.recStreamKey = Date.now();
        this._startRecFps();
      }
    },

    // ── go2rtc / WHEP ─────────────────────────────────────────────────────────

    whepStop() {
      if (this._stopRecFps) { this._stopRecFps(); this._stopRecFps = null; }
      this.recFps = null;
      if (this.whepPc) {
        this.whepPc.close();
        this.whepPc = null;
      }
      this.whepConnecting = false;
      this.whepStatus = '';
      const v = this.$refs.previewVideo;
      if (v) { v.srcObject = null; }
    },

    whepReconnect() {
      this.whepStop();
      this.$nextTick(() => this.whepConnect());
    },

    async whepConnect() {
      if (this.recPreviewMode !== 'webrtc') return;
      this.whepConnecting = true;
      this.whepStatus = 'Creating peer connection…';
      try {
        const pc = new RTCPeerConnection({ iceServers: [] });
        this.whepPc = pc;

        const v = this.$refs.previewVideo;
        pc.addEventListener('track', (ev) => {
          if (v && v.srcObject !== ev.streams[0]) v.srcObject = ev.streams[0];
        });

        pc.addEventListener('connectionstatechange', () => {
          if (pc.connectionState === 'connected') {
            this.whepConnecting = false;
            this._startRecFps();
          } else if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') {
            console.log('[whep] connection', pc.connectionState, '— retrying in 3s');
            this.whepStop();
            setTimeout(() => { if (this.recPreviewMode === 'webrtc') this.whepConnect(); }, 3000);
          }
        });

        pc.addTransceiver('video', { direction: 'recvonly' });

        this.whepStatus = 'Creating offer…';
        const offer = await pc.createOffer();
        await pc.setLocalDescription(offer);

        // go2rtc does not support trickle ICE — wait for full candidate gathering
        this.whepStatus = 'Gathering ICE candidates…';
        await new Promise((resolve) => {
          if (pc.iceGatheringState === 'complete') { resolve(); return; }
          pc.addEventListener('icegatheringstatechange', () => {
            if (pc.iceGatheringState === 'complete') resolve();
          });
          setTimeout(resolve, 5000);
        });

        this.whepStatus = 'Sending offer to go2rtc…';
        const resp = await fetch('/api/whep', {
          method: 'POST',
          headers: { 'Content-Type': 'application/sdp' },
          body: pc.localDescription.sdp,
          signal: AbortSignal.timeout(20000),
        });

        if (!resp.ok) {
          const msg = await resp.text();
          throw new Error(`WHEP ${resp.status}: ${msg}`);
        }

        const answerSdp = await resp.text();
        this.whepStatus = 'Setting remote description…';
        await pc.setRemoteDescription({ type: 'answer', sdp: answerSdp });
        this.whepStatus = 'Waiting for video…';

      } catch (err) {
        console.error('[whep] connect failed:', err.message);
        this.whepStatus = 'Failed: ' + err.message;
        this.whepConnecting = true;
      }
    },

    async go2rtcCheckStatus() {
      try {
        const d = await this.api('GET', '/api/go2rtc/status');
        this.go2rtcRunning = d.running || false;
      } catch(e) { this.go2rtcRunning = false; }
    },

    startGo2rtcEvents() {
      if (this._go2rtcEs) return; // already open
      const es = new EventSource('/api/go2rtc/events');
      this._go2rtcEs = es;
      es.onmessage = (e) => {
        try {
          const ev = JSON.parse(e.data);
          this.go2rtcRunning = ev.ok;
          this.go2rtcEvents = [ev, ...this.go2rtcEvents].slice(0, 20);
        } catch {}
      };
      es.onerror = () => {
        // EventSource auto-reconnects; just mark unknown status after a pause
        setTimeout(() => { if (this._go2rtcEs === es && !es.OPEN) this.go2rtcRunning = false; }, 5000);
      };
    },

    stopGo2rtcEvents() {
      if (this._go2rtcEs) {
        this._go2rtcEs.close();
        this._go2rtcEs = null;
      }
    },

    async go2rtcInstall() {
      this.go2rtcInstalling = true;
      this.go2rtcInstallLog = '';
      try {
        const d = await this.api('POST', '/api/go2rtc/install');
        this.go2rtcInstallLog = d.output || d.error || '';
        if (d.success) {
          this.toast('go2rtc installed successfully');
          await this.go2rtcCheckStatus();
        } else {
          this.toast('Install failed — see log below', false);
        }
      } catch(e) {
        this.go2rtcInstallLog = 'Error: ' + e.message;
        this.toast('Install failed', false);
      } finally {
        this.go2rtcInstalling = false;
      }
    },

    startRecPreview() {
      clearTimeout(this._recPreviewTimer);
      this._previewActive = true;
      this._previewLastSuccess = 0;
      this.previewStale = false;
      this._previewLoop();
      this.startPreviewWatchdog();
    },

    stopRecPreview() {
      this._previewActive = false;
      clearTimeout(this._recPreviewTimer);
      clearInterval(this._previewWatchdog);
    },

    startPreviewWatchdog() {
      clearInterval(this._previewWatchdog);
      this._previewWatchdog = setInterval(() => {
        if (!this._previewActive) return;
        const age = this._previewLastSuccess > 0 ? Date.now() - this._previewLastSuccess : 0;
        this.previewStale = age > 12000;
      }, 2000);
    },

    togglePreviewFullscreen() {
      const el = this.$refs.previewBox;
      if (!el) return;
      if (document.fullscreenElement === el || document.webkitFullscreenElement === el) {
        (document.exitFullscreen || document.webkitExitFullscreen).call(document);
      } else {
        (el.requestFullscreen || el.webkitRequestFullscreen).call(el);
      }
    },

    // ── OCR region selection ─────────────────────────────────────────────────
    _ocrToFrac(ex, ey) {
      const box = this.$refs.previewBox.getBoundingClientRect();
      const vid = this.recPreviewMode === 'webrtc' ? this.$refs.previewVideo : null;
      const img = vid ? null : this.$refs.previewImg;
      const natW = vid ? vid.videoWidth  : img?.naturalWidth;
      const natH = vid ? vid.videoHeight : img?.naturalHeight;
      if (!natW || !natH) return null;
      const scale = Math.min(box.width / natW, box.height / natH);
      const rendW = natW * scale, rendH = natH * scale;
      const offX = (box.width - rendW) / 2, offY = (box.height - rendH) / 2;
      const px = ex - box.left, py = ey - box.top;
      return {
        x: Math.max(0, Math.min(1, (px - offX) / rendW)),
        y: Math.max(0, Math.min(1, (py - offY) / rendH)),
      };
    },

    ocrStartDrag(e) {
      if (e.button !== 0) return;
      e.preventDefault();
      const f = this._ocrToFrac(e.clientX, e.clientY);
      if (!f) return;
      this.ocrDragging = true;
      this._ocrStart = f;
      this.ocrRegion = null;
      this._ocrDrawCanvas(f, f);
    },

    ocrMoveDrag(e) {
      if (!this.ocrDragging || !this._ocrStart) return;
      const f = this._ocrToFrac(e.clientX, e.clientY);
      if (f) this._ocrDrawCanvas(this._ocrStart, f);
    },

    ocrEndDrag(e) {
      if (!this.ocrDragging) return;
      this.ocrDragging = false;
      const f = this._ocrToFrac(e.clientX, e.clientY);
      if (!f || !this._ocrStart) return;
      const s = this._ocrStart;
      const x = Math.min(s.x, f.x), y = Math.min(s.y, f.y);
      const w = Math.abs(f.x - s.x), h = Math.abs(f.y - s.y);
      if (w * h < 0.001) { this.ocrClear(); return; }
      this.ocrRegion = { x, y, w, h };
    },

    ocrClear() {
      this.ocrRegion = null;
      this.ocrDragging = false;
      this._ocrStart = null;
      const c = this.$refs.ocrCanvas;
      if (c) c.getContext('2d').clearRect(0, 0, c.width, c.height);
    },

    _ocrDrawCanvas(a, b) {
      const c = this.$refs.ocrCanvas;
      const box = this.$refs.previewBox.getBoundingClientRect();
      if (!c || !box) return;
      c.width = box.width; c.height = box.height;
      const img = this.$refs.previewImg;
      const natW = img?.naturalWidth || 1280, natH = img?.naturalHeight || 720;
      const scale = Math.min(box.width / natW, box.height / natH);
      const rendW = natW * scale, rendH = natH * scale;
      const offX = (box.width - rendW) / 2, offY = (box.height - rendH) / 2;
      const px = x => offX + x * rendW, py = y => offY + y * rendH;
      const x1 = px(Math.min(a.x,b.x)), y1 = py(Math.min(a.y,b.y));
      const x2 = px(Math.max(a.x,b.x)), y2 = py(Math.max(a.y,b.y));
      const ctx = c.getContext('2d');
      ctx.clearRect(0, 0, c.width, c.height);
      ctx.strokeStyle = '#3b82f6'; ctx.lineWidth = 2; ctx.setLineDash([6,3]);
      ctx.strokeRect(x1, y1, x2-x1, y2-y1);
      ctx.fillStyle = 'rgba(59,130,246,0.12)';
      ctx.fillRect(x1, y1, x2-x1, y2-y1);
    },

    async ocrCapture() {
      if (!this.ocrRegion || this.ocrLoading) return;
      this.ocrLoading = true;
      this.ocrText = null;
      try {
        const { x, y, w, h } = this.ocrRegion;
        const r = await fetch(`/api/ocr?x=${x}&y=${y}&w=${w}&h=${h}`);
        const d = await r.json();
        if (!d.success && d.error && d.error.includes('tesseract not available')) {
          this.ocrAvailable = false;
          this.ocrText = 'tesseract is not installed.\n\nRun: sudo apt-get install tesseract-ocr';
        } else {
          this.ocrText = d.success ? (d.text || '(no text found)') : ('Error: ' + d.error);
        }
      } catch(e) {
        this.ocrText = 'Error: ' + e.message;
      } finally {
        this.ocrLoading = false;
      }
    },

    async _previewLoop() {
      if (!this._previewActive || this.activeTab !== 'recording' || this.recPreviewMode !== 'screenshot') return;
      await this.refreshPreview();
      try {
        const statusRes = await this.api('GET', '/api/recording/status');
        if (statusRes.success) this.recStatus = statusRes.data;
      } catch(e) {}
      this._recPreviewTimer = setTimeout(() => this._previewLoop(), 3000);
    },

    async refreshPreview() {
      try {
        const r = await fetch('/api/screenshot', { signal: AbortSignal.timeout(8000) });
        if (!r.ok) return;
        const blob = await r.blob();
        if (blob.size < 1024) return;
        const newSrc = URL.createObjectURL(blob);
        await new Promise(resolve => {
          const tmp = new Image();
          tmp.onload = tmp.onerror = resolve;
          tmp.src = newSrc;
        });
        const oldSrc = this.recPreviewSrc;
        this.recPreviewSrc = newSrc;
        this.recPreviewTs = new Date().toLocaleTimeString('en-US');
        this._previewLastSuccess = Date.now();
        this.previewStale = false;
        if (oldSrc) setTimeout(() => URL.revokeObjectURL(oldSrc), 200);
      } catch(e) {
        // Keep last good frame visible on error
      }
    },

    async loadCaptureLog() {
      try {
        const d = await this.api('GET', '/api/capture-log');
        if (d.success) this.captureLogEntries = (d.data || []).reverse();
      } catch(e) {}
    },

    async saveRecCfg() {
      try {
        const r = await fetch('/api/recording/config', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(this.recCfg),
        });
        const d = await r.json();
        this.toast(d.message || d.error, d.success !== false);
        if (d.success) this.loadRecording();
      } catch(e) {
        this.toast('Save failed: ' + e.message, false);
      }
    },

    async loadFrames() {
      this.stopPlayback();
      this.playbackFrames = [];
      this.playbackIdx = 0;
      this.videoSegments = [];
      this.videoSegment = '';
      if (!this.playbackDate) return;
      try {
        const [framesRes, segsRes] = await Promise.all([
          this.api('GET', `/api/recording/frames?date=${this.playbackDate}`),
          this.api('GET', `/api/recording/segments?date=${this.playbackDate}`),
        ]);
        if (framesRes.success) this.playbackFrames = framesRes.data || [];
        if (segsRes.success) {
          this.videoSegments = segsRes.data || [];
          if (this.videoSegments.length) {
            // Select latest segment and load video
            this.selectVideoSegment(this.videoSegments[this.videoSegments.length - 1]);
          }
        }
      } catch(e) {
        this.toast('Failed to load frames', false);
      }
    },

    selectVideoSegment(seg) {
      this.videoSegment = seg;
      // <video> ignores src attribute changes — must set .src and call .load() explicitly.
      // Use setTimeout to allow x-show to render the element first.
      setTimeout(() => {
        const v = this.$refs.videoPlayer;
        if (v) {
          v.src = `/api/recording/videofile?date=${this.playbackDate}&segment=${seg}`;
          v.load();
          v.play().catch(() => {});
        }
      }, 50);
    },

    formatSegmentHour(seg) {
      if (!seg) return '';
      const h = parseInt(seg, 10);
      const ampm = h >= 12 ? 'PM' : 'AM';
      const h12 = h % 12 || 12;
      return `${h12}:00 ${ampm}`;
    },

    get currentFrameSrc() {
      if (!this.playbackDate || !this.playbackFrames.length) return '';
      return `/api/recording/frame?date=${this.playbackDate}&time=${this.playbackFrames[this.playbackIdx]}`;
    },

    updateFrame() {
      const img = this.$refs.playerImg;
      if (img) {
        const src = `/api/recording/frame?date=${this.playbackDate}&time=${this.playbackFrames[this.playbackIdx]}`;
        if (img.src !== src) img.src = src;
      }
    },

    toggleFullscreen() {
      const el = this.$refs.playerBox;
      if (!el) return;
      if (document.fullscreenElement || document.webkitFullscreenElement) {
        (document.exitFullscreen || document.webkitExitFullscreen).call(document);
      } else {
        (el.requestFullscreen || el.webkitRequestFullscreen).call(el);
      }
    },

    toggleVideoFullscreen() {
      const el = this.$refs.videoBox;
      if (!el) return;
      if (document.fullscreenElement || document.webkitFullscreenElement) {
        (document.exitFullscreen || document.webkitExitFullscreen).call(document);
      } else {
        (el.requestFullscreen || el.webkitRequestFullscreen).call(el);
      }
    },

    playbackStep(dir) {
      const next = this.playbackIdx + dir;
      if (next >= 0 && next < this.playbackFrames.length) {
        this.playbackIdx = next;
        this.updateFrame();
      }
    },

    togglePlayback() {
      if (this.playbackPlaying) {
        this.stopPlayback();
      } else {
        this.playbackPlaying = true;
        const tick = () => {
          if (!this.playbackPlaying) return;
          if (this.playbackIdx >= this.playbackFrames.length - 1) {
            this.stopPlayback();
            return;
          }
          this.playbackIdx++;
          this.updateFrame();
          const interval = this.recCfg.interval_seconds || 5;
          const ms = Math.max((interval * 1000) / this.playbackSpeed, 50);
          this._playTimer = setTimeout(tick, ms);
        };
        tick();
      }
    },

    stopPlayback() {
      this.playbackPlaying = false;
      clearTimeout(this._playTimer);
    },

    // ── Settings ───────────────────────────────────────────────────────────────

    async loadSettings() {
      try {
        const d = await this.api('GET', '/api/settings');
        if (d.success) {
          this.settings = {
            host:       d.host         || '',
            user:       d.user         || '',
            sshUser:    d.ssh_user     || 'root',
            sshPassSet: !!d.ssh_pass_set,
            sshKeyPath: d.ssh_key_path || '',
            pass:       '',
            sshPass:    '',
          };
          this.settingsLoaded = true;
        }
      } catch(e) {
        this.toast('Failed to load settings', false);
      }
      fetch('/api/bitrate').then(r=>r.json()).then(d=>{ if (d.bitrate) this.bitrateValue = d.bitrate; }).catch(()=>{});
    },

    async setBitrate() {
      this.bitrateLoading = true;
      this.bitrateMsg = '';
      this.bitrateErr = '';
      try {
        const d = await this.api('POST', '/api/bitrate', { bitrate: this.bitrateValue });
        if (d.success) {
          this.bitrateMsg = d.message || 'Applied';
          setTimeout(() => { this.bitrateMsg = ''; }, 5000);
        } else {
          this.bitrateErr = d.error || 'Failed';
        }
      } catch(e) {
        this.bitrateErr = e.message;
      } finally {
        this.bitrateLoading = false;
      }
    },

    // ── EDID / 4K → 1080p ────────────────────────────────────────────────────

    async edidCheckStatus() {
      this.edidLoading = true;
      this.edidLog = '';
      try {
        const d = await this.api('GET', '/api/edid/status');
        if (d.success) {
          this.edidInputRes = d.input_res || '';
          this.edidLog = d.output || '';
        } else {
          this.edidLog = 'Error: ' + (d.error || 'unknown');
        }
      } catch(e) {
        this.edidLog = 'Error: ' + e.message;
      } finally {
        this.edidLoading = false;
      }
    },

    async edidSet() {
      this.edidLoading = true;
      this.edidLog = 'Applying 1080p EDID via SSH…';
      try {
        const d = await this.api('POST', '/api/edid/set');
        this.edidLog = d.output || (d.error || 'Done');
        if (d.success) this.toast('1080p EDID applied — replug HDMI cable if resolution does not change');
        else this.toast(d.error || 'EDID set failed', false);
      } catch(e) {
        this.edidLog = 'Error: ' + e.message;
        this.toast('EDID set failed: ' + e.message, false);
      } finally {
        this.edidLoading = false;
      }
    },

    async edidRestore() {
      this.edidLoading = true;
      this.edidLog = 'Restoring default EDID…';
      try {
        const d = await this.api('POST', '/api/edid/restore');
        this.edidLog = d.output || (d.error || 'Done');
        if (d.success) this.toast('EDID restored to device default');
        else this.toast(d.error || 'Restore failed', false);
      } catch(e) {
        this.edidLog = 'Error: ' + e.message;
        this.toast('EDID restore failed: ' + e.message, false);
      } finally {
        this.edidLoading = false;
        this.edidInputRes = '';
      }
    },

    // ── FPS measurement ───────────────────────────────────────────────────────

    // Canvas-based frame-change detector for MJPEG <img> elements.
    // Samples a small 64×36 tile each RAF; counts pixel-change events per second.
    _startFpsSampler(imgEl, setter) {
      const cv = document.createElement('canvas');
      cv.width = 64; cv.height = 36;
      const ctx = cv.getContext('2d', { willReadFrequently: true });
      let frames = 0, last = performance.now(), prev = null;
      let raf;
      const tick = () => {
        try {
          ctx.drawImage(imgEl, 0, 0, 64, 36);
          const d = ctx.getImageData(0, 0, 64, 36).data;
          if (prev) {
            let changed = false;
            for (let i = 0; i < d.length && !changed; i += 128) {
              if (d[i] !== prev[i] || d[i+1] !== prev[i+1]) changed = true;
            }
            if (changed) frames++;
          }
          prev = d.slice();
        } catch(e) {}
        const now = performance.now();
        if (now - last >= 1000) {
          setter(Math.round(frames * 1000 / (now - last)));
          frames = 0; last = now;
        }
        raf = requestAnimationFrame(tick);
      };
      raf = requestAnimationFrame(tick);
      return () => cancelAnimationFrame(raf);
    },

    // WebRTC stats-based FPS reader via RTCPeerConnection.getStats().
    _startFpsStats(setter) {
      const id = setInterval(async () => {
        if (!this.whepPc) return;
        try {
          const stats = await this.whepPc.getStats();
          stats.forEach(r => {
            if (r.type === 'inbound-rtp' && r.kind === 'video' && r.framesPerSecond != null)
              setter(Math.round(r.framesPerSecond));
          });
        } catch(e) {}
      }, 1000);
      return () => clearInterval(id);
    },

    _startDashFps() {
      if (this._stopDashFps) { this._stopDashFps(); this._stopDashFps = null; }
      this.streamFps = null;
      if (this.streamMode === 'screenshot') return;
      this.$nextTick(() => {
        const img = this.$refs.dashStreamImg;
        if (img) this._stopDashFps = this._startFpsSampler(img, fps => { this.streamFps = fps; });
      });
    },

    _startRecFps() {
      if (this._stopRecFps) { this._stopRecFps(); this._stopRecFps = null; }
      this.recFps = null;
      if (this.recPreviewMode === 'screenshot') return;
      if (this.recPreviewMode === 'webrtc') {
        this._stopRecFps = this._startFpsStats(fps => { this.recFps = fps; });
        return;
      }
      this.$nextTick(() => {
        const img = this.$refs.previewImg;
        if (img) this._stopRecFps = this._startFpsSampler(img, fps => { this.recFps = fps; });
      });
    },

    async saveSettings() {
      this.settingsSaving = true;
      try {
        const body = {
          host:         this.settings.host,
          user:         this.settings.user,
          ssh_user:     this.settings.sshUser,
          ssh_key_path: this.settings.sshKeyPath,
        };
        if (this.settings.pass)    body.pass     = this.settings.pass;
        if (this.settings.sshPass) body.ssh_pass  = this.settings.sshPass;
        const d = await this.api('POST', '/api/settings', body);
        this.toast(d.message || d.error, d.success !== false);
      } catch(e) {
        this.toast('Save failed: ' + e.message, false);
      } finally {
        this.settingsSaving = false;
      }
    },
  };
}
