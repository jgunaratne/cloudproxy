# CloudProxy — Complete Implementation Reference

> **Purpose**: Stream live video from a Raspberry Pi 5 webcam through a Google Cloud server to a MacBook browser, with lowest possible latency.
>
> **This document is the single source of truth** for the project. It contains everything needed for any developer or coding agent to understand, build, deploy, and debug the system.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Network Topology & Constraints](#2-network-topology--constraints)
3. [Protocol Specification](#3-protocol-specification)
4. [Component 1: Server (Go WebSocket Relay)](#4-component-1-server-go-websocket-relay)
5. [Component 2: Pi Client (Go + ffmpeg)](#5-component-2-pi-client-go--ffmpeg)
6. [Component 3: Viewer (Vite + TypeScript + WebCodecs)](#6-component-3-viewer-vite--typescript--webcodecs)
7. [Build & Cross-Compilation](#7-build--cross-compilation)
8. [Deployment Guide](#8-deployment-guide)
9. [Testing Checklist](#9-testing-checklist)
10. [Troubleshooting](#10-troubleshooting)
11. [Future Enhancements](#11-future-enhancements)
12. [Implementation Status](#12-implementation-status)

---

## 1. Architecture Overview

```
Guest WiFi Network              Google Cloud (Cloud Run)          Corporate Network
┌──────────────────┐            ┌──────────────────────┐         ┌──────────────────┐
│  Raspberry Pi 5  │            │  cloudproxy-server   │         │  MacBook         │
│                  │            │                      │         │                  │
│  USB Webcam      │            │  ┌────────────────┐  │         │  ┌────────────┐  │
│  (/dev/video0)   │            │  │  Go binary     │  │         │  │ Vite App   │  │
│        │         │            │  │  (WebSocket    │  │         │  │ (Browser)  │  │
│        ▼         │            │  │   relay)       │  │         │  │            │  │
│  ┌──────────┐    │  WebSocket │  │                │  │WebSocket│  │ <canvas>   │  │
│  │ ffmpeg   │    │  binary    │  │  Receives H264 │  │ binary  │  │ WebCodecs  │  │
│  │ H264 enc │    │  (TCP)     │  │  binary from   │  │ (TCP)   │  │ H264 decode│  │
│  └──────────┘    │            │  │  Pi, broadcasts│  │         │  └────────────┘  │
│        │         │            │  │  to all viewers│  │         │        │         │
│        ▼         │            │  │                │  │         │        ▼         │
│  ┌──────────┐    │            │  │  Caches SPS/PPS│  │         │  ┌────────────┐  │
│  │ H264     │────┼───────────┼──│  + last keyframe│──┼─────────┼──│ VideoFrame │  │
│  │ Annex B  │    │            │  │  for new viewer│  │         │  │ → canvas   │  │
│  │ → WS     │    │            │  │  bootstrap     │  │         │  │ rendering  │  │
│  └──────────┘    │            │  └────────────────┘  │         │  └────────────┘  │
└──────────────────┘            │                      │         └──────────────────┘
                                │  Port 8080 (HTTP)    │
                                │  (Cloud Run auto-TLS │
                                │   on 443 in prod)    │
                                └──────────────────────┘
```

### Data Flow Summary

| Path | Transport | Protocol | Content | Latency |
|------|-----------|----------|---------|---------|
| Pi → Server | TCP/WSS | WebSocket (binary) | H264 Annex B access units | N/A |
| Server → Browser | TCP/WSS | WebSocket (binary) | H264 Annex B access units (forwarded) | N/A |
| **Total end-to-end** | | | | **~100-300ms** |

### Why This Architecture

- **WebSocket-only (TCP)**: Cloud Run only supports TCP. No UDP means no WebRTC. WebSocket binary messages carry H264 Annex B access units directly.
- **Relay pattern**: Server receives binary frames from the publisher and broadcasts unchanged to all connected viewers. No transcoding, minimal CPU.
- **WebCodecs on the viewer**: The browser's `VideoDecoder` API decodes H264 Annex B data and renders to a `<canvas>`. This replaces the `<video>` + WebRTC `MediaStream` approach.
- **Keyframe caching**: Server caches the most recent SPS/PPS and keyframe so new viewers can start decoding immediately without waiting for the next keyframe from the Pi.
- **Separate networks**: Pi is on guest WiFi, MacBook on corporate network. No LAN P2P possible. All traffic must transit the cloud server.
- **No STUN/TURN needed**: Pure TCP/WebSocket eliminates the need for STUN, TURN, or any UDP infrastructure.

---

## 2. Network Topology & Constraints

### Pi (pidesk.local)
- **Network**: Corporate guest WiFi (separate from corp network, no routing to corp devices)
- **Hardware**: Raspberry Pi 5 Model B Rev 1.1, 8GB RAM, ARM64 (aarch64)
- **OS**: Debian 13 (Trixie), kernel 6.18.34+rpt-rpi-2712
- **Camera**: USB webcam at `/dev/video0`, supports YUYV and **MJPEG** at 640x480, 1280x720, 1920x1080
- **Software**: ffmpeg installed, Python 3.13.5, no Go (must install or cross-compile)
- **Firewall**: Can make outbound connections (TCP to public internet)

### MacBook
- **Network**: Corporate network
- **Software**: Node.js v24.18.0, npm, Python 3.9.6, no Docker, no gcloud CLI
- **Role**: Runs the Vite dev server for the viewer app

### Cloud Run Service (or GCE VM)
- **Cloud Run**: Fully managed, auto-scaling, TLS termination, WebSocket support
- **Port**: 8080 (set by `PORT` env var, Cloud Run standard)
- **Networking**: TCP only (no UDP). WebSocket connections are supported.
- **Alternative**: GCE e2-small VM for persistent connections if needed

---

## 3. Protocol Specification

All communication happens over **WebSocket** at `ws(s)://SERVER:8080/ws`.

### Connection

```
WebSocket URL: ws(s)://server-host:8080/ws?role={publisher|viewer}&token={auth-token}

Query parameters:
  role:  "publisher" (Pi) or "viewer" (browser)
  token: shared secret, checked against server's AUTH_TOKEN env var
```

### Message Types

The WebSocket carries two types of messages:

1. **Text messages**: JSON control messages
2. **Binary messages**: H264 video data

### JSON Control Messages

#### Server → Client

```jsonc
// Sent immediately after WebSocket connection
{"type": "welcome", "id": "uuid-string"}

// Publisher status notifications (sent to viewers only)
{"type": "publisher_online"}
{"type": "publisher_offline"}

// Error
{"type": "error", "message": "human-readable error description"}
```

#### Client → Server

Currently no client→server JSON messages are defined. The publisher sends only binary video data after the initial handshake.

### Binary Message Format

Binary messages carry H264 video data with a 5-byte header:

```
Byte 0: flags
  bit 0 (0x01): keyframe — access unit contains an IDR NAL unit
  bit 1 (0x02): codec init — access unit contains SPS/PPS NAL units

Bytes 1-4: timestamp in milliseconds (uint32 big-endian)
  Relative to stream start on the publisher side.

Bytes 5+: H264 Annex B data
  Complete access unit with 4-byte start codes (0x00000001).
  May contain multiple NAL units (e.g., SPS + PPS + IDR for keyframes).
```

### Flow: Publisher (Pi)

```
Pi                                      Server
│                                         │
│─── WS connect ───────────────────────→  │
│    /ws?role=publisher&token=xxx         │
│                                         │
│  ←── {"type":"welcome","id":"..."}  ──  │
│                                         │
│─── Binary H264 frames ──────────────→  │  Server caches SPS/PPS + keyframe
│    [flags][ts][annexB data]            │  Broadcasts to all viewers
│    (continuous stream)                 │
│                                         │
│─── Binary H264 frames ──────────────→  │
│    ...                                 │
```

### Flow: Viewer (Browser)

```
Browser                                 Server
│                                         │
│─── WS connect ───────────────────────→  │
│    /ws?role=viewer&token=xxx            │
│                                         │
│  ←── {"type":"welcome","id":"..."}  ──  │
│                                         │
│  ←── {"type":"publisher_online"}    ──  │  (if publisher is connected)
│                                         │
│  ←── Binary: cached SPS/PPS        ──  │  (if available, for decoder init)
│  ←── Binary: cached keyframe       ──  │  (if available, for immediate display)
│                                         │
│  ←── Binary H264 frames            ──  │  (continuous stream from publisher)
│      [flags][ts][annexB data]          │
│      ...                               │
│                                         │
│  ←── {"type":"publisher_offline"}   ──  │  (if publisher disconnects)
```

---

## 4. Component 1: Server (Go WebSocket Relay)

### Location: `server/`

### Files

| File | Purpose |
|------|---------|
| `main.go` | All server logic (~380 lines) |
| `go.mod` | Go module definition |
| `go.sum` | Dependency checksums (auto-generated) |
| `Dockerfile` | Multi-stage Docker build for Cloud Run |

### Dependencies

```
github.com/gorilla/websocket — WebSocket server
github.com/google/uuid       — UUID generation (for client IDs)
```

No Pion/WebRTC dependencies. Pure WebSocket relay.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server port (Cloud Run sets this automatically) |
| `AUTH_TOKEN` | `cloudproxy-dev-token` | Shared secret for WebSocket auth |

### Key Server State

```go
type Server struct {
    mu            sync.RWMutex
    publisherConn *websocket.Conn
    viewers       map[string]*Viewer

    // Cached init data for new viewer joins.
    initData     []byte // most recent SPS/PPS frame (binary msg with flag 0x02)
    lastKeyframe []byte // most recent keyframe (binary msg with flag 0x01)

    authToken string
}

type Viewer struct {
    ID   string
    Conn *websocket.Conn
    mu   sync.Mutex
}
```

### Core Relay Logic

When the publisher sends a binary message:

```go
func (s *Server) handlePublisherBinary(data []byte) {
    flags := data[0]

    // Cache SPS/PPS and/or keyframe for new viewer init.
    if flags & flagSPSPPS != 0 {
        s.initData = copy(data)
    }
    if flags & flagKeyframe != 0 {
        s.lastKeyframe = copy(data)
    }

    // Broadcast to all connected viewers.
    for _, viewer := range s.viewers {
        viewer.SendBinary(data)
    }
}
```

When a new viewer connects and the publisher is already online:

```go
// Send cached data for immediate decoder bootstrap.
if publisherOnline {
    viewer.SendJSON(ControlMessage{Type: "publisher_online"})
    if cachedInit != nil {
        viewer.SendBinary(cachedInit)      // SPS/PPS for decoder config
    }
    if cachedKeyframe != nil {
        viewer.SendBinary(cachedKeyframe)  // Last keyframe for immediate display
    }
}
```

### HTTP Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/ws` | WebSocket upgrade for signaling + video relay |
| `GET` | `/health` | Health check, returns 200 |

### CORS

Allow all origins (for local Vite dev server at `http://localhost:5173`).

---

## 5. Component 2: Pi Client (Go + ffmpeg)

### Location: `pi-client/`

### Files

| File | Purpose |
|------|---------|
| `main.go` | Client logic (~530 lines) |
| `go.mod` | Go module definition |
| `go.sum` | Auto-generated |

### Dependencies

```
github.com/gorilla/websocket
```

No Pion/WebRTC dependencies. Pure WebSocket + ffmpeg pipe.

### Configuration (Environment Variables / Flags)

| Variable | Default | Description |
|----------|---------|-------------|
| `SIGNAL_URL` | `ws://localhost:8080/ws` | Server WebSocket URL |
| `AUTH_TOKEN` | `cloudproxy-dev-token` | Auth token |
| `VIDEO_DEVICE` | `/dev/video0` | V4L2 video device |
| `VIDEO_WIDTH` | `1280` | Capture width |
| `VIDEO_HEIGHT` | `720` | Capture height |
| `VIDEO_FPS` | `30` | Capture framerate |
| `VIDEO_BITRATE` | `2500k` | H264 encoding bitrate |

### Video Pipeline

```
/dev/video0 (MJPEG) → ffmpeg (transcode to H264 Annex B) → stdout pipe → Go NAL parser → binary WebSocket messages → server
```

ffmpeg command:
```bash
ffmpeg -f v4l2 -input_format mjpeg -video_size 1280x720 -framerate 30 \
  -i /dev/video0 \
  -pix_fmt yuv420p \
  -c:v libx264 -preset ultrafast -tune zerolatency \
  -profile:v baseline -level 3.1 \
  -b:v 2500k -maxrate 2500k -bufsize 5000k \
  -g 60 -keyint_min 60 -forced-idr 1 \
  -an \
  -f h264 pipe:1
```

Key ffmpeg flags:
- `-input_format mjpeg`: Camera outputs MJPEG (hardware compressed, less USB bandwidth)
- `-pix_fmt yuv420p`: Convert 4:2:2 chroma to 4:2:0 (required for H264 baseline profile)
- `-preset ultrafast -tune zerolatency`: Minimize encoding latency
- `-profile:v baseline`: Maximum browser WebCodecs compatibility
- `-g 60 -keyint_min 60`: Keyframe every 60 frames (2s at 30fps) — important for viewer join
- `-forced-idr 1`: Ensure keyframes are IDR (not recovery points)
- `-f h264 pipe:1`: Output raw H264 Annex B to stdout
- `-an`: No audio

### H264 NAL Parsing

The pi-client reads H264 Annex B data byte-by-byte from ffmpeg's stdout, detecting start codes (`0x00000001` or `0x000001`) to split the stream into individual NAL units. NAL units are grouped into access units based on VCL boundaries:

```go
// NAL types relevant for framing
nalTypeNonIDR = 1   // Non-IDR slice (P-frame)
nalTypeSPS    = 7   // Sequence Parameter Set
nalTypePPS    = 8   // Picture Parameter Set
nalTypeIDR    = 5   // IDR slice (keyframe)

// A new VCL NAL (type 1 or 5) after a previous VCL NAL = new access unit
// Non-VCL NALs (SPS, PPS) are grouped with the following VCL NAL
```

Each complete access unit is packaged into a binary WebSocket message:
```
[flags(1)][timestamp_ms(4, big-endian)][annexB data with start codes]
```

### Reconnection Logic

```
Main Loop:
  1. Connect WebSocket to server
  2. Wait for welcome message
  3. Start ffmpeg subprocess (H264 Annex B to stdout)
  4. Parse NAL units, group into access units, send as binary WS messages
  5. If any error/disconnect:
     a. Kill ffmpeg subprocess
     b. Close WebSocket
     c. Wait 3 seconds
     d. Go to step 1
```

### Graceful Shutdown

- Handle `SIGINT` and `SIGTERM`
- Kill ffmpeg subprocess (entire process group)
- Close WebSocket

---

## 6. Component 3: Viewer (Vite + TypeScript + WebCodecs)

### Location: `viewer/`

### Files

| File | Purpose |
|------|---------|
| `package.json` | Dependencies: vite, typescript |
| `tsconfig.json` | TypeScript strict config |
| `vite.config.ts` | Vite dev server config |
| `index.html` | HTML entry point, loads Inter font |
| `src/main.ts` | All application logic (~330 lines) |
| `src/style.css` | All styles (~300 lines) |

### Browser Requirements

The viewer requires **WebCodecs API** support. Compatible browsers:
- Chrome 94+
- Edge 94+
- Opera 80+
- **Not supported**: Firefox, Safari (as of 2024)

The viewer checks for WebCodecs support on load and shows an error message if not available.

### UI States

```
┌─────────────────┐     Connect      ┌─────────────────┐    First frame    ┌─────────────────┐
│   DISCONNECTED  │ ──────────────→  │   CONNECTED     │ ─────────────→   │   STREAMING     │
│                 │                   │   (waiting for  │    decoded        │                 │
│ [Server URL]    │  ←────────────── │    publisher)   │                   │ ┌─────────────┐ │
│ [Token]         │     Disconnect   │                 │                   │ │  <canvas>   │ │
│ [Connect ▶]     │                  │ "Waiting for    │  ←───────────── │ │  live feed  │ │
│                 │                  │  camera..."     │    Publisher      │ │             │ │
│ Status: ●       │                  │                 │    offline        │ │ [FPS][Res]  │ │
│ Disconnected    │                  │ Status: ●       │                   │ │ [Bitrate]   │ │
└─────────────────┘                  │ Connected       │                   │ │ [Frames]    │ │
                                     └─────────────────┘                   │ └─────────────┘ │
                                                                           │ Status: ●       │
                                                                           │ Live            │
                                                                           └─────────────────┘
```

### WebCodecs Decoding

```typescript
// Create VideoDecoder
const decoder = new VideoDecoder({
  output: (frame: VideoFrame) => {
    // Update canvas dimensions from frame
    canvas.width = frame.displayWidth;
    canvas.height = frame.displayHeight;
    // Draw frame to canvas
    ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
    frame.close();
  },
  error: (e: Error) => console.error('[decoder] error:', e),
});

// Configure for H264 baseline profile, level 3.1
decoder.configure({
  codec: 'avc1.42C01F',
  optimizeForLatency: true,
});

// When binary message received:
if (event.data instanceof ArrayBuffer) {
  const view = new DataView(event.data);
  const flags = view.getUint8(0);
  const timestamp = view.getUint32(1, false); // big-endian
  const isKeyframe = (flags & 0x01) !== 0;
  const h264Data = new Uint8Array(event.data, 5);

  const chunk = new EncodedVideoChunk({
    type: isKeyframe ? 'key' : 'delta',
    timestamp: timestamp * 1000, // ms → µs
    data: h264Data,
  });

  decoder.decode(chunk);
}
```

### Stats Collection

Stats are tracked manually (no `RTCPeerConnection.getStats()`):

```typescript
// Per-interval counters (reset every second)
let frameCount = 0;
let bytesReceived = 0;

// Decoder output callback:
frameCount++;
totalFrames++;

// Binary message handler:
bytesReceived += data.byteLength;

// Every 1 second:
const fps = Math.round(frameCount / elapsed);
const kbps = Math.round((bytesReceived * 8) / 1000 / elapsed);
```

Stats overlay shows:
- **FPS**: Decoded frames per second
- **Res**: Video resolution (from `VideoFrame.displayWidth × displayHeight`)
- **Bitrate**: WebSocket bytes received per second
- **Frames**: Total decoded frames (replaces RTT which is not measurable without WebRTC)

### Canvas Rendering

The `<canvas>` element replaces the `<video>` element. It uses the same CSS class (`video-wrap__video`) for consistent styling with the existing dark glassmorphism theme. Canvas dimensions update dynamically from decoded `VideoFrame` dimensions.

### Decoder Queue Management

To prevent decoder queue overflow (which causes increasing latency), frames are dropped when the queue is too deep:

```typescript
if (decoder.decodeQueueSize > 5) {
  console.warn('[decoder] queue overflow, skipping frame');
  return;
}
```

### Design Tokens

```css
:root {
  --bg-primary: #0a0a0f;
  --bg-secondary: #12121a;
  --surface: rgba(255, 255, 255, 0.03);
  --surface-hover: rgba(255, 255, 255, 0.06);
  --border: rgba(255, 255, 255, 0.08);
  --border-active: rgba(0, 212, 170, 0.3);
  --accent-primary: #00d4aa;
  --accent-secondary: #00b4d8;
  --text-primary: #e8e8e8;
  --text-secondary: #888888;
  --error: #ff4757;
  --success: #00d4aa;
  --warning: #ffa502;
  --font-family: 'Inter', -apple-system, sans-serif;
  --radius: 12px;
  --blur: 20px;
}
```

---

## 7. Build & Cross-Compilation

### Installing Go

**On MacBook** (for cross-compiling):
```bash
brew install go
```

**On Pi** (if building natively):
```bash
sudo apt install golang-go
```

### Building the Server

```bash
cd server
go mod tidy
go build -o cloudproxy-server .
```

Docker build (for Cloud Run):
```bash
cd server
docker build -t cloudproxy-server .
```

### Building the Pi Client

**Option A: Cross-compile on Mac** (preferred — no Go needed on Pi):
```bash
cd pi-client
go mod tidy
GOOS=linux GOARCH=arm64 go build -o cloudproxy-pi-client .
# Copy binary to Pi:
scp cloudproxy-pi-client pidesk.local:~/
```

**Option B: Build on Pi** (requires Go on Pi):
```bash
ssh pidesk.local
cd pi-client
go build -o cloudproxy-pi-client .
```

### Building the Viewer

```bash
cd viewer
npm install
npm run dev  # for development (http://localhost:5173)
npm run build  # for production (output in dist/)
```

---

## 8. Deployment Guide

### Cloud Run Deployment (Recommended)

Cloud Run provides auto-TLS, auto-scaling, and WebSocket support over TCP.

```bash
# Build and push Docker image
cd server
gcloud builds submit --tag gcr.io/PROJECT_ID/cloudproxy-server

# Deploy to Cloud Run
gcloud run deploy cloudproxy-server \
  --image gcr.io/PROJECT_ID/cloudproxy-server \
  --region us-west1 \
  --port 8080 \
  --allow-unauthenticated \
  --set-env-vars AUTH_TOKEN=your-secret-token-here \
  --min-instances 1 \
  --max-instances 1 \
  --session-affinity \
  --timeout 3600
```

Key Cloud Run settings:
- `--min-instances 1`: Keep at least one instance warm for low-latency connections
- `--session-affinity`: Ensure WebSocket connections stick to the same instance
- `--timeout 3600`: Allow long-lived WebSocket connections (1 hour max)
- No UDP ports needed — pure TCP/WebSocket

### GCE VM Setup (Alternative)

If persistent connections beyond Cloud Run's timeout are needed:

```bash
# Create VM
gcloud compute instances create cloudproxy-server \
  --zone=us-west1-b \
  --machine-type=e2-small \
  --image-family=debian-12 \
  --image-project=debian-cloud \
  --boot-disk-size=20GB \
  --tags=cloudproxy

# Firewall: only TCP 8080 needed (no UDP ports!)
gcloud compute firewall-rules create cloudproxy-http \
  --allow=tcp:8080 --target-tags=cloudproxy
```

### Deploy Server to GCE

```bash
# Option 1: Docker
sudo docker run -d \
  --name cloudproxy \
  --restart=always \
  --network=host \
  -e AUTH_TOKEN=your-secret-token-here \
  -e PORT=8080 \
  cloudproxy-server

# Option 2: Binary
scp cloudproxy-server user@$STATIC_IP:~/
ssh user@$STATIC_IP
chmod +x cloudproxy-server
AUTH_TOKEN=your-secret-token ./cloudproxy-server
```

### Deploy Pi Client

```bash
# Cross-compile on Mac
cd pi-client && GOOS=linux GOARCH=arm64 go build -o cloudproxy-pi-client .

# Copy to Pi
scp cloudproxy-pi-client pidesk.local:~/

# Run on Pi
ssh pidesk.local
SIGNAL_URL=wss://cloudproxy-server-xxx-uc.a.run.app/ws AUTH_TOKEN=your-secret-token ./cloudproxy-pi-client
```

For auto-start on boot (systemd service on Pi):
```ini
# /etc/systemd/system/cloudproxy-pi.service
[Unit]
Description=CloudProxy Pi Camera Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=SIGNAL_URL=wss://cloudproxy-server-xxx-uc.a.run.app/ws
Environment=AUTH_TOKEN=your-secret-token
Environment=VIDEO_DEVICE=/dev/video0
Environment=VIDEO_WIDTH=1280
Environment=VIDEO_HEIGHT=720
Environment=VIDEO_FPS=15
ExecStart=/home/pi/cloudproxy-pi-client
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

### Run Viewer (Local Dev)

```bash
cd viewer
npm run dev
# Open http://localhost:5173
# Enter server URL: wss://cloudproxy-server-xxx-uc.a.run.app/ws
# Enter token: your-secret-token
# Click Connect
```

---

## 9. Testing Checklist

### Local Testing (without Cloud)

1. **Start server locally**:
   ```bash
   cd server && go run .
   # Listening on :8080
   ```

2. **Start Pi client** (on the Pi or locally if you have a webcam):
   ```bash
   cd pi-client && SIGNAL_URL=ws://localhost:8080/ws go run .
   ```

3. **Start viewer**:
   ```bash
   cd viewer && npm run dev
   # Open http://localhost:5173, connect to ws://localhost:8080/ws
   ```

### Integration Testing Checklist

- [ ] Server starts and listens on port 8080
- [ ] Server `/health` endpoint returns 200
- [ ] Pi client connects WebSocket to server
- [ ] Pi client receives welcome message
- [ ] Pi client starts sending binary H264 frames
- [ ] Server logs binary frame reception with stats
- [ ] Viewer connects WebSocket to server
- [ ] Viewer receives "publisher_online" message
- [ ] Viewer receives cached SPS/PPS and keyframe (if available)
- [ ] WebCodecs VideoDecoder initializes without errors
- [ ] Video appears on browser `<canvas>` element
- [ ] Stats overlay shows FPS, resolution, bitrate, frame count
- [ ] Pi client disconnect → viewer shows "publisher_offline"
- [ ] Pi client reconnect → viewer auto-receives new stream
- [ ] Multiple viewers can connect simultaneously
- [ ] Server correctly replaces stale publisher connections

---

## 10. Troubleshooting

### Common Issues

| Problem | Cause | Solution |
|---------|-------|----------|
| No video in browser | WebCodecs not supported | Use Chrome 94+, Edge 94+, or Opera 80+. Firefox/Safari don't support WebCodecs. |
| "VideoDecoder error" | H264 profile mismatch | Ensure ffmpeg uses `-profile:v baseline`. WebCodecs codec string is `avc1.42C01F`. |
| ffmpeg "device busy" | Another process using camera | `sudo fuser /dev/video0` to find, kill the process |
| High latency (>500ms) | TCP head-of-line blocking or decoder queue buildup | Check network quality. Decoder drops frames when `decodeQueueSize > 5`. Reduce bitrate if needed. |
| Viewer connects but no video | Publisher not sending or frames not reaching viewer | Check server logs for "Broadcasting frame" messages. Ensure publisher is connected. |
| WebSocket 401 | Token mismatch | Check `AUTH_TOKEN` env var matches on all components |
| Canvas is blank | Viewer connected before publisher started | Wait for publisher to come online. Cached keyframe should bootstrap immediately. |
| Decoder queue overflow warnings | Network or decode throughput too low | Reduce resolution/bitrate in pi-client config. Check CPU usage in browser. |

### Latency Considerations

Since we moved from WebRTC (UDP) to WebSocket (TCP), latency is higher:

- **WebRTC (old)**: ~60-160ms end-to-end (UDP, no head-of-line blocking)
- **WebSocket (current)**: ~100-300ms end-to-end (TCP, head-of-line blocking on packet loss)

This tradeoff is acceptable because:
1. Cloud Run only supports TCP — no choice
2. For a monitoring camera, 100-300ms is fine
3. TCP provides reliable delivery (no random frame corruption)

### Adding TLS (Production)

Cloud Run provides automatic TLS. For GCE VM:

```bash
# Install nginx + certbot
sudo apt install nginx certbot python3-certbot-nginx

# Get certificate
sudo certbot --nginx -d cloudproxy.yourdomain.com

# nginx proxies WSS (443) → WS (localhost:8080)
```

---

## 11. Future Enhancements

- [ ] **Audio support**: Add audio track from Pi microphone
- [ ] **Data channel**: Send commands to Pi via WebSocket text messages (e.g., PTZ controls)
- [ ] **Multiple cameras**: Support multiple Pi publishers with stream IDs
- [ ] **Recording**: Save stream to disk on the server
- [ ] **TLS/nginx**: For GCE deployment, put server behind nginx with Let's Encrypt
- [ ] **Authentication**: Replace shared token with JWT or OAuth
- [ ] **Adaptive bitrate**: Server requests lower bitrate from Pi based on viewer bandwidth
- [ ] **Mobile viewer**: Progressive web app for mobile browsers (Chrome Android supports WebCodecs)
- [ ] **Firefox/Safari**: Fall back to MSE (Media Source Extensions) for browsers without WebCodecs

---

## 12. Implementation Status

### Current Phase: Cloud Run Deployed ✅ — Testing

| Component | Status | Notes |
|-----------|--------|-------|
| Server (`server/main.go`) | ✅ Done | WebSocket binary relay, SPS/PPS + keyframe caching |
| Pi Client (`pi-client/main.go`) | ✅ Done | ffmpeg → H264 Annex B pipe → NAL parser → binary WS |
| Viewer (`viewer/`) | ✅ Done | WebCodecs VideoDecoder, canvas rendering, manual stats |
| ARCHITECTURE.md | ✅ Done | This document (updated for WebSocket architecture) |
| Cross-compile pi-client | ✅ Done | `GOOS=linux GOARCH=arm64 go build` → SCP to Pi |
| Local end-to-end test | ✅ Done | Pi → Mac server → Mac browser, 30fps 720p working |
| Publisher reconnection | ✅ Done | Server replaces stale publishers instead of rejecting |
| Cloud Run deployment | ✅ Done | `hansel-487018` / `us-west1` / revision `cloudproxy-server-00007-7rb` |
| Cloud Run auth proxy | ✅ Done | Vite dev server proxies WS + injects Google identity token |
| DNS/domain | ⏳ TODO | Optional — can use Cloud Run default URL initially |

### Cloud Run Details

| Setting | Value |
|---------|-------|
| Service URL | `https://cloudproxy-server-530731599092.us-west1.run.app` |
| GCP Project | `hansel-487018` |
| Region | `us-west1` |
| AUTH_TOKEN | `your-secret-token` |
| Min instances | 1 (always warm) |
| Max instances | 1 (single instance for WS state) |
| Session affinity | Enabled |
| Timeout | 3600s (1 hour) |
| Public access | Blocked by org policy — requires Google identity token |

### Auth: Google Org Policy

The Google org policy prevents `allUsers` IAM binding on Cloud Run. All requests
require a valid Google identity token in the `Authorization: Bearer` header.

- **Viewer**: The Vite dev server proxies `/ws` to Cloud Run and injects the
  identity token automatically (via `gcloud auth print-identity-token`).
- **Pi client**: Use `gcloud run services proxy` to tunnel from the Pi, or
  run the Pi on a GCE VM where the metadata server provides identity tokens.

### What's Working Now

```
Pi (guest WiFi)
    ↓ WebSocket binary (H264 Annex B)
Cloud Run (wss://cloudproxy-server-530731599092.us-west1.run.app/ws)
    ↓ WebSocket binary (H264 Annex B, forwarded)
Vite dev server (localhost:3000 → proxies /ws to Cloud Run + injects auth)
    ↓
Browser (localhost:3000 → ws://localhost:3000/ws)
    ↓ WebCodecs VideoDecoder → <canvas>
```

**Confirmed working locally**: 1280×720 @ 30fps, ~2500kbps

### How to Run (Cloud Run)

```bash
# Terminal 1: Viewer (Mac) — includes WS proxy to Cloud Run
cd viewer && npm run dev
# Open http://localhost:3000
# Default URL: ws://localhost:3000/ws (proxied to Cloud Run)
# Default token: your-secret-token

# Terminal 2: Pi client (on the Pi via SSH)
# Option A: Direct (if Pi has gcloud configured)
SIGNAL_URL=wss://cloudproxy-server-530731599092.us-west1.run.app/ws AUTH_TOKEN=your-secret-token ~/cloudproxy-pi-client
# Option B: Via gcloud proxy tunnel
gcloud run services proxy cloudproxy-server --region=us-west1 --port=8080
SIGNAL_URL=ws://localhost:8080/ws AUTH_TOKEN=your-secret-token ~/cloudproxy-pi-client
```

### How to Run (Local — no Cloud Run)

```bash
# Terminal 1: Server (Mac)
cd server && go run .

# Terminal 2: Pi client (on the Pi via SSH)
SIGNAL_URL=ws://192.168.100.1:8080/ws VIDEO_FPS=30 VIDEO_BITRATE=3000k ~/cloudproxy-pi-client

# Terminal 3: Viewer (Mac)
cd viewer && npm run dev
# Open http://localhost:3000, change URL to ws://localhost:8080/ws
```

### Lessons Learned (Critical for Future Agents)

> [!CAUTION]
> **WebCodecs requires Chrome/Edge.** Firefox and Safari do not support `VideoDecoder` as of 2024.
> If cross-browser support is needed, add an MSE (Media Source Extensions) fallback path.

> [!IMPORTANT]
> **`-pix_fmt yuv420p`** is mandatory in ffmpeg args — the webcam outputs MJPEG 4:2:2, but H264 baseline requires 4:2:0.

Other important notes:
- **Publisher reconnection**: Server must accept new publisher connections even if a stale one exists (replace, don't reject)
- **Keyframe caching**: Server caches SPS/PPS and last keyframe so new viewers can start immediately
- **Decoder queue management**: Drop frames when `decodeQueueSize > 5` to prevent latency buildup
- **`forced-idr 1`**: Ensure ffmpeg produces true IDR frames (not recovery points) so the WebCodecs decoder can use them as keyframes
- **Binary type**: Set `ws.binaryType = 'arraybuffer'` on the viewer WebSocket before receiving data

### Git Repository

- **Remote**: `git@github.com:jgunaratne/cloudproxy.git`
- **Branch**: `main`

### File Structure

```
cloudproxy/
├── server/
│   ├── main.go              # WebSocket relay server (binary broadcast)
│   ├── go.mod
│   ├── go.sum
│   └── Dockerfile           # Multi-stage Docker build for Cloud Run
├── pi-client/
│   ├── main.go              # Pi camera client (ffmpeg → H264 Annex B → WS)
│   ├── go.mod
│   └── go.sum
├── viewer/
│   ├── package.json
│   ├── tsconfig.json
│   ├── vite.config.ts
│   ├── index.html
│   └── src/
│       ├── main.ts           # WebCodecs viewer logic
│       └── style.css         # Dark glassmorphism theme
├── .gitignore
└── ARCHITECTURE.md           # ← This file (single source of truth)
```
