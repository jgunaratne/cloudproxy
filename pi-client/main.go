package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// Config holds all configuration for the pi-client.
type Config struct {
	SignalURL    string
	AuthToken    string
	VideoDevice  string
	VideoWidth   string
	VideoHeight  string
	VideoFPS     string
	VideoBitrate string
}

// SignalMessage represents a JSON message exchanged over the signaling WebSocket.
type SignalMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	SDP       string `json:"sdp,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Message   string `json:"message,omitempty"`
}

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
	flag.Parse()

	return cfg
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// runSession executes a single signaling + WebRTC session.
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

	wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
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

	// Open a local UDP listener for ffmpeg's RTP output.
	rtpListener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listening for RTP: %w", err)
	}
	defer rtpListener.Close()
	rtpPort := rtpListener.LocalAddr().(*net.UDPAddr).Port
	log.Printf("RTP listener on 127.0.0.1:%d", rtpPort)

	// Start ffmpeg subprocess — outputs RTP to our local UDP listener.
	ffmpegCmd, err := startFFmpeg(ctx, cfg, rtpPort)
	if err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	defer func() {
		if ffmpegCmd.Process != nil {
			_ = ffmpegCmd.Process.Kill()
			_ = ffmpegCmd.Wait()
		}
	}()

	log.Printf("ffmpeg started, pid=%d", ffmpegCmd.Process.Pid)

	// Create WebRTC PeerConnection.
	pcConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(pcConfig)
	if err != nil {
		return fmt.Errorf("creating peer connection: %w", err)
	}
	defer pc.Close()

	// Create H264 video track (RTP-based — we write raw RTP packets).
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		"video",     // track ID
		"pi-camera", // stream ID
	)
	if err != nil {
		return fmt.Errorf("creating video track: %w", err)
	}

	rtpSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("adding track: %w", err)
	}

	// Read and discard RTCP packets (required to keep the sender alive).
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(buf); err != nil {
				return
			}
		}
	}()

	// Monitor PeerConnection state.
	pcFailed := make(chan struct{}, 1)

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("peer connection state: %s", state.String())
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateClosed:
			select {
			case pcFailed <- struct{}{}:
			default:
			}
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE connection state: %s", state.String())
	})

	// Send local ICE candidates to the signaling server.
	var wsMu sync.Mutex
	sendJSON := func(msg SignalMessage) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		return wsConn.WriteJSON(msg)
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidateJSON, err := json.Marshal(c.ToJSON())
		if err != nil {
			log.Printf("error marshaling ICE candidate: %v", err)
			return
		}
		log.Printf("sending ICE candidate")
		if err := sendJSON(SignalMessage{
			Type:      "candidate",
			Candidate: string(candidateJSON),
		}); err != nil {
			log.Printf("error sending ICE candidate: %v", err)
		}
	})

	// Create offer.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("creating offer: %w", err)
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("setting local description: %w", err)
	}

	log.Printf("sending offer")
	if err := sendJSON(SignalMessage{
		Type: "offer",
		SDP:  offer.SDP,
	}); err != nil {
		return fmt.Errorf("sending offer: %w", err)
	}

	// Start reading RTP packets from ffmpeg's UDP output and writing to the track.
	rtpDone := make(chan error, 1)
	go func() {
		rtpDone <- forwardRTP(rtpListener, videoTrack)
	}()

	// Read signaling messages.
	wsDone := make(chan error, 1)
	go func() {
		wsDone <- readSignalingMessages(wsConn, pc)
	}()

	// Wait for session end.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-wsDone:
		return fmt.Errorf("websocket closed: %w", err)
	case err := <-rtpDone:
		return fmt.Errorf("rtp forwarder stopped: %w", err)
	case <-pcFailed:
		return fmt.Errorf("peer connection failed")
	}
}

// readSignalingMessages reads messages from the WebSocket and handles answers
// and ICE candidates from the server.
func readSignalingMessages(wsConn *websocket.Conn, pc *webrtc.PeerConnection) error {
	for {
		var msg SignalMessage
		if err := wsConn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("reading message: %w", err)
		}

		switch msg.Type {
		case "answer":
			log.Printf("received answer")
			answer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  msg.SDP,
			}
			if err := pc.SetRemoteDescription(answer); err != nil {
				log.Printf("error setting remote description: %v", err)
			}

		case "candidate":
			log.Printf("received ICE candidate")
			var candidate webrtc.ICECandidateInit
			if err := json.Unmarshal([]byte(msg.Candidate), &candidate); err != nil {
				log.Printf("error unmarshaling ICE candidate: %v", err)
				continue
			}
			if err := pc.AddICECandidate(candidate); err != nil {
				log.Printf("error adding ICE candidate: %v", err)
			}

		case "error":
			log.Printf("server error: %s", msg.Message)

		default:
			log.Printf("unknown message type: %s", msg.Type)
		}
	}
}

// startFFmpeg starts the ffmpeg subprocess that captures H264 video from the
// webcam and outputs RTP to a local UDP port.
func startFFmpeg(ctx context.Context, cfg Config, rtpPort int) (*exec.Cmd, error) {
	videoSize := cfg.VideoWidth + "x" + cfg.VideoHeight
	rtpDest := fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", rtpPort)

	// Keyframe interval = 2 seconds worth of frames
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
		"-an",
		"-f", "rtp",
		rtpDest,
	}

	log.Printf("ffmpeg command: ffmpeg %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr // Show ffmpeg logs.

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}

	return cmd, nil
}

// forwardRTP reads RTP packets from the UDP listener (ffmpeg output)
// and writes them directly to the WebRTC track. ffmpeg handles all
// H264 RTP packetization (FU-A, STAP-A, etc.) correctly.
func forwardRTP(listener net.PacketConn, track *webrtc.TrackLocalStaticRTP) error {
	buf := make([]byte, 4096)
	packetCount := 0

	for {
		n, _, err := listener.ReadFrom(buf)
		if err != nil {
			return fmt.Errorf("reading RTP: %w", err)
		}

		// RTP packets start with version 2 (first byte top 2 bits = 10)
		if n < 12 || (buf[0]>>6) != 2 {
			continue // skip non-RTP packets (RTCP, etc.)
		}

		if _, err := track.Write(buf[:n]); err != nil {
			return fmt.Errorf("writing RTP: %w", err)
		}

		packetCount++
		if packetCount%150 == 0 { // ~10 seconds at 15fps
			log.Printf("forwarded %d RTP packets", packetCount)
		}
	}
}
