import './style.css';

// ── Types ─────────────────────────────────────

type ConnectionState =
  | 'disconnected'
  | 'connecting'
  | 'connected'
  | 'streaming'
  | 'error';

interface Stats {
  fps: number;
  resolution: string;
  bitrate: string;
  rtt: string;
}

// ── SVG Icons ─────────────────────────────────

const CAMERA_ICON = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" class="video-wrap__overlay-icon"><path d="M23 7l-7 5 7 5V7z"/><rect x="1" y="5" width="15" height="14" rx="2" ry="2"/></svg>`;

const OFFLINE_ICON = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" class="video-wrap__overlay-icon"><line x1="1" y1="1" x2="23" y2="23"/><path d="M21 21H3a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h3m3-3h6l2 3h4a2 2 0 0 1 2 2v9.34"/><circle cx="12" cy="13" r="3" opacity="0.4"/></svg>`;

// ── DOM Setup ─────────────────────────────────

function buildUI(): {
  video: HTMLVideoElement;
  overlay: HTMLDivElement;
  overlayIcon: HTMLDivElement;
  overlayText: HTMLDivElement;
  statsEl: HTMLDivElement;
  serverInput: HTMLInputElement;
  tokenInput: HTMLInputElement;
  connectBtn: HTMLButtonElement;
  statusDot: HTMLDivElement;
  statusText: HTMLSpanElement;
} {
  const app = document.getElementById('app')!;
  app.innerHTML = `
    <header class="header">
      <h1 class="header__title">CloudProxy</h1>
      <p class="header__subtitle">Live Camera Feed</p>
    </header>

    <div class="video-wrap" id="videoWrap">
      <div class="video-wrap__aspect">
        <video class="video-wrap__video" id="video"></video>
        <div class="video-wrap__overlay" id="overlay">
          <div id="overlayIcon">${CAMERA_ICON}</div>
          <div class="video-wrap__overlay-text video-wrap__overlay-text--pulse" id="overlayText">Waiting for camera…</div>
        </div>
        <div class="stats" id="stats">
          <div class="stats__row"><span class="stats__label">FPS</span><span class="stats__value" id="statFps">—</span></div>
          <div class="stats__row"><span class="stats__label">Res</span><span class="stats__value" id="statRes">—</span></div>
          <div class="stats__row"><span class="stats__label">Bitrate</span><span class="stats__value" id="statBitrate">—</span></div>
          <div class="stats__row"><span class="stats__label">RTT</span><span class="stats__value" id="statRtt">—</span></div>
        </div>
      </div>
    </div>

    <div class="panel">
      <div class="panel__row">
        <div class="panel__field">
          <label class="panel__label" for="serverUrl">Server URL</label>
          <input class="panel__input" id="serverUrl" type="text" placeholder="ws://localhost:8080/ws" value="ws://localhost:8080/ws" />
        </div>
        <div class="panel__field" style="max-width:260px">
          <label class="panel__label" for="token">Token</label>
          <input class="panel__input" id="token" type="password" placeholder="Auth token" value="cloudproxy-dev-token" />
        </div>
        <button class="btn btn--connect" id="connectBtn">Connect</button>
      </div>
      <div class="status">
        <div class="status__dot status__dot--disconnected" id="statusDot"></div>
        <span class="status__text" id="statusText">Disconnected</span>
      </div>
    </div>
  `;

  return {
    video: document.getElementById('video') as HTMLVideoElement,
    overlay: document.getElementById('overlay') as HTMLDivElement,
    overlayIcon: document.getElementById('overlayIcon') as HTMLDivElement,
    overlayText: document.getElementById('overlayText') as HTMLDivElement,
    statsEl: document.getElementById('stats') as HTMLDivElement,
    serverInput: document.getElementById('serverUrl') as HTMLInputElement,
    tokenInput: document.getElementById('token') as HTMLInputElement,
    connectBtn: document.getElementById('connectBtn') as HTMLButtonElement,
    statusDot: document.getElementById('statusDot') as HTMLDivElement,
    statusText: document.getElementById('statusText') as HTMLSpanElement,
  };
}

// ── Main Application ──────────────────────────

function main() {
  const ui = buildUI();
  let ws: WebSocket | null = null;
  let pc: RTCPeerConnection | null = null;
  let statsInterval: ReturnType<typeof setInterval> | null = null;
  let prevBytesReceived = 0;
  let prevTimestamp = 0;
  let state: ConnectionState = 'disconnected';

  // -- Video setup
  ui.video.autoplay = true;
  ui.video.muted = true;
  ui.video.playsInline = true;

  // -- State management
  function setState(newState: ConnectionState, message?: string) {
    state = newState;
    const videoWrap = document.getElementById('videoWrap')!;

    // Status dot
    ui.statusDot.className = `status__dot status__dot--${newState}`;

    // Status text
    const labels: Record<ConnectionState, string> = {
      disconnected: 'Disconnected',
      connecting: 'Connecting…',
      connected: 'Connected — waiting for camera',
      streaming: 'Live',
      error: message || 'Connection error',
    };
    ui.statusText.textContent = message || labels[newState];

    // Button
    if (newState === 'disconnected' || newState === 'error') {
      ui.connectBtn.textContent = 'Connect';
      ui.connectBtn.className = 'btn btn--connect';
      ui.connectBtn.disabled = false;
      ui.serverInput.disabled = false;
      ui.tokenInput.disabled = false;
    } else if (newState === 'connecting') {
      ui.connectBtn.textContent = 'Connecting…';
      ui.connectBtn.className = 'btn btn--connect';
      ui.connectBtn.disabled = true;
      ui.serverInput.disabled = true;
      ui.tokenInput.disabled = true;
    } else {
      ui.connectBtn.textContent = 'Disconnect';
      ui.connectBtn.className = 'btn btn--disconnect';
      ui.connectBtn.disabled = false;
      ui.serverInput.disabled = true;
      ui.tokenInput.disabled = true;
    }

    // Video wrap glow
    videoWrap.classList.toggle('video-wrap--live', newState === 'streaming');

    // Overlay
    if (newState === 'streaming') {
      ui.overlay.classList.add('video-wrap__overlay--hidden');
      ui.overlay.classList.remove('video-wrap__overlay--offline');
    } else {
      ui.overlay.classList.remove('video-wrap__overlay--hidden');
      if (newState === 'disconnected' || newState === 'error') {
        ui.overlayIcon.innerHTML = OFFLINE_ICON;
        ui.overlayText.textContent = newState === 'error' ? (message || 'Error') : 'Camera Offline';
        ui.overlayText.className = 'video-wrap__overlay-text';
        ui.overlay.classList.add('video-wrap__overlay--offline');
      } else if (newState === 'connected') {
        ui.overlayIcon.innerHTML = CAMERA_ICON;
        ui.overlayText.textContent = 'Waiting for camera…';
        ui.overlayText.className = 'video-wrap__overlay-text video-wrap__overlay-text--pulse';
        ui.overlay.classList.remove('video-wrap__overlay--offline');
      } else {
        ui.overlayIcon.innerHTML = CAMERA_ICON;
        ui.overlayText.textContent = 'Connecting…';
        ui.overlayText.className = 'video-wrap__overlay-text video-wrap__overlay-text--pulse';
        ui.overlay.classList.remove('video-wrap__overlay--offline');
      }
    }

    // Stats overlay
    ui.statsEl.classList.toggle('stats--visible', newState === 'streaming');
  }

  // -- Stats collection
  function startStatsCollection() {
    stopStatsCollection();
    prevBytesReceived = 0;
    prevTimestamp = 0;

    statsInterval = setInterval(async () => {
      if (!pc) return;

      try {
        const report = await pc.getStats();
        const stats: Partial<Stats> = {};

        report.forEach((s) => {
          if (s.type === 'inbound-rtp' && s.kind === 'video') {
            // FPS
            if (s.framesPerSecond !== undefined) {
              stats.fps = s.framesPerSecond;
            }
            // Resolution
            if (s.frameWidth && s.frameHeight) {
              stats.resolution = `${s.frameWidth}×${s.frameHeight}`;
            }
            // Bitrate
            const now = s.timestamp;
            const bytes = s.bytesReceived || 0;
            if (prevTimestamp > 0 && now > prevTimestamp) {
              const deltaSec = (now - prevTimestamp) / 1000;
              const deltaBytes = bytes - prevBytesReceived;
              const kbps = ((deltaBytes * 8) / 1000 / deltaSec).toFixed(0);
              stats.bitrate = `${kbps} kbps`;
            }
            prevBytesReceived = bytes;
            prevTimestamp = now;
          }

          if (s.type === 'candidate-pair' && s.state === 'succeeded') {
            if (s.currentRoundTripTime !== undefined) {
              stats.rtt = `${(s.currentRoundTripTime * 1000).toFixed(0)} ms`;
            }
          }
        });

        // Update DOM
        const el = (id: string) => document.getElementById(id);
        if (stats.fps !== undefined) el('statFps')!.textContent = String(stats.fps);
        if (stats.resolution) el('statRes')!.textContent = stats.resolution;
        if (stats.bitrate) el('statBitrate')!.textContent = stats.bitrate;
        if (stats.rtt) el('statRtt')!.textContent = stats.rtt;
      } catch {
        // stats collection failed silently
      }
    }, 1000);
  }

  function stopStatsCollection() {
    if (statsInterval !== null) {
      clearInterval(statsInterval);
      statsInterval = null;
    }
    // Reset stat values
    const ids = ['statFps', 'statRes', 'statBitrate', 'statRtt'];
    ids.forEach((id) => {
      const el = document.getElementById(id);
      if (el) el.textContent = '—';
    });
  }

  // -- Cleanup
  function cleanup() {
    stopStatsCollection();

    if (pc) {
      pc.ontrack = null;
      pc.onicecandidate = null;
      pc.oniceconnectionstatechange = null;
      pc.close();
      pc = null;
    }

    if (ws) {
      ws.onopen = null;
      ws.onmessage = null;
      ws.onerror = null;
      ws.onclose = null;
      ws.close();
      ws = null;
    }

    ui.video.srcObject = null;
  }

  // -- Connect
  function connect() {
    cleanup();
    setState('connecting');

    const serverUrl = ui.serverInput.value.trim();
    const token = ui.tokenInput.value.trim();

    if (!serverUrl) {
      setState('error', 'Server URL is required');
      return;
    }

    const sep = serverUrl.includes('?') ? '&' : '?';
    const wsUrl = `${serverUrl}${sep}role=viewer&token=${encodeURIComponent(token)}`;

    try {
      ws = new WebSocket(wsUrl);
    } catch (err) {
      setState('error', `Invalid URL: ${err}`);
      return;
    }

    ws.onopen = () => {
      console.log('[ws] connected');
    };

    ws.onmessage = async (event) => {
      let msg: { type: string; id?: string; sdp?: string; candidate?: string; message?: string };
      try {
        msg = JSON.parse(event.data as string);
      } catch {
        console.warn('[ws] failed to parse message');
        return;
      }

      console.log('[ws] ←', msg.type);

      switch (msg.type) {
        case 'welcome':
          console.log('[ws] session id:', msg.id);
          setState('connected');
          break;

        case 'publisher_online':
          console.log('[ws] publisher is online');
          setState('connected', 'Publisher online — waiting for offer');
          break;

        case 'publisher_offline':
          console.log('[ws] publisher went offline');
          // Close peer connection but keep WebSocket
          if (pc) {
            pc.close();
            pc = null;
          }
          stopStatsCollection();
          ui.video.srcObject = null;
          setState('connected', 'Camera offline');
          // Show offline overlay
          ui.overlay.classList.remove('video-wrap__overlay--hidden');
          ui.overlay.classList.add('video-wrap__overlay--offline');
          ui.overlayIcon.innerHTML = OFFLINE_ICON;
          ui.overlayText.textContent = 'Camera Offline';
          ui.overlayText.className = 'video-wrap__overlay-text';
          break;

        case 'offer':
          await handleOffer(msg.sdp!);
          break;

        case 'candidate':
          await handleRemoteCandidate(msg.candidate!);
          break;

        case 'error':
          console.error('[ws] server error:', msg.message);
          setState('error', msg.message || 'Server error');
          break;
      }
    };

    ws.onerror = () => {
      console.error('[ws] error');
    };

    ws.onclose = (event) => {
      console.log('[ws] closed', event.code, event.reason);
      if (state !== 'disconnected') {
        cleanup();
        setState('disconnected');
      }
    };
  }

  // -- Handle SDP Offer
  async function handleOffer(sdp: string) {
    console.log('[rtc] received offer');

    pc = new RTCPeerConnection({
      iceServers: [{ urls: 'stun:stun.l.google.com:19302' }],
    });

    // ICE candidates → send to server
    pc.onicecandidate = (event) => {
      if (event.candidate && ws && ws.readyState === WebSocket.OPEN) {
        ws.send(
          JSON.stringify({
            type: 'candidate',
            candidate: JSON.stringify(event.candidate.toJSON()),
          })
        );
      }
    };

    // Track received → attach to video
    pc.ontrack = (event) => {
      console.log('[rtc] track received:', event.track.kind);
      if (event.streams && event.streams[0]) {
        ui.video.srcObject = event.streams[0];
      } else {
        const stream = new MediaStream();
        stream.addTrack(event.track);
        ui.video.srcObject = stream;
      }
      setState('streaming');
      startStatsCollection();
    };

    // ICE connection state
    pc.oniceconnectionstatechange = () => {
      console.log('[rtc] ICE state:', pc?.iceConnectionState);
      if (
        pc?.iceConnectionState === 'failed' ||
        pc?.iceConnectionState === 'disconnected'
      ) {
        console.warn('[rtc] ICE failed or disconnected');
      }
    };

    // Set remote offer
    await pc.setRemoteDescription(
      new RTCSessionDescription({ type: 'offer', sdp })
    );

    // Create and send answer
    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);

    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(
        JSON.stringify({
          type: 'answer',
          sdp: answer.sdp,
        })
      );
      console.log('[rtc] answer sent');
    }
  }

  // -- Handle remote ICE candidate
  async function handleRemoteCandidate(candidateStr: string) {
    if (!pc) {
      console.warn('[rtc] no peer connection for candidate');
      return;
    }
    try {
      const candidate = JSON.parse(candidateStr);
      await pc.addIceCandidate(new RTCIceCandidate(candidate));
      console.log('[rtc] added remote candidate');
    } catch (err) {
      console.warn('[rtc] failed to add candidate:', err);
    }
  }

  // -- Disconnect
  function disconnect() {
    cleanup();
    setState('disconnected');
  }

  // -- Button handler
  ui.connectBtn.addEventListener('click', () => {
    if (state === 'disconnected' || state === 'error') {
      connect();
    } else {
      disconnect();
    }
  });

  // -- Allow Enter key in inputs to connect
  const handleEnter = (e: KeyboardEvent) => {
    if (e.key === 'Enter' && (state === 'disconnected' || state === 'error')) {
      connect();
    }
  };
  ui.serverInput.addEventListener('keydown', handleEnter);
  ui.tokenInput.addEventListener('keydown', handleEnter);
}

// ── Boot ──────────────────────────────────────

main();
