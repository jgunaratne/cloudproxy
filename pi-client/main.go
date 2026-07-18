package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Config holds all configuration for the pi-client.
type Config struct {
	SignalURL        string
	AuthToken        string
	VideoDevice      string
	VideoWidth       string
	VideoHeight      string
	VideoFPS         string
	VideoBitrate     string
	GCPIdentityURL   string // Cloud Run service URL for identity token audience (optional, uses metadata server)
	GCPIdentityToken string // Pre-fetched identity token (optional, bypasses metadata server)
	GCPTokenFile     string // Path to a file holding the identity token, re-read on every reconnect (optional)
}

// SignalMessage represents a JSON message exchanged over the signaling WebSocket.
type SignalMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
}

// NAL unit types relevant for H264 parsing.
const (
	nalTypeNonIDR = 1
	nalTypeSPS    = 7
	nalTypePPS    = 8
	nalTypeIDR    = 5
)

// Binary message header flags.
const (
	flagKeyframe = 0x01 // Access unit contains IDR NAL
	flagCodecInit = 0x02 // Access unit contains SPS/PPS
)

// headerSize is the size of the binary message header (1 byte flags + 4 bytes timestamp).
const headerSize = 5

func main() {
	cfg := parseConfig()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	log.Printf("pi-client starting")
	log.Printf("  signal_url=%s", cfg.SignalURL)
	log.Printf("  video_device=%s", cfg.VideoDevice)
	log.Printf("  video=%sx%s@%sfps", cfg.VideoWidth, cfg.VideoHeight, cfg.VideoFPS)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received signal %v, shutting down", sig)
		cancel()
	}()

	// Main reconnection loop.
	for {
		if ctx.Err() != nil {
			log.Printf("context cancelled, exiting")
			return
		}

		err := runSession(ctx, cfg)
		if ctx.Err() != nil {
			log.Printf("session ended due to shutdown")
			return
		}
		log.Printf("session ended: %v", err)
		log.Printf("reconnecting in 3 seconds...")

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// parseConfig reads configuration from flags and environment variables.
// Flags take precedence over env vars.
func parseConfig() Config {
	cfg := Config{}

	flag.StringVar(&cfg.SignalURL, "signal-url", envOrDefault("SIGNAL_URL", "ws://localhost:8080/ws"), "WebSocket URL of the signaling server")
	flag.StringVar(&cfg.AuthToken, "auth-token", envOrDefault("AUTH_TOKEN", "cloudproxy-dev-token"), "Authentication token")
	flag.StringVar(&cfg.VideoDevice, "video-device", envOrDefault("VIDEO_DEVICE", "/dev/video0"), "Video device path")
	flag.StringVar(&cfg.VideoWidth, "video-width", envOrDefault("VIDEO_WIDTH", "1280"), "Video width")
	flag.StringVar(&cfg.VideoHeight, "video-height", envOrDefault("VIDEO_HEIGHT", "720"), "Video height")
	flag.StringVar(&cfg.VideoFPS, "video-fps", envOrDefault("VIDEO_FPS", "30"), "Video framerate")
	flag.StringVar(&cfg.VideoBitrate, "video-bitrate", envOrDefault("VIDEO_BITRATE", "2500k"), "H264 encoding bitrate")
	flag.StringVar(&cfg.GCPIdentityURL, "gcp-identity-url", envOrDefault("GCP_IDENTITY_URL", ""), "Cloud Run service URL for identity token audience (e.g. https://cloudproxy-server-xxx.run.app)")
	flag.StringVar(&cfg.GCPIdentityToken, "gcp-identity-token", envOrDefault("GCP_IDENTITY_TOKEN", ""), "Pre-fetched Google identity token (bypasses metadata server)")
	flag.StringVar(&cfg.GCPTokenFile, "gcp-identity-token-file", envOrDefault("GCP_IDENTITY_TOKEN_FILE", ""), "File containing a Google identity token, re-read on every reconnect (e.g. pushed by scripts/push-token-to-pi.sh)")
	flag.Parse()

	return cfg
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// fetchGCPIdentityToken fetches a Google Cloud identity token from the
// GCE metadata server. The audience should be the Cloud Run service URL.
// This works when running on GCE, Cloud Run, GKE, or any environment
// with a service account and metadata server.
func fetchGCPIdentityToken(audience string) (string, error) {
	metadataURL := fmt.Sprintf(
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s",
		url.QueryEscape(audience),
	)

	req, err := http.NewRequest("GET", metadataURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("metadata returned %d: %s", resp.StatusCode, string(body))
	}

	token, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading token: %w", err)
	}

	return strings.TrimSpace(string(token)), nil
}

// runSession executes a single signaling + streaming session.
// It returns when the session ends (error or disconnect).
func runSession(ctx context.Context, cfg Config) error {
	// Build the signaling URL with query parameters.
	u, err := url.Parse(cfg.SignalURL)
	if err != nil {
		return fmt.Errorf("invalid signal URL: %w", err)
	}
	q := u.Query()
	q.Set("role", "publisher")
	q.Set("token", cfg.AuthToken)
	u.RawQuery = q.Encode()

	log.Printf("connecting to signaling server: %s", u.String())

	// Build HTTP headers for WebSocket dial.
	headers := http.Header{}

	// Authenticate to Cloud Run if identity token or identity URL is configured.
	if cfg.GCPTokenFile != "" {
		// Re-read on every connection attempt so an external refresher
		// (scripts/push-token-to-pi.sh) can rotate the ~1h IAP token
		// without restarting this process: an expired token just drops
		// the session, and the reconnect picks up the fresh file.
		data, err := os.ReadFile(cfg.GCPTokenFile)
		if err != nil {
			log.Printf("warning: reading identity token file: %v (continuing without it)", err)
		} else {
			headers.Set("Authorization", "Bearer "+strings.TrimSpace(string(data)))
			log.Printf("using GCP identity token from %s for Cloud Run auth", cfg.GCPTokenFile)
		}
	} else if cfg.GCPIdentityToken != "" {
		// Use pre-fetched token directly (e.g. from `gcloud auth print-identity-token`).
		headers.Set("Authorization", "Bearer "+cfg.GCPIdentityToken)
		log.Printf("using pre-fetched GCP identity token for Cloud Run auth")
	} else if cfg.GCPIdentityURL != "" {
		// Fetch from GCE metadata server (works on GCE, Cloud Run, GKE).
		token, err := fetchGCPIdentityToken(cfg.GCPIdentityURL)
		if err != nil {
			log.Printf("warning: failed to fetch GCP identity token: %v (continuing without it)", err)
		} else {
			headers.Set("Authorization", "Bearer "+token)
			log.Printf("using GCP identity token from metadata server for Cloud Run auth")
		}
	}

	wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), headers)
	if err != nil {
		return fmt.Errorf("websocket dial: %w", err)
	}
	defer wsConn.Close()

	log.Printf("websocket connected")

	// Wait for welcome message.
	var welcome SignalMessage
	if err := wsConn.ReadJSON(&welcome); err != nil {
		return fmt.Errorf("reading welcome: %w", err)
	}
	if welcome.Type != "welcome" {
		return fmt.Errorf("expected welcome, got %q", welcome.Type)
	}
	log.Printf("received welcome, id=%s", welcome.ID)

	// Start ffmpeg subprocess — outputs H264 Annex B to stdout.
	ffmpegCmd, stdout, err := startFFmpeg(ctx, cfg)
	if err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	defer killFFmpeg(ffmpegCmd)

	log.Printf("ffmpeg started, pid=%d", ffmpegCmd.Process.Pid)

	// Read from WebSocket in background to detect disconnect.
	wsDone := make(chan error, 1)
	go func() {
		wsDone <- readWebSocket(wsConn)
	}()

	// Stream H264 frames over the WebSocket.
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- streamH264(ctx, stdout, wsConn)
	}()

	// Wait for session end.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-wsDone:
		return fmt.Errorf("websocket closed: %w", err)
	case err := <-streamDone:
		return fmt.Errorf("stream stopped: %w", err)
	}
}

// readWebSocket reads messages from the WebSocket to detect disconnection.
// The server won't send much after welcome; this mainly detects close.
func readWebSocket(wsConn *websocket.Conn) error {
	for {
		_, raw, err := wsConn.ReadMessage()
		if err != nil {
			return fmt.Errorf("reading message: %w", err)
		}

		var msg SignalMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("ignoring non-JSON message (%d bytes)", len(raw))
			continue
		}

		switch msg.Type {
		case "error":
			log.Printf("server error: %s", msg.Message)
		default:
			log.Printf("received message type=%s", msg.Type)
		}
	}
}

// startFFmpeg starts the ffmpeg subprocess that captures H264 video from the
// webcam and outputs Annex B H264 to stdout (pipe).
func startFFmpeg(ctx context.Context, cfg Config) (*exec.Cmd, io.ReadCloser, error) {
	videoSize := cfg.VideoWidth + "x" + cfg.VideoHeight

	// Keyframe interval = 2 seconds worth of frames.
	fps := 30
	fmt.Sscanf(cfg.VideoFPS, "%d", &fps)
	gopSize := fmt.Sprintf("%d", fps*2)
	bufSize := cfg.VideoBitrate[:len(cfg.VideoBitrate)-1] // strip 'k' suffix
	bufSizeInt := 0
	fmt.Sscanf(bufSize, "%d", &bufSizeInt)
	vbvBuf := fmt.Sprintf("%dk", bufSizeInt*2)

	args := []string{
		"-f", "v4l2",
		"-input_format", "mjpeg",
		"-video_size", videoSize,
		"-framerate", cfg.VideoFPS,
		"-i", cfg.VideoDevice,
		"-pix_fmt", "yuv420p",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-profile:v", "baseline",
		"-level", "3.1",
		"-b:v", cfg.VideoBitrate,
		"-maxrate", cfg.VideoBitrate,
		"-bufsize", vbvBuf,
		"-g", gopSize,
		"-keyint_min", gopSize,
		"-forced-idr", "1",
		"-x264-params", "slices=1",
		"-an",
		"-f", "h264",
		"pipe:1",
	}

	log.Printf("ffmpeg command: ffmpeg %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr // Show ffmpeg logs.

	// Use process group so we can kill all child processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting ffmpeg: %w", err)
	}

	return cmd, stdout, nil
}

// killFFmpeg kills the ffmpeg process and its process group.
func killFFmpeg(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Kill the entire process group.
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

// accessUnit represents a complete video frame (one or more NAL units).
type accessUnit struct {
	nalus      [][]byte // Individual NAL units (without start codes).
	hasIDR     bool
	hasSPS     bool
	hasPPS     bool
	receivedAt time.Time
}

// annexBBytes returns the access unit encoded in Annex B format (with 4-byte start codes).
func (au *accessUnit) annexBBytes() []byte {
	// Pre-calculate total size.
	totalSize := 0
	for _, nalu := range au.nalus {
		totalSize += 4 + len(nalu) // 4-byte start code + NAL data
	}
	buf := make([]byte, 0, totalSize)
	for _, nalu := range au.nalus {
		buf = append(buf, 0x00, 0x00, 0x00, 0x01)
		buf = append(buf, nalu...)
	}
	return buf
}

// streamH264 reads H264 Annex B data from ffmpeg stdout, parses NAL units,
// groups them into access units, and sends each as a binary WebSocket message.
func streamH264(ctx context.Context, stdout io.Reader, wsConn *websocket.Conn) error {
	reader := bufio.NewReaderSize(stdout, 1024*1024) // 1MB buffer

	streamStart := time.Now()
	var frameCount uint64
	var totalBytes uint64
	var lastStatTime = time.Now()
	var wsMu sync.Mutex

	var currentAU *accessUnit

	sendAccessUnit := func(au *accessUnit) error {
		if au == nil || len(au.nalus) == 0 {
			return nil
		}

		// Construct flags byte.
		var flags byte
		if au.hasIDR {
			flags |= flagKeyframe
		}
		if au.hasSPS || au.hasPPS {
			flags |= flagCodecInit
		}

		// Calculate timestamp in ms since stream start.
		tsMs := uint32(au.receivedAt.Sub(streamStart).Milliseconds())

		// Build the binary message: [flags(1)][timestamp(4)][annexB data].
		annexB := au.annexBBytes()
		msg := make([]byte, headerSize+len(annexB))
		msg[0] = flags
		binary.BigEndian.PutUint32(msg[1:5], tsMs)
		copy(msg[headerSize:], annexB)

		// Send as binary WebSocket message.
		wsMu.Lock()
		wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := wsConn.WriteMessage(websocket.BinaryMessage, msg)
		wsMu.Unlock()
		if err != nil {
			return fmt.Errorf("writing websocket: %w", err)
		}

		// Update stats.
		count := atomic.AddUint64(&frameCount, 1)
		atomic.AddUint64(&totalBytes, uint64(len(msg)))

		// Log stats every 5 seconds.
		now := time.Now()
		if now.Sub(lastStatTime) >= 5*time.Second {
			elapsed := now.Sub(lastStatTime).Seconds()
			fps := float64(count) / now.Sub(streamStart).Seconds()
			bytesSent := atomic.LoadUint64(&totalBytes)
			kbps := float64(bytesSent) * 8.0 / now.Sub(streamStart).Seconds() / 1000.0
			log.Printf("stream stats: %d frames sent, %.1f fps, %.0f kbps, ts=%dms",
				count, fps, kbps, tsMs)
			lastStatTime = now
			_ = elapsed
		}

		return nil
	}

	// Read NAL units from the H264 Annex B stream.
	// We scan for start codes (0x00 0x00 0x00 0x01 or 0x00 0x00 0x01).
	nalCh := make(chan []byte, 64)
	nalErr := make(chan error, 1)

	go func() {
		defer close(nalCh)
		err := parseNALUnits(reader, nalCh)
		if err != nil {
			nalErr <- err
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Send any remaining access unit.
			if currentAU != nil {
				_ = sendAccessUnit(currentAU)
			}
			return ctx.Err()

		case nalu, ok := <-nalCh:
			if !ok {
				// Channel closed — send remaining access unit.
				if currentAU != nil {
					if err := sendAccessUnit(currentAU); err != nil {
						return err
					}
				}
				select {
				case err := <-nalErr:
					return err
				default:
					return fmt.Errorf("ffmpeg stdout closed")
				}
			}

			if len(nalu) == 0 {
				continue
			}

			nalType := nalu[0] & 0x1F

			// Determine if this NAL starts a new access unit (new picture).
			// VCL NALs (type 1 = non-IDR slice, type 5 = IDR slice) are video data.
			// A VCL NAL with first_mb_in_slice == 0 indicates the first slice
			// of a new picture, which starts a new access unit. Subsequent
			// slices of the same picture have first_mb_in_slice > 0 and belong
			// to the current access unit.
			isVCL := nalType == nalTypeNonIDR || nalType == nalTypeIDR
			isNewPicture := false
			if isVCL && len(nalu) >= 2 {
				// first_mb_in_slice is the first syntax element in the slice
				// header, encoded as an exp-Golomb code right after the NAL
				// header byte. If the first bit after the NAL header is 1,
				// the value is 0 (= first slice of a new picture).
				isNewPicture = (nalu[1] & 0x80) != 0
			}

			if isNewPicture && currentAU != nil && hasVCL(currentAU) {
				// First slice of a new picture — send the previous access unit.
				if err := sendAccessUnit(currentAU); err != nil {
					return err
				}
				currentAU = nil
			}

			// Create new access unit if needed.
			if currentAU == nil {
				currentAU = &accessUnit{
					receivedAt: time.Now(),
				}
			}

			// Add NAL to current access unit.
			currentAU.nalus = append(currentAU.nalus, nalu)
			switch nalType {
			case nalTypeIDR:
				currentAU.hasIDR = true
			case nalTypeSPS:
				currentAU.hasSPS = true
			case nalTypePPS:
				currentAU.hasPPS = true
			}

		case err := <-nalErr:
			if currentAU != nil {
				_ = sendAccessUnit(currentAU)
			}
			return err
		}
	}
}

// hasVCL returns true if the access unit already contains a VCL NAL unit.
func hasVCL(au *accessUnit) bool {
	for _, nalu := range au.nalus {
		if len(nalu) == 0 {
			continue
		}
		t := nalu[0] & 0x1F
		if t == nalTypeNonIDR || t == nalTypeIDR {
			return true
		}
	}
	return false
}

// parseNALUnits reads an H264 Annex B byte stream and sends individual NAL units
// (without start codes) to the output channel. It handles both 3-byte (0x000001)
// and 4-byte (0x00000001) start codes, and correctly handles start codes that
// span buffer read boundaries.
func parseNALUnits(reader *bufio.Reader, out chan<- []byte) error {
	// Accumulate the current NAL unit data (without the start code).
	var nalBuf []byte

	// State machine for detecting start codes in the byte stream.
	// We track consecutive zero bytes to find start code patterns.
	zeroCount := 0
	inNAL := false

	for {
		b, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF {
				// Emit any remaining NAL data.
				if inNAL && len(nalBuf) > 0 {
					out <- nalBuf
				}
				return nil
			}
			return fmt.Errorf("reading h264 stream: %w", err)
		}

		if b == 0x00 {
			zeroCount++
			continue
		}

		if b == 0x01 && zeroCount >= 2 {
			// Found a start code (0x000001 or 0x00000001).
			// Emit the previous NAL unit if we have one.
			if inNAL && len(nalBuf) > 0 {
				out <- nalBuf
				nalBuf = nil
			}
			inNAL = true
			zeroCount = 0
			continue
		}

		// Not a start code sequence. If we had accumulated zeros, they
		// belong to the current NAL unit's data.
		if inNAL {
			for i := 0; i < zeroCount; i++ {
				nalBuf = append(nalBuf, 0x00)
			}
			nalBuf = append(nalBuf, b)
		}
		zeroCount = 0
	}
}
