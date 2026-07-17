import './style.css';

// ── Types ─────────────────────────────────────

type ConnectionState =
  | 'disconnected'
  | 'connecting'
  | 'connected'
  | 'streaming'
  | 'error';

// ── SVG Icons ─────────────────────────────────

const CAMERA_ICON = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" class="video-wrap__overlay-icon"><path d="M23 7l-7 5 7 5V7z"/><rect x="1" y="5" width="15" height="14" rx="2" ry="2"/></svg>`;

const OFFLINE_ICON = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" class="video-wrap__overlay-icon"><line x1="1" y1="1" x2="23" y2="23"/><path d="M21 21H3a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h3m3-3h6l2 3h4a2 2 0 0 1 2 2v9.34"/><circle cx="12" cy="13" r="3" opacity="0.4"/></svg>`;

// ── Helpers ───────────────────────────────────

/**
 * Returns the default WebSocket URL to pre-fill in the UI.
 * - On localhost (Vite dev server): use the Vite proxy path.
 * - On any other host (e.g. Cloud Run): derive wss://<host>/ws from location.
 */
function getDefaultWsUrl(): string {
  const { hostname, protocol, host } = window.location;
  if (hostname === 'localhost' || hostname === '127.0.0.1') {
    return 'ws://localhost:3000/cloudproxy-ws';
  }
  const wsProtocol = protocol === 'https:' ? 'wss:' : 'ws:';
  return `${wsProtocol}//${host}/ws`;
}

// ── DOM Setup ─────────────────────────────────


function buildUI(): {
  canvas: HTMLCanvasElement;
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
        <canvas class="video-wrap__video" id="videoCanvas"></canvas>
        <div class="video-wrap__overlay" id="overlay">
          <div id="overlayIcon">${CAMERA_ICON}</div>
          <div class="video-wrap__overlay-text video-wrap__overlay-text--pulse" id="overlayText">Waiting for camera…</div>
        </div>
        <div class="stats" id="stats">
          <div class="stats__row"><span class="stats__label">FPS</span><span class="stats__value" id="statFps">—</span></div>
          <div class="stats__row"><span class="stats__label">Res</span><span class="stats__value" id="statRes">—</span></div>
          <div class="stats__row"><span class="stats__label">Bitrate</span><span class="stats__value" id="statBitrate">—</span></div>
          <div class="stats__row"><span class="stats__label">Frames</span><span class="stats__value" id="statFrames">—</span></div>
        </div>
      </div>
    </div>

    <div class="panel">
      <div class="panel__row">
        <div class="panel__field">
          <label class="panel__label" for="serverUrl">Server URL</label>
          <input class="panel__input" id="serverUrl" type="text" placeholder="ws://localhost:3000/cloudproxy-ws" value="${getDefaultWsUrl()}" />
        </div>
        <div class="panel__field" style="max-width:260px">
          <label class="panel__label" for="token">Token</label>
          <input class="panel__input" id="token" type="password" placeholder="Auth token" value="${import.meta.env.VITE_AUTH_TOKEN ?? ''}" />
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
    canvas: document.getElementById('videoCanvas') as HTMLCanvasElement,
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
  // Check for WebCodecs support
  if (!('VideoDecoder' in window)) {
    const app = document.getElementById('app')!;
    app.innerHTML = `
      <div style="display:flex;align-items:center;justify-content:center;height:100vh;padding:2rem;text-align:center;">
        <div>
          <h1 style="color:#e74c3c;margin-bottom:1rem;">WebCodecs Not Supported</h1>
          <p style="color:#aaa;max-width:480px;">
            Your browser does not support the WebCodecs API, which is required for video decoding.
            Please use a recent version of Chrome, Edge, or Opera.
          </p>
        </div>
      </div>
    `;
    return;
  }

  const ui = buildUI();
  let ws: WebSocket | null = null;
  let decoder: VideoDecoder | null = null;
  let statsInterval: ReturnType<typeof setInterval> | null = null;
  let state: ConnectionState = 'disconnected';

  // Stats tracking
  let frameCount = 0;
  let totalFrames = 0;
  let bytesReceived = 0;
  let lastStatsTime = performance.now();
  let lastWidth = 0;
  let lastHeight = 0;
  let currentResolution = '';
  let firstFrameReceived = false;

  // Canvas context
  const ctx = ui.canvas.getContext('2d')!;

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
    frameCount = 0;
    totalFrames = 0;
    bytesReceived = 0;
    lastStatsTime = performance.now();
    currentResolution = '';

    statsInterval = setInterval(() => {
      const now = performance.now();
      const elapsed = (now - lastStatsTime) / 1000;

      const fps = elapsed > 0 ? Math.round(frameCount / elapsed) : 0;
      const kbps = elapsed > 0 ? Math.round((bytesReceived * 8) / 1000 / elapsed) : 0;

      // Update DOM
      const el = (id: string) => document.getElementById(id);
      el('statFps')!.textContent = String(fps);
      if (currentResolution) el('statRes')!.textContent = currentResolution;
      el('statBitrate')!.textContent = `${kbps} kbps`;
      el('statFrames')!.textContent = String(totalFrames);

      // Reset per-interval counters
      frameCount = 0;
      bytesReceived = 0;
      lastStatsTime = now;
    }, 1000);
  }

  function stopStatsCollection() {
    if (statsInterval !== null) {
      clearInterval(statsInterval);
      statsInterval = null;
    }
    // Reset stat values
    const ids = ['statFps', 'statRes', 'statBitrate', 'statFrames'];
    ids.forEach((id) => {
      const el = document.getElementById(id);
      if (el) el.textContent = '—';
    });
  }

  // -- Create VideoDecoder
  function createDecoder(): VideoDecoder {
    const d = new VideoDecoder({
      output: (frame: VideoFrame) => {
        // Update canvas dimensions if resolution changed
        if (frame.displayWidth !== lastWidth || frame.displayHeight !== lastHeight) {
          lastWidth = frame.displayWidth;
          lastHeight = frame.displayHeight;
          ui.canvas.width = frame.displayWidth;
          ui.canvas.height = frame.displayHeight;
          currentResolution = `${frame.displayWidth}×${frame.displayHeight}`;
        }

        // Draw frame to canvas
        ctx.drawImage(frame, 0, 0, ui.canvas.width, ui.canvas.height);
        frame.close();

        // Update stats
        frameCount++;
        totalFrames++;

        // Transition to streaming on first frame
        if (!firstFrameReceived) {
          firstFrameReceived = true;
          setState('streaming');
          startStatsCollection();
        }
      },
      error: (e: Error) => {
        console.error('[decoder] error:', e);
      },
    });

    // Configure decoder for H264 baseline profile, level 3.1
    d.configure({
      codec: 'avc1.42C01F',
      optimizeForLatency: true,
    });

    return d;
  }

  // -- Cleanup
  function cleanup() {
    stopStatsCollection();

    if (decoder) {
      try {
        decoder.close();
      } catch {
        // decoder may already be closed
      }
      decoder = null;
    }

    if (ws) {
      ws.onopen = null;
      ws.onmessage = null;
      ws.onerror = null;
      ws.onclose = null;
      ws.close();
      ws = null;
    }

    // Reset canvas
    ctx.clearRect(0, 0, ui.canvas.width, ui.canvas.height);

    // Reset stats tracking
    frameCount = 0;
    totalFrames = 0;
    bytesReceived = 0;
    lastWidth = 0;
    lastHeight = 0;
    currentResolution = '';
    firstFrameReceived = false;
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

    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
      console.log('[ws] connected');
    };

    ws.onmessage = (event: MessageEvent) => {
      if (event.data instanceof ArrayBuffer) {
        handleBinaryMessage(event.data);
      } else {
        handleTextMessage(event.data as string);
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

  // -- Handle binary WebSocket message (H264 video data)
  function handleBinaryMessage(data: ArrayBuffer) {
    if (data.byteLength < 5) {
      console.warn('[ws] binary message too short:', data.byteLength);
      return;
    }

    // Ensure decoder exists
    if (!decoder || decoder.state === 'closed') {
      decoder = createDecoder();
    }

    // Skip frames if decoder queue is backing up
    if (decoder.decodeQueueSize > 5) {
      console.warn('[decoder] queue overflow, skipping frame (queue size:', decoder.decodeQueueSize, ')');
      return;
    }

    const view = new DataView(data);
    const flags = view.getUint8(0);
    const timestamp = view.getUint32(1, false); // big-endian
    const isKeyframe = (flags & 0x01) !== 0;
    const h264Data = new Uint8Array(data, 5); // skip 5-byte header

    // Track bytes for bitrate calculation
    bytesReceived += data.byteLength;

    const chunk = new EncodedVideoChunk({
      type: isKeyframe ? 'key' : 'delta',
      timestamp: timestamp * 1000, // convert ms to microseconds
      data: h264Data,
    });

    try {
      decoder.decode(chunk);
    } catch (e) {
      console.error('[decoder] decode error:', e);
    }
  }

  // -- Handle text WebSocket message (JSON control)
  function handleTextMessage(data: string) {
    let msg: { type: string; id?: string; message?: string };
    try {
      msg = JSON.parse(data);
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
        // Create decoder in preparation for video data
        if (!decoder || decoder.state === 'closed') {
          decoder = createDecoder();
        }
        firstFrameReceived = false;
        setState('connected', 'Publisher online — waiting for video');
        break;

      case 'publisher_offline':
        console.log('[ws] publisher went offline');
        // Close decoder but keep WebSocket
        if (decoder) {
          try {
            decoder.close();
          } catch {
            // decoder may already be closed
          }
          decoder = null;
        }
        stopStatsCollection();
        ctx.clearRect(0, 0, ui.canvas.width, ui.canvas.height);
        firstFrameReceived = false;
        setState('connected', 'Camera offline');
        // Show offline overlay
        ui.overlay.classList.remove('video-wrap__overlay--hidden');
        ui.overlay.classList.add('video-wrap__overlay--offline');
        ui.overlayIcon.innerHTML = OFFLINE_ICON;
        ui.overlayText.textContent = 'Camera Offline';
        ui.overlayText.className = 'video-wrap__overlay-text';
        break;

      case 'error':
        console.error('[ws] server error:', msg.message);
        setState('error', msg.message || 'Server error');
        break;
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
