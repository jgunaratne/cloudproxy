# CloudProxy

Stream live video from a Raspberry Pi 5 webcam through a Cloud Run relay server to a browser.

```
Pi (guest WiFi)                Cloud Run                    Mac (browser)
┌──────────┐    WebSocket     ┌──────────┐    WebSocket    ┌──────────┐
│ USB cam  │───────────────→  │  Relay   │ ──────────────→ │ Viewer   │
│ + ffmpeg │   H264 binary    │  Server  │   H264 binary   │ WebCodecs│
│ + Go     │                  │  (Go)    │                  │ + canvas │
└──────────┘                  └──────────┘                  └──────────┘
```

## Prerequisites

| Device | Requirements |
|--------|-------------|
| **Mac** | Go, Node.js, npm, `gcloud` CLI (authenticated) |
| **Pi** | `ffmpeg`, USB webcam at `/dev/video0`, WiFi with internet access |

## Authentication & Tokens

There are **two separate auth layers** to configure:

### 1. App-level token (`AUTH_TOKEN`)

A shared secret that the server, Pi client, and viewer all use to authenticate WebSocket connections. **All three must use the same value.**

| Component | How to set |
|-----------|-----------|
| **Server** (local) | `AUTH_TOKEN=my-token go run .` |
| **Server** (Cloud Run) | `--set-env-vars AUTH_TOKEN=my-token` in `gcloud run deploy` |
| **Pi client** | `AUTH_TOKEN=my-token ~/cloudproxy-pi-client` (or in the systemd unit) |
| **Viewer** | Enter in the **Token** field in the UI before clicking Connect |

> The default dev token is `cloudproxy-dev-token` (used when `AUTH_TOKEN` is unset). For production, set a strong shared secret.

### 2. Cloud Run IAM token (production only)

The Cloud Run service requires a **Google identity token** for IAM authentication (org policy blocks unauthenticated access). This is separate from the app-level token above.

- **Viewer (Mac)**: The Vite dev server automatically fetches a token via `gcloud auth print-identity-token` and injects it into the proxied WebSocket connection. Just make sure you've run `gcloud auth login` first.
- **Pi client**: Use one of these options:
  - `GCP_IDENTITY_URL` — set to the Cloud Run service URL and the Pi will fetch a token from the GCE metadata server (works on GCE/GKE/Cloud Run)
  - `GCP_IDENTITY_TOKEN` — set to a pre-fetched token (e.g. from `gcloud auth print-identity-token --audiences=SERVICE_URL`)

> For **local development** (server running on your Mac), neither component needs an identity token — only `AUTH_TOKEN` is required.

## Quick Start (Cloud Run — already deployed)

### 1. Start the Viewer (Mac)

```bash
# One-time: ensure gcloud is authenticated
gcloud auth login

cd viewer
npm install    # first time only
cp .env.example .env  # fill in CLOUD_RUN_URL and VITE_AUTH_TOKEN
npm run dev
```

Opens at **http://localhost:3000**. The Vite dev server proxies WebSocket connections to Cloud Run and injects your Google identity token automatically.

### 2. Start the Pi Client (pidesk)

**First time — cross-compile on Mac and copy to Pi:**

```bash
cd pi-client
go mod tidy
GOOS=linux GOARCH=arm64 go build -o cloudproxy-pi-client .
scp cloudproxy-pi-client pidesk.local:~/
```

**Run on the Pi:**

```bash
ssh pidesk.local

# Fetch a GCP identity token (run this on your Mac, paste into the Pi shell).
# Tokens expire after ~1 hour — re-run this if the connection drops with a 401.
GCP_TOKEN=$(gcloud auth print-identity-token \
  --audiences=https://cloudproxy-server-530731599092.us-west1.run.app)

SIGNAL_URL=wss://cloudproxy-server-530731599092.us-west1.run.app/ws \
AUTH_TOKEN=your-secret-token \
GCP_IDENTITY_TOKEN=$GCP_TOKEN \
~/cloudproxy-pi-client
```

> **Tip:** For always-on use, set `GCP_IDENTITY_URL` instead so the Pi fetches tokens automatically via the GCE metadata server (requires a service account). See [Authentication & Tokens](#authentication--tokens).

### 3. Watch the Stream

1. Open **http://localhost:3000** in **Chrome or Edge** (Firefox/Safari don't support WebCodecs)
2. Click **Connect** (default URL and token are pre-filled)
3. Live video should appear with stats overlay

## Local Development (no Cloud Run)

Run all three components locally for testing.

### Terminal 1 — Server (Mac)

```bash
cd server
go mod tidy    # first time only
go run .
# Listening on :8080
```

### Terminal 2 — Pi Client (pidesk via SSH)

```bash
ssh pidesk.local

SIGNAL_URL=ws://<YOUR_MAC_IP>:8080/ws \
AUTH_TOKEN=cloudproxy-dev-token \
~/cloudproxy-pi-client
```

Replace `<YOUR_MAC_IP>` with your Mac's IP reachable from the Pi's network.

### Terminal 3 — Viewer (Mac)

```bash
cd viewer
npm run dev
```

Open **http://localhost:3000**, change the server URL to `ws://localhost:8080/ws`, set token to `cloudproxy-dev-token`, and click Connect.

## Pi Client Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `SIGNAL_URL` | `ws://localhost:8080/ws` | Server WebSocket URL |
| `AUTH_TOKEN` | `cloudproxy-dev-token` | Shared auth token (must match server) |
| `VIDEO_DEVICE` | `/dev/video0` | Camera device path |
| `VIDEO_WIDTH` | `1280` | Capture width |
| `VIDEO_HEIGHT` | `720` | Capture height |
| `VIDEO_FPS` | `30` | Framerate |
| `VIDEO_BITRATE` | `2500k` | H264 encoding bitrate |

## Auto-Start Pi Client on Boot

Create a systemd service on the Pi:

```bash
sudo nano /etc/systemd/system/cloudproxy-pi.service
```

```ini
[Unit]
Description=CloudProxy Pi Camera Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=SIGNAL_URL=wss://cloudproxy-server-530731599092.us-west1.run.app/ws
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

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable cloudproxy-pi.service
sudo systemctl start cloudproxy-pi.service

# Check status / logs
sudo systemctl status cloudproxy-pi.service
journalctl -u cloudproxy-pi.service -f
```

## Pi WiFi Setup

If the Pi isn't connected to WiFi:

```bash
# List available networks
sudo nmcli device wifi list

# Connect (auto-saved for boot)
sudo nmcli device wifi connect "NETWORK_NAME" password "WIFI_PASSWORD"

# Verify
ping -c 2 google.com
```

## Rebuilding After Code Changes

### Server

```bash
cd server
# Local:
go run .
# Cloud Run:
gcloud builds submit --tag gcr.io/hansel-487018/cloudproxy-server
gcloud run deploy cloudproxy-server \
  --image gcr.io/hansel-487018/cloudproxy-server \
  --region us-west1 --port 8080 \
  --set-env-vars AUTH_TOKEN=your-secret-token \
  --min-instances 1 --max-instances 1 \
  --session-affinity --timeout 3600
```

### Pi Client

```bash
cd pi-client
GOOS=linux GOARCH=arm64 go build -o cloudproxy-pi-client .
scp cloudproxy-pi-client pidesk.local:~/
# Restart on Pi:
ssh pidesk.local "sudo systemctl restart cloudproxy-pi.service"
```

## Cloud Run Details

| Setting | Value |
|---------|-------|
| Service URL | `https://cloudproxy-server-530731599092.us-west1.run.app` |
| GCP Project | `hansel-487018` |
| Region | `us-west1` |
| Auth | Google identity token (org policy blocks `allUsers`) |

## Troubleshooting

| Problem | Fix |
|---------|-----|
| No video in browser | Use **Chrome or Edge** — Firefox/Safari don't support WebCodecs |
| Camera offline in viewer | Pi client isn't connected — check it's running and has internet |
| DNS errors on Pi | WiFi may be down — run `sudo nmcli device wifi list` and reconnect |
| `gcloud auth` errors in Vite | Run `gcloud auth login` on your Mac |
| ffmpeg "device busy" | Run `sudo fuser /dev/video0` on the Pi to find/kill the process |
| WebSocket 401 | `AUTH_TOKEN` mismatch between client and server |
| High latency (>500ms) | Reduce `VIDEO_BITRATE` or `VIDEO_FPS` on the Pi |

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full technical reference.
