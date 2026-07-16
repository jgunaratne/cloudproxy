# CloudProxy — Complete Implementation Reference

> **Purpose**: Stream live video from a Raspberry Pi 5 webcam through a Google Cloud server to a MacBook browser, with lowest possible latency.
>
> **This document is the single source of truth** for the project. It contains everything needed for any developer or coding agent to understand, build, deploy, and debug the system.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Network Topology & Constraints](#2-network-topology--constraints)
3. [Signaling Protocol Specification](#3-signaling-protocol-specification)
4. [Component 1: Server (Go + Pion SFU)](#4-component-1-server-go--pion-sfu)
5. [Component 2: Pi Client (Go + Pion + ffmpeg)](#5-component-2-pi-client-go--pion--ffmpeg)
6. [Component 3: Viewer (Vite + TypeScript + Browser WebRTC)](#6-component-3-viewer-vite--typescript--browser-webrtc)
7. [Build & Cross-Compilation](#7-build--cross-compilation)
8. [Deployment Guide](#8-deployment-guide)
9. [Testing Checklist](#9-testing-checklist)
10. [Troubleshooting](#10-troubleshooting)
11. [Future Enhancements (Phase 2)](#11-future-enhancements-phase-2)
12. [Implementation Status](#12-implementation-status)

---

## 1. Architecture Overview

```
Guest WiFi Network              Google Cloud (GCE VM)            Corporate Network
┌──────────────────┐            ┌──────────────────────┐         ┌──────────────────┐
│  Raspberry Pi 5  │            │  cloudproxy-server   │         │  MacBook         │
│                  │            │                      │         │                  │
│  USB Webcam      │            │  ┌────────────────┐  │         │  ┌────────────┐  │
│  (/dev/video0)   │            │  │  Pion SFU      │  │         │  │ Vite App   │  │
│        │         │            │  │  (Go binary)   │  │         │  │ (Browser)  │  │
│        ▼         │            │  │                │  │         │  │            │  │
│  ┌──────────┐    │  WebRTC    │  │  Receives H264 │  │ WebRTC  │  │ <video>    │  │
│  │ ffmpeg   │    │  (UDP)     │  │  from Pi via   │  │ (UDP)   │  │ element    │  │
│  │ H264 enc │────┼───────────┼──│  WebRTC track  │──┼─────────┼──│ HW decode  │  │
│  └──────────┘    │            │  │                │  │         │  └────────────┘  │
│        │         │            │  │  Forwards to   │  │         │        │         │
│        ▼         │  WSS       │  │  browser via   │  │  WSS    │        ▼         │
│  ┌──────────┐    │  signaling │  │  WebRTC track  │  │ signal  │  ┌────────────┐  │
│  │ Pion     │────┼───────────┼──│                │──┼─────────┼──│ WebSocket  │  │
│  │ client   │    │  (TCP 443) │  └────────────────┘  │ (TCP)   │  │ signaling  │  │
│  └──────────┘    │            │                      │         │  └────────────┘  │
└──────────────────┘            │  Port 8080 (HTTP)    │         └──────────────────┘
                                │  (behind nginx/LB    │
                                │   on 443 in prod)    │
                                └──────────────────────┘
```

### Data Flow Summary

| Path | Transport | Protocol | Content | Latency |
|------|-----------|----------|---------|---------|
| Pi → Server (signaling) | TCP/WSS | WebSocket | SDP offers/answers, ICE candidates | N/A |
| Pi → Server (media) | UDP | WebRTC (RTP/SRTP) | H264 video stream | ~30-80ms |
| Server → Browser (signaling) | TCP/WSS | WebSocket | SDP offers/answers, ICE candidates | N/A |
| Server → Browser (media) | UDP | WebRTC (RTP/SRTP) | H264 video stream (forwarded) | ~30-80ms |
| **Total end-to-end** | | | | **~60-160ms** |

### Why This Architecture

- **WebRTC on both legs**: The Pi is on guest WiFi (packet loss likely). UDP/WebRTC handles loss gracefully (dropped frames, no stalls). TCP/WebSocket would cause head-of-line blocking and visible freezes.
- **SFU pattern (not MCU)**: Server forwards RTP packets unchanged. No transcoding. Minimal CPU. One `TrackLocalStaticRTP` serves all viewers.
- **No TURN server needed initially**: The GCE VM has a public IP. Both Pi and browser connect directly to it. TURN only needed if corporate firewall blocks all outbound UDP (add later if needed).
- **Separate networks**: Pi is on guest WiFi, MacBook on corporate network. No LAN P2P possible. All traffic must transit the cloud server.

---

## 2. Network Topology & Constraints

### Pi (pidesk.local)
- **Network**: Corporate guest WiFi (separate from corp network, no routing to corp devices)
- **Hardware**: Raspberry Pi 5 Model B Rev 1.1, 8GB RAM, ARM64 (aarch64)
- **OS**: Debian 13 (Trixie), kernel 6.18.34+rpt-rpi-2712
- **Camera**: USB webcam at `/dev/video0`, supports YUYV and **MJPEG** at 640x480, 1280x720, 1920x1080
- **Software**: ffmpeg installed, Python 3.13.5, no Go (must install or cross-compile)
- **Firewall**: Can make outbound connections (TCP/UDP to public internet)

### MacBook
- **Network**: Corporate network
- **Software**: Node.js v24.18.0, npm, Python 3.9.6, no Go, no Docker, no gcloud CLI
- **Role**: Runs the Vite dev server for the viewer app

### GCE VM (to be created)
- **Recommended**: e2-small (2 vCPU, 2GB RAM), ~$13/month
- **Region**: us-west1 (closest to user, adjust as needed)
- **OS**: Debian 12 or Ubuntu 22.04
- **Ports**: 8080 (HTTP/WebSocket), UDP range for WebRTC (50000-60000)
- **Static IP**: Assign one for stable ICE candidates

---

## 3. Signaling Protocol Specification

All signaling happens over **WebSocket** at `ws(s)://SERVER:8080/ws`.

### Connection

```
WebSocket URL: ws(s)://server-host:8080/ws?role={publisher|viewer}&token={auth-token}

Query parameters:
  role:  "publisher" (Pi) or "viewer" (browser)
  token: shared secret, checked against server's AUTH_TOKEN env var
```

### Message Format

All messages are JSON objects with a `type` field.

#### Server → Client Messages

```jsonc
// Sent immediately after WebSocket connection
{"type": "welcome", "id": "uuid-string"}

// SDP offer (server → viewer, after publisher track is available)
{"type": "offer", "sdp": "<sdp-string>"}

// SDP answer (server → publisher, in response to publisher's offer)
{"type": "answer", "sdp": "<sdp-string>"}

// ICE candidate
{"type": "candidate", "candidate": "<json-string-of-RTCIceCandidate>"}

// Publisher status notifications (sent to viewers only)
{"type": "publisher_online"}
{"type": "publisher_offline"}

// Error
{"type": "error", "message": "human-readable error description"}
```

#### Client → Server Messages

```jsonc
// SDP offer (publisher → server, with H264 video track)
{"type": "offer", "sdp": "<sdp-string>"}

// SDP answer (viewer → server, in response to server's offer)
{"type": "answer", "sdp": "<sdp-string>"}

// ICE candidate
{"type": "candidate", "candidate": "<json-string-of-RTCIceCandidate>"}
```

### Signaling Flow: Publisher (Pi)

```
Pi                                      Server
│                                         │
│─── WS connect ───────────────────────→  │
│    /ws?role=publisher&token=xxx         │
│                                         │
│  ←── {"type":"welcome","id":"..."}  ──  │
│                                         │
│─── {"type":"offer","sdp":"..."}  ─────→ │  Server creates PeerConnection
│    (SDP with H264 video track)          │  Sets remote description (offer)
│                                         │  Creates answer
│  ←── {"type":"answer","sdp":"..."}  ──  │
│                                         │
│ ←→ ICE candidate exchange ←→            │
│    {"type":"candidate",...}              │
│                                         │
│ ════ WebRTC media flows (UDP) ════════  │  Server OnTrack: creates
│      H264 RTP packets                  │  TrackLocalStaticRTP, starts
│                                         │  forwarding goroutine
```

### Signaling Flow: Viewer (Browser)

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
│  ←── {"type":"offer","sdp":"..."}   ──  │  Server creates PeerConnection
│      (SDP with video track from Pi)     │  for this viewer, adds localTrack,
│                                         │  creates offer
│─── {"type":"answer","sdp":"..."}  ────→ │
│                                         │
│ ←→ ICE candidate exchange ←→            │
│                                         │
│ ════ WebRTC media flows (UDP) ════════  │  Browser receives H264 RTP,
│      H264 RTP packets                  │  hardware decodes, renders
│                                         │  in <video> element
│                                         │
│  ←── {"type":"publisher_offline"}   ──  │  (if publisher disconnects)
```

### ICE Candidate Format

The `candidate` field is a **JSON string** (not a raw object). This means it's double-encoded in the WebSocket message:

```json
{
  "type": "candidate",
  "candidate": "{\"candidate\":\"candidate:123 1 udp ...\",\"sdpMid\":\"0\",\"sdpMLineIndex\":0}"
}
```

Parsing in TypeScript:
```typescript
const msg = JSON.parse(event.data);        // parse WebSocket message
const iceCandidate = JSON.parse(msg.candidate);  // parse candidate field
await pc.addIceCandidate(new RTCIceCandidate(iceCandidate));
```

Parsing in Go (Pion):
```go
var candidateInit webrtc.ICECandidateInit
json.Unmarshal([]byte(msg.Candidate), &candidateInit)
peerConnection.AddICECandidate(candidateInit)
```

---

## 4. Component 1: Server (Go + Pion SFU)

### Location: `server/`

### Files

| File | Purpose |
|------|---------|
| `main.go` | All server logic (~300 lines) |
| `go.mod` | Go module definition |
| `go.sum` | Dependency checksums (auto-generated) |
| `Dockerfile` | Multi-stage Docker build |

### Dependencies

```
github.com/pion/webrtc/v4    — WebRTC implementation
github.com/gorilla/websocket — WebSocket server
github.com/google/uuid       — UUID generation (for client IDs)
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server port |
| `AUTH_TOKEN` | `cloudproxy-dev-token` | Shared secret for WebSocket auth |
| `TURN_SERVERS` | (empty) | Optional TURN servers, format: `turn:host:port,username,credential` |

### Key Server State

```go
type Server struct {
    mu sync.RWMutex

    // Publisher (Pi) state — at most one
    publisherWS   *websocket.Conn
    publisherPC   *webrtc.PeerConnection
    localTrack    *webrtc.TrackLocalStaticRTP  // created from publisher's remote track

    // Viewers — zero or more
    viewers map[string]*Viewer
}

type Viewer struct {
    id string
    ws *websocket.Conn
    pc *webrtc.PeerConnection
}
```

### Core SFU Logic: Track Forwarding

When the Pi's WebRTC connection fires `OnTrack`:

```go
publisherPC.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
    // 1. Create a local track with the same codec
    localTrack, _ := webrtc.NewTrackLocalStaticRTP(
        remoteTrack.Codec().RTPCodecCapability,
        "video", "cloudproxy",
    )
    server.localTrack = localTrack

    // 2. Periodically request keyframes (PLI) from publisher
    go func() {
        ticker := time.NewTicker(3 * time.Second)
        for range ticker.C {
            pc.WriteRTCP([]rtcp.Packet{
                &rtcp.PictureLossIndication{MediaSSRC: uint32(remoteTrack.SSRC())},
            })
        }
    }()

    // 3. Forward RTP packets in a goroutine (4096 byte buffer)
    go func() {
        buf := make([]byte, 4096)
        for {
            n, _, err := remoteTrack.Read(buf)
            if err != nil { return }
            localTrack.Write(buf[:n])
        }
    }()

    // 4. Notify waiting viewers and initiate negotiation
    server.negotiateAllViewers()
})
```

When a viewer connects and the publisher is already online:

```go
func (s *Server) negotiateViewer(viewer *Viewer) {
    // Create PeerConnection for viewer
    viewerPC, _ := webrtc.NewPeerConnection(config)

    // Add the shared local track
    viewerPC.AddTrack(s.localTrack)

    // Create offer and send to viewer
    offer, _ := viewerPC.CreateOffer(nil)
    viewerPC.SetLocalDescription(offer)
    viewer.ws.WriteJSON(Message{Type: "offer", SDP: offer.SDP})

    // Handle answer from viewer
    // Handle ICE candidates
}
```

### HTTP Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/ws` | WebSocket upgrade for signaling |
| `GET` | `/health` | Health check, returns 200 |

### CORS

Allow all origins (for local Vite dev server at `http://localhost:5173`).

---

## 5. Component 2: Pi Client (Go + Pion + ffmpeg)

### Location: `pi-client/`

### Files

| File | Purpose |
|------|---------|
| `main.go` | Client logic (~200 lines) |
| `go.mod` | Go module definition |
| `go.sum` | Auto-generated |

### Dependencies

```
github.com/pion/webrtc/v4
github.com/gorilla/websocket
```

### Configuration (Environment Variables / Flags)

| Variable | Default | Description |
|----------|---------|-------------|
| `SIGNAL_URL` | `ws://localhost:8080/ws` | Signaling server WebSocket URL |
| `AUTH_TOKEN` | `cloudproxy-dev-token` | Auth token |
| `VIDEO_DEVICE` | `/dev/video0` | V4L2 video device |
| `VIDEO_WIDTH` | `1280` | Capture width |
| `VIDEO_HEIGHT` | `720` | Capture height |
| `VIDEO_FPS` | `30` | Capture framerate |
| `VIDEO_BITRATE` | `2500k` | H264 encoding bitrate |

### Video Pipeline

```
/dev/video0 (MJPEG) → ffmpeg (transcode to H264, output RTP) → UDP 127.0.0.1:PORT → Go program reads RTP → Pion TrackLocalStaticRTP → WebRTC to server
```

> **Important**: Do NOT use `TrackLocalStaticSample` with Annex B pipe (`-f h264 pipe:1`).
> Pion's built-in H264 packetizer has issues with FU-A fragmentation that cause
> green band artifacts in the browser decoder. Using ffmpeg's native RTP output
> (`-f rtp`) bypasses this entirely and produces correct RTP packets.

ffmpeg command:
```bash
ffmpeg -f v4l2 -input_format mjpeg -video_size 1280x720 -framerate 30 \
  -i /dev/video0 \
  -pix_fmt yuv420p \
  -c:v libx264 -preset ultrafast -tune zerolatency \
  -profile:v baseline -level 3.1 \
  -b:v 2500k -maxrate 2500k -bufsize 5000k \
  -g 60 -keyint_min 60 \
  -an \
  -f rtp rtp://127.0.0.1:PORT?pkt_size=1200
```

Key ffmpeg flags:
- `-input_format mjpeg`: Camera outputs MJPEG (hardware compressed, less USB bandwidth)
- `-pix_fmt yuv420p`: Convert 4:2:2 chroma to 4:2:0 (required for H264 baseline profile)
- `-preset ultrafast -tune zerolatency`: Minimize encoding latency
- `-profile:v baseline`: Maximum browser compatibility
- `-g 60 -keyint_min 60`: Keyframe every 60 frames (2s at 30fps) — important for viewer join
- `-f rtp`: Output RTP packets to UDP (ffmpeg handles H264→RTP packetization correctly)
- `pkt_size=1200`: Keep RTP packets under typical MTU
- `-an`: No audio

### RTP Forwarding (replaces H264 NAL parsing)

The Pi client opens a local UDP listener, tells ffmpeg to send RTP to it,
and forwards raw RTP packets directly to the Pion `TrackLocalStaticRTP`:

```go
// Open local UDP listener for ffmpeg RTP output
listener, _ := net.ListenPacket("udp4", "127.0.0.1:0")
rtpPort := listener.LocalAddr().(*net.UDPAddr).Port

// Create RTP-based track (not Sample-based)
videoTrack, _ := webrtc.NewTrackLocalStaticRTP(
    webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
    "video", "pi-camera",
)

// Forward RTP packets from ffmpeg → WebRTC
buf := make([]byte, 4096)
for {
    n, _, _ := listener.ReadFrom(buf)
    if n >= 12 && (buf[0]>>6) == 2 {  // RTP version check
        videoTrack.Write(buf[:n])
    }
}
```

### Reconnection Logic

```
Main Loop:
  1. Connect WebSocket to signaling server
  2. Start ffmpeg subprocess
  3. Create PeerConnection, create offer, send via WS
  4. Receive answer, exchange ICE candidates
  5. Start forwarding H264 from ffmpeg to WebRTC track
  6. If any error/disconnect:
     a. Close PeerConnection
     b. Kill ffmpeg
     c. Close WebSocket
     d. Wait 3 seconds
     e. Go to step 1
```

### Graceful Shutdown

- Handle `SIGINT` and `SIGTERM`
- Kill ffmpeg subprocess
- Close PeerConnection
- Close WebSocket

---

## 6. Component 3: Viewer (Vite + TypeScript + Browser WebRTC)

### Location: `viewer/`

### Files

| File | Purpose |
|------|---------|
| `package.json` | Dependencies: vite, typescript |
| `tsconfig.json` | TypeScript strict config |
| `vite.config.ts` | Vite dev server config |
| `index.html` | HTML entry point, loads Inter font |
| `src/main.ts` | All application logic (~250 lines) |
| `src/style.css` | All styles (~300 lines) |

### UI States

```
┌─────────────────┐     Connect      ┌─────────────────┐    Publisher     ┌─────────────────┐
│   DISCONNECTED  │ ──────────────→  │   CONNECTED     │ ─────────────→  │   STREAMING     │
│                 │                   │   (waiting for  │    comes        │                 │
│ [Server URL]    │  ←────────────── │    publisher)   │    online       │ ┌─────────────┐ │
│ [Token]         │     Disconnect   │                 │                  │ │  <video>    │ │
│ [Connect ▶]     │                  │ "Waiting for    │  ←───────────── │ │  live feed  │ │
│                 │                  │  camera..."     │    Publisher     │ │             │ │
│ Status: ●       │                  │                 │    offline       │ │ [FPS][Res]  │ │
│ Disconnected    │                  │ Status: ●       │                  │ │ [Bitrate]   │ │
└─────────────────┘                  │ Connected       │                  │ └─────────────┘ │
                                     └─────────────────┘                  │ Status: ●       │
                                                                          │ Streaming       │
                                                                          └─────────────────┘
```

### WebRTC Connection Code (Browser)

```typescript
// Create peer connection
const pc = new RTCPeerConnection({
  iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
});

// Handle incoming track
pc.ontrack = (event) => {
  const video = document.getElementById('video') as HTMLVideoElement;
  video.srcObject = event.streams[0];
};

// When server sends offer:
await pc.setRemoteDescription(new RTCSessionDescription({
  type: 'offer',
  sdp: msg.sdp
}));
const answer = await pc.createAnswer();
await pc.setLocalDescription(answer);
ws.send(JSON.stringify({ type: 'answer', sdp: answer.sdp }));

// ICE candidates
pc.onicecandidate = (event) => {
  if (event.candidate) {
    ws.send(JSON.stringify({
      type: 'candidate',
      candidate: JSON.stringify(event.candidate.toJSON())
    }));
  }
};
```

### Stats Collection

Poll `RTCPeerConnection.getStats()` every 1 second:

```typescript
const stats = await pc.getStats();
stats.forEach((report) => {
  if (report.type === 'inbound-rtp' && report.kind === 'video') {
    // Frames per second
    const fps = report.framesPerSecond;
    // Resolution
    const width = report.frameWidth;
    const height = report.frameHeight;
    // Bitrate (calculate from bytesReceived delta)
    const bitrate = (report.bytesReceived - prevBytes) * 8; // bits/sec
    prevBytes = report.bytesReceived;
  }
  if (report.type === 'candidate-pair' && report.state === 'succeeded') {
    const rtt = report.currentRoundTripTime * 1000; // ms
  }
});
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

Docker build:
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

### GCE VM Setup

```bash
# Create VM
gcloud compute instances create cloudproxy-server \
  --zone=us-west1-b \
  --machine-type=e2-small \
  --image-family=debian-12 \
  --image-project=debian-cloud \
  --boot-disk-size=20GB \
  --tags=cloudproxy

# Reserve static IP
gcloud compute addresses create cloudproxy-ip --region=us-west1
STATIC_IP=$(gcloud compute addresses describe cloudproxy-ip --region=us-west1 --format='value(address)')

# Assign to VM
gcloud compute instances delete-access-config cloudproxy-server \
  --zone=us-west1-b --access-config-name="External NAT"
gcloud compute instances add-access-config cloudproxy-server \
  --zone=us-west1-b --address=$STATIC_IP

# Firewall rules
gcloud compute firewall-rules create cloudproxy-http \
  --allow=tcp:8080 --target-tags=cloudproxy
gcloud compute firewall-rules create cloudproxy-webrtc \
  --allow=udp:50000-60000 --target-tags=cloudproxy
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
SIGNAL_URL=ws://$STATIC_IP:8080/ws AUTH_TOKEN=your-secret-token ./cloudproxy-pi-client
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
Environment=SIGNAL_URL=ws://SERVER_IP:8080/ws
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
# Enter server URL: ws://SERVER_IP:8080/ws
# Enter token: your-secret-token
# Click Connect
```

---

## 9. Testing Checklist

### Local Testing (without GCE VM)

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
- [ ] Pi client sends SDP offer
- [ ] Server responds with SDP answer
- [ ] ICE candidates exchange completes
- [ ] Server logs "publisher connected" and "track received"
- [ ] Viewer connects WebSocket to server
- [ ] Viewer receives "publisher_online" message
- [ ] Viewer receives SDP offer from server
- [ ] Viewer sends SDP answer
- [ ] ICE candidates exchange completes
- [ ] Video appears in browser `<video>` element
- [ ] Stats overlay shows FPS, resolution, bitrate, RTT
- [ ] Pi client disconnect → viewer shows "publisher_offline"
- [ ] Pi client reconnect → viewer auto-receives new stream
- [ ] Multiple viewers can connect simultaneously

---

## 10. Troubleshooting

### Common Issues

| Problem | Cause | Solution |
|---------|-------|----------|
| No video in browser | ICE failed (UDP blocked) | Check firewall rules. Add TURN server as fallback. |
| ffmpeg "device busy" | Another process using camera | `sudo fuser /dev/video0` to find, kill the process |
| "codec not supported" | H264 profile mismatch | Ensure ffmpeg uses `-profile:v baseline` |
| High latency (>500ms) | TCP fallback or buffering | Check ICE candidate type (should be "host" or "srflx", not "relay"). Check ffmpeg has `-tune zerolatency`. |
| Viewer gets offer but no video | Track not forwarding | Check server logs for "OnTrack" event. Ensure forwarding goroutine is running. |
| WebSocket 401 | Token mismatch | Check `AUTH_TOKEN` env var matches on all components |

### H264 NAL Unit Parser Fallback

If `h264reader` from Pion isn't available at the expected import path, use this simple Annex B parser:

```go
func splitNALUnits(data []byte) [][]byte {
    var nals [][]byte
    start := 0

    for i := 0; i < len(data)-3; i++ {
        if data[i] == 0 && data[i+1] == 0 {
            if data[i+2] == 1 || (i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1) {
                if i > start {
                    nals = append(nals, data[start:i])
                }
                start = i
                if data[i+2] == 0 {
                    i += 3
                } else {
                    i += 2
                }
            }
        }
    }

    if start < len(data) {
        nals = append(nals, data[start:])
    }
    return nals
}
```

### Adding TURN Fallback

If UDP is blocked by the corporate firewall, add a TURN server:

**Option A: coturn on the same GCE VM**
```bash
sudo apt install coturn
# /etc/turnserver.conf
listening-port=3478
tls-listening-port=5349
realm=cloudproxy
# Use static auth or generate HMAC credentials from the server
```

**Option B: Managed TURN (Twilio)**
```bash
# Get TURN credentials via Twilio API
curl -X POST "https://api.twilio.com/2010-04-01/Accounts/ACXXX/Tokens.json" \
  -u "ACXXX:auth_token"
# Returns ICE servers with TURN credentials, pass to both clients
```

---

## 11. Future Enhancements (Phase 2)

- [ ] **Audio support**: Add audio track from Pi microphone
- [ ] **Data channel**: Send commands to Pi (e.g., PTZ controls, GPIO)
- [ ] **Multiple cameras**: Support multiple Pi publishers
- [ ] **Recording**: Save stream to disk on the server
- [ ] **TURN integration**: Add coturn or Twilio TURN for restricted networks
- [ ] **TLS/nginx**: Put server behind nginx with Let's Encrypt for WSS
- [ ] **Authentication**: Replace shared token with JWT or OAuth
- [ ] **Adaptive bitrate**: Server-side bandwidth estimation, request keyframes
- [ ] **Mobile viewer**: iOS/Android WebRTC viewer
- [ ] **Cloud Run signaling**: Move signaling to Cloud Run, keep only media on GCE VM

---

## 12. Implementation Status

### Current Phase: Initial Build

| Component | Status | Notes |
|-----------|--------|-------|
| Server (`server/main.go`) | ✅ Done | 618 lines Go, Pion SFU + WebSocket signaling |
| Pi Client (`pi-client/main.go`) | ✅ Done | 485 lines Go, Pion + ffmpeg + NAL parser |
| Viewer (`viewer/`) | ✅ Done | Vite + TypeScript, dark glassmorphism UI, stats overlay |
| ARCHITECTURE.md | ✅ Done | This document |
| `go mod tidy` (server) | ⏳ Pending | Needs Go 1.22+ installed (`brew install go`) |
| `go mod tidy` (pi-client) | ⏳ Pending | Needs Go 1.22+ installed |
| Cross-compile pi-client | ⏳ Pending | `GOOS=linux GOARCH=arm64 go build` then scp to Pi |
| GCE VM deployment | ⏳ Pending | Need gcloud CLI or manual setup |
| End-to-end test | ⏳ Pending | After all components deployed |

### File Structure

```
cloudproxy/
├── server/
│   ├── main.go              # SFU server (~300 lines Go)
│   ├── go.mod
│   ├── go.sum
│   └── Dockerfile
├── pi-client/
│   ├── main.go              # Pi camera client (~200 lines Go)
│   ├── go.mod
│   └── go.sum
├── viewer/
│   ├── package.json
│   ├── tsconfig.json
│   ├── vite.config.ts
│   ├── index.html
│   └── src/
│       ├── main.ts           # WebRTC viewer logic (~250 lines TS)
│       └── style.css         # Dark theme styles (~300 lines CSS)
├── ARCHITECTURE.md           # ← This file
└── README.md                 # Quick start guide
```
