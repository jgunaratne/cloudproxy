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

### 2. Google-level access via IAP (production only)

The Cloud Run service sits behind **Identity-Aware Proxy** (org policy blocks `allUsers`, so the service can't be made public). This is separate from the app-level token above.

- **Browser**: sign in with a Google account that has `roles/iap.httpsResourceAccessor` on the service. Grant with:
  ```bash
  gcloud beta iap web add-iam-policy-binding \
    --resource-type=cloud-run --service=cloudproxy-server --region=us-west1 \
    --member=user:YOUR_EMAIL --role=roles/iap.httpsResourceAccessor
  ```
- **Pi client (or any machine caller)**: IAP's Google-managed OAuth client **rejects ordinary identity tokens** (`gcloud auth print-identity-token` gets a 401 "Invalid JWT audience"). What works is a **service-account signed JWT** with audience `https://<service-url>/*`. Mint one with:
  ```bash
  GCP_IDENTITY_TOKEN=$(./scripts/mint-iap-token.sh)   # valid 1 hour
  ```
  This signs as `cloudproxy-pi@hansel-487018.iam.gserviceaccount.com`, which has `iap.httpsResourceAccessor`. Your user needs `roles/iam.serviceAccountTokenCreator` on that SA, plus a one-time `gcloud auth application-default login`.

  Since tokens expire after ~1 hour, don't paste them by hand — install the **automatic token pusher** (see [Automatic IAP token refresh](#automatic-iap-token-refresh-mac--pi)) so the Mac keeps the Pi supplied with fresh tokens.

> For **local development** (server running on your Mac), neither component needs any Google token — only `AUTH_TOKEN` is required.

## Quick Start (Cloud Run — already deployed)

### Option A: Open the Cloud Run URL directly

**https://cloudproxy-server-530731599092.us-west1.run.app/** serves the viewer UI directly — no local setup needed. IAP will prompt you to sign in with Google (your account needs `iap.httpsResourceAccessor`, see [Authentication & Tokens](#authentication--tokens)); then enter the app token and click Connect.

### Option B: Run the Viewer locally (Mac)

```bash
# One-time: ensure gcloud is authenticated
gcloud auth login

cd viewer
npm install    # first time only
cp .env.example .env  # fill in CLOUD_RUN_URL and VITE_AUTH_TOKEN
npm run dev
```

Opens at **http://localhost:3000**. The Vite dev server proxies WebSocket connections to Cloud Run and injects your Google identity token automatically.

### Start the Pi Client (pidesk)

(Required either way — this is what actually publishes video.)

**First time — cross-compile on Mac and copy to Pi:**

```bash
cd pi-client
go mod tidy
GOOS=linux GOARCH=arm64 go build -o cloudproxy-pi-client .
scp cloudproxy-pi-client pidesk.local:~/
```

**Set up automatic IAP token refresh (Mac → Pi):**

<a id="automatic-iap-token-refresh-mac--pi"></a>
IAP tokens expire after ~1 hour, and the org's IAM policy means the Pi can't mint its own (that requires either a downloadable service-account key or a metadata server, neither of which the Pi has). Instead, the Mac — which already has `gcloud` credentials — mints and pushes tokens on a schedule:

```bash
# One-time, on the Mac. Requires passwordless (key-based) SSH to the Pi.
./scripts/install-token-pusher.sh        # or: PI_HOST=mypi.local ./scripts/install-token-pusher.sh
```

This installs a launchd agent that runs `scripts/push-token-to-pi.sh` every 45 minutes: it mints a token via `mint-iap-token.sh` and writes it atomically to `~/.cloudproxy/iap-token` on the Pi. Logs go to `~/Library/Logs/cloudproxy-token-pusher.log`. To push once manually, just run `./scripts/push-token-to-pi.sh`.

**Run on the Pi:**

```bash
ssh pidesk.local

SIGNAL_URL=wss://cloudproxy-server-530731599092.us-west1.run.app/ws \
AUTH_TOKEN=your-secret-token \
GCP_IDENTITY_TOKEN_FILE=$HOME/.cloudproxy/iap-token \
~/cloudproxy-pi-client
```

The client re-reads the token file on every reconnect: when a token expires, Cloud Run drops the connection, and the automatic 3-second reconnect picks up the fresh token the Mac pushed in the meantime.

> **Caveats:** the Mac must be awake and on a network that can reach the Pi for the pusher to fire (launchd catches up after sleep). For a one-off session without the pusher, you can still mint manually with `GCP_IDENTITY_TOKEN=$(./scripts/mint-iap-token.sh)` and pass that instead. If the Pi ever runs inside GCP, set `GCP_IDENTITY_URL` to use the metadata server directly.

### Watch the Stream

1. Open **http://localhost:3000** (Option B) or the Cloud Run URL (Option A) in **Chrome or Edge** (Firefox/Safari don't support WebCodecs)
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
| `GCP_IDENTITY_TOKEN_FILE` | _(unset)_ | File holding an IAP token, re-read on every reconnect (kept fresh by `scripts/push-token-to-pi.sh`) |
| `GCP_IDENTITY_TOKEN` | _(unset)_ | A pre-fetched IAP token, used as-is (expires ~1 hour) |
| `GCP_IDENTITY_URL` | _(unset)_ | Cloud Run URL as token audience; fetches tokens from the GCE metadata server (GCP-hosted clients only) |

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
Environment=GCP_IDENTITY_TOKEN_FILE=/home/pi/.cloudproxy/iap-token
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
# Local (serves a placeholder page at "/"; only /ws matters for local dev):
cd server && go run .
```

The Dockerfile for Cloud Run is at the **repo root** (not `server/Dockerfile`) so it
can build the viewer UI and embed it into the Go binary via `go:embed`. Run
`gcloud builds submit` from the repo root:

```bash
cd /path/to/cloudproxy   # repo root, not server/

# Build and push image
gcloud builds submit --tag gcr.io/hansel-487018/cloudproxy-server

# Deploy to Cloud Run. Access control is handled by IAP (already enabled on
# the service via `gcloud beta run services update cloudproxy-server --iap`);
# org policy blocks --allow-unauthenticated, so don't bother with it.
gcloud run deploy cloudproxy-server \
  --image gcr.io/hansel-487018/cloudproxy-server \
  --region us-west1 --port 8080 \
  --set-env-vars AUTH_TOKEN=your-secret-token \
  --min-instances 1 --max-instances 1 \
  --session-affinity --timeout 3600
```

After deploying, the viewer UI is served at:
**https://cloudproxy-server-530731599092.us-west1.run.app/**

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
| Auth | IAP (Google sign-in for browsers; signed SA JWT via `scripts/mint-iap-token.sh` for machines) + app-level `AUTH_TOKEN` |
| IAP client ID | `369001918367-t5qrahnqdaasaifvk6akpqkpjk9vli58.apps.googleusercontent.com` |
| Pi service account | `cloudproxy-pi@hansel-487018.iam.gserviceaccount.com` |

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
