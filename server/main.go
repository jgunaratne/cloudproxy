package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// ---------------------------------------------------------------------------
// Signaling message types
// ---------------------------------------------------------------------------

type SignalMessage struct {
	Type      string `json:"type"`
	SDP       string `json:"sdp,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	ID        string `json:"id,omitempty"`
	Message   string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Viewer state
// ---------------------------------------------------------------------------

type Viewer struct {
	ID   string
	Conn *websocket.Conn
	PC   *webrtc.PeerConnection
	mu   sync.Mutex // protects websocket writes
}

func (v *Viewer) SendJSON(msg SignalMessage) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Conn.WriteJSON(msg)
}

// ---------------------------------------------------------------------------
// Server (shared state)
// ---------------------------------------------------------------------------

type Server struct {
	mu sync.RWMutex

	// Publisher state
	publisherConn *websocket.Conn
	publisherPC   *webrtc.PeerConnection
	publisherRecv *webrtc.RTPReceiver // stored to send PLI/FIR requests
	localTrack    *webrtc.TrackLocalStaticRTP

	// Viewers
	viewers map[string]*Viewer

	// WebRTC API (shared media engine with H264)
	api *webrtc.API

	// ICE servers
	iceServers []webrtc.ICEServer

	// Auth
	authToken string
}

func NewServer() *Server {
	// ---------- Media Engine with H264 ----------
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			Channels:    0,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		log.Fatalf("Failed to register H264 codec: %v", err)
	}

	// ---------- Interceptor Registry (for RTCP feedback) ----------
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		log.Fatalf("Failed to register interceptors: %v", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	// ---------- ICE Servers ----------
	iceServers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
	if turnEnv := os.Getenv("TURN_SERVERS"); turnEnv != "" {
		// Format: turn:host:port?transport=udp,username,credential
		parts := strings.SplitN(turnEnv, ",", 3)
		if len(parts) == 3 {
			iceServers = append(iceServers, webrtc.ICEServer{
				URLs:           []string{parts[0]},
				Username:       parts[1],
				Credential:     parts[2],
				CredentialType: webrtc.ICECredentialTypePassword,
			})
			log.Printf("Added TURN server: %s", parts[0])
		}
	}

	// ---------- Auth token ----------
	token := os.Getenv("AUTH_TOKEN")
	if token == "" {
		token = "cloudproxy-dev-token"
	}

	return &Server{
		viewers:    make(map[string]*Viewer),
		api:        api,
		iceServers: iceServers,
		authToken:  token,
	}
}

// ---------------------------------------------------------------------------
// WebSocket upgrader (allow all origins)
// ---------------------------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "OK")
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// ---------- Query params ----------
	role := r.URL.Query().Get("role")
	token := r.URL.Query().Get("token")

	if role != "publisher" && role != "viewer" {
		http.Error(w, "role must be publisher or viewer", http.StatusBadRequest)
		return
	}
	if token != s.authToken {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// ---------- Upgrade ----------
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	id := uuid.New().String()

	// Send welcome
	if err := conn.WriteJSON(SignalMessage{Type: "welcome", ID: id}); err != nil {
		log.Printf("Failed to send welcome: %v", err)
		conn.Close()
		return
	}

	if role == "publisher" {
		s.handlePublisher(conn, id)
	} else {
		s.handleViewer(conn, id)
	}
}

// ---------------------------------------------------------------------------
// Publisher handling
// ---------------------------------------------------------------------------

func (s *Server) handlePublisher(conn *websocket.Conn, id string) {
	log.Printf("Publisher connected: %s", id)

	s.mu.Lock()
	if s.publisherConn != nil {
		log.Printf("Replacing existing publisher connection")
		// Close old publisher connection — new one takes over
		oldConn := s.publisherConn
		oldConn.Close()
		if s.publisherPC != nil {
			s.publisherPC.Close()
			s.publisherPC = nil
		}
		s.localTrack = nil
		s.publisherRecv = nil
	}
	s.publisherConn = conn
	s.mu.Unlock()

	defer func() {
		log.Printf("Publisher disconnected: %s", id)
		s.cleanupPublisher()
		conn.Close()
	}()

	// Read signaling messages
	for {
		var msg SignalMessage
		if err := conn.ReadJSON(&msg); err != nil {
			log.Printf("Publisher read error: %v", err)
			return
		}

		switch msg.Type {
		case "offer":
			s.handlePublisherOffer(conn, msg)
		case "candidate":
			s.handlePublisherCandidate(msg)
		default:
			log.Printf("Publisher sent unknown message type: %s", msg.Type)
		}
	}
}

func (s *Server) handlePublisherOffer(conn *websocket.Conn, msg SignalMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create PeerConnection for publisher
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: s.iceServers,
	})
	if err != nil {
		log.Printf("Failed to create publisher PeerConnection: %v", err)
		conn.WriteJSON(SignalMessage{Type: "error", Message: "failed to create peer connection"})
		return
	}

	// Clean up old PC if any
	if s.publisherPC != nil {
		s.publisherPC.Close()
	}
	s.publisherPC = pc

	// ICE candidate handler
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidateJSON, err := json.Marshal(c.ToJSON())
		if err != nil {
			log.Printf("Failed to marshal ICE candidate: %v", err)
			return
		}
		s.mu.RLock()
		pubConn := s.publisherConn
		s.mu.RUnlock()
		if pubConn != nil {
			pubConn.WriteJSON(SignalMessage{
				Type:      "candidate",
				Candidate: string(candidateJSON),
			})
		}
	})

	// Connection state handler
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Publisher PeerConnection state: %s", state.String())
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			log.Printf("Publisher PeerConnection %s, cleaning up", state.String())
			go s.cleanupPublisher()
		}
	})

	// OnTrack handler — receives the video track from the Pi
	pc.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Publisher track received: codec=%s, id=%s", remoteTrack.Codec().MimeType, remoteTrack.ID())

		// Create local track that viewers will subscribe to
		localTrack, err := webrtc.NewTrackLocalStaticRTP(
			remoteTrack.Codec().RTPCodecCapability,
			"video", "cloudproxy",
		)
		if err != nil {
			log.Printf("Failed to create local track: %v", err)
			return
		}

		s.mu.Lock()
		s.localTrack = localTrack
		s.publisherRecv = receiver
		s.mu.Unlock()

		// Notify all viewers
		s.notifyViewers(SignalMessage{Type: "publisher_online"})

		// Negotiate with waiting viewers
		s.negotiateWaitingViewers()

		// Periodically send PLI to publisher to request keyframes.
		// This ensures new viewers get a clean IDR frame quickly.
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				s.mu.RLock()
				currentPC := s.publisherPC
				s.mu.RUnlock()
				if currentPC == nil || currentPC != pc {
					return
				}
				if err := pc.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{
						MediaSSRC: uint32(remoteTrack.SSRC()),
					},
				}); err != nil {
					log.Printf("Failed to send PLI: %v", err)
					return
				}
			}
		}()

		// Forward RTP packets from remote to local track
		buf := make([]byte, 4096)
		for {
			n, _, err := remoteTrack.Read(buf)
			if err != nil {
				log.Printf("Publisher track read error: %v", err)
				return
			}
			if _, err := localTrack.Write(buf[:n]); err != nil {
				log.Printf("Local track write error: %v", err)
				return
			}
		}
	})

	// Set remote description (publisher's offer)
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  msg.SDP,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		log.Printf("Failed to set publisher remote description: %v", err)
		conn.WriteJSON(SignalMessage{Type: "error", Message: "failed to set remote description"})
		return
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("Failed to create answer: %v", err)
		conn.WriteJSON(SignalMessage{Type: "error", Message: "failed to create answer"})
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("Failed to set local description: %v", err)
		return
	}

	conn.WriteJSON(SignalMessage{Type: "answer", SDP: answer.SDP})
}

func (s *Server) handlePublisherCandidate(msg SignalMessage) {
	s.mu.RLock()
	pc := s.publisherPC
	s.mu.RUnlock()

	if pc == nil {
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(msg.Candidate), &candidate); err != nil {
		log.Printf("Failed to unmarshal publisher ICE candidate: %v", err)
		return
	}
	if err := pc.AddICECandidate(candidate); err != nil {
		log.Printf("Failed to add publisher ICE candidate: %v", err)
	}
}

func (s *Server) cleanupPublisher() {
	s.mu.Lock()

	s.publisherConn = nil
	s.localTrack = nil

	if s.publisherPC != nil {
		s.publisherPC.Close()
		s.publisherPC = nil
	}

	// Close all viewer PeerConnections
	for _, v := range s.viewers {
		if v.PC != nil {
			v.PC.Close()
			v.PC = nil
		}
	}

	// Copy viewers for notification outside lock
	viewers := make([]*Viewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		viewers = append(viewers, v)
	}
	s.mu.Unlock()

	// Notify viewers
	for _, v := range viewers {
		v.SendJSON(SignalMessage{Type: "publisher_offline"})
	}
}

// ---------------------------------------------------------------------------
// Viewer handling
// ---------------------------------------------------------------------------

func (s *Server) handleViewer(conn *websocket.Conn, id string) {
	log.Printf("Viewer connected: %s", id)

	viewer := &Viewer{
		ID:   id,
		Conn: conn,
	}

	s.mu.Lock()
	s.viewers[id] = viewer
	s.mu.Unlock()

	defer func() {
		log.Printf("Viewer disconnected: %s", id)
		s.mu.Lock()
		delete(s.viewers, id)
		if viewer.PC != nil {
			viewer.PC.Close()
		}
		s.mu.Unlock()
		conn.Close()
	}()

	// If publisher track is already available, start negotiation
	s.mu.RLock()
	trackReady := s.localTrack != nil
	s.mu.RUnlock()

	if trackReady {
		viewer.SendJSON(SignalMessage{Type: "publisher_online"})
		s.negotiateViewer(viewer)
	}

	// Read signaling messages from viewer
	for {
		var msg SignalMessage
		if err := conn.ReadJSON(&msg); err != nil {
			log.Printf("Viewer %s read error: %v", id, err)
			return
		}

		switch msg.Type {
		case "answer":
			s.handleViewerAnswer(viewer, msg)
		case "candidate":
			s.handleViewerCandidate(viewer, msg)
		default:
			log.Printf("Viewer %s sent unknown message type: %s", id, msg.Type)
		}
	}
}

func (s *Server) negotiateViewer(viewer *Viewer) {
	s.mu.RLock()
	localTrack := s.localTrack
	s.mu.RUnlock()

	if localTrack == nil {
		return
	}

	// Create PeerConnection for this viewer
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: s.iceServers,
	})
	if err != nil {
		log.Printf("Failed to create viewer PeerConnection: %v", err)
		viewer.SendJSON(SignalMessage{Type: "error", Message: "failed to create peer connection"})
		return
	}

	viewer.mu.Lock()
	if viewer.PC != nil {
		viewer.PC.Close()
	}
	viewer.PC = pc
	viewer.mu.Unlock()

	// ICE candidate handler
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidateJSON, err := json.Marshal(c.ToJSON())
		if err != nil {
			log.Printf("Failed to marshal ICE candidate: %v", err)
			return
		}
		viewer.SendJSON(SignalMessage{
			Type:      "candidate",
			Candidate: string(candidateJSON),
		})
	})

	// Connection state handler
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Viewer %s PeerConnection state: %s", viewer.ID, state.String())
	})

	// Add the publisher's local track to this viewer's PC
	rtpSender, err := pc.AddTrack(localTrack)
	if err != nil {
		log.Printf("Failed to add track to viewer: %v", err)
		viewer.SendJSON(SignalMessage{Type: "error", Message: "failed to add track"})
		return
	}

	// Immediately request a keyframe so the new viewer gets a clean IDR
	// as soon as possible instead of seeing green until the next scheduled one.
	s.mu.RLock()
	pubPC := s.publisherPC
	s.mu.RUnlock()
	if pubPC != nil {
		if err := pubPC.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{},
		}); err != nil {
			log.Printf("Failed to send PLI on viewer join: %v", err)
		}
	}

	// Read and forward RTCP from the viewer (PLI/FIR → publisher)
	go func() {
		for {
			_, _, err := rtpSender.ReadRTCP()
			if err != nil {
				return
			}
			// When viewer sends PLI, forward it to the publisher
			s.mu.RLock()
			pubPC := s.publisherPC
			pubRecv := s.publisherRecv
			s.mu.RUnlock()
			if pubPC != nil && pubRecv != nil {
				// Request keyframe from publisher
				pubPC.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{},
				})
			}
		}
	}()

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Printf("Failed to create offer for viewer: %v", err)
		viewer.SendJSON(SignalMessage{Type: "error", Message: "failed to create offer"})
		return
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		log.Printf("Failed to set local description for viewer: %v", err)
		return
	}

	viewer.SendJSON(SignalMessage{Type: "offer", SDP: offer.SDP})
}

func (s *Server) handleViewerAnswer(viewer *Viewer, msg SignalMessage) {
	viewer.mu.Lock()
	pc := viewer.PC
	viewer.mu.Unlock()

	if pc == nil {
		return
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  msg.SDP,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		log.Printf("Failed to set viewer remote description: %v", err)
	}
}

func (s *Server) handleViewerCandidate(viewer *Viewer, msg SignalMessage) {
	viewer.mu.Lock()
	pc := viewer.PC
	viewer.mu.Unlock()

	if pc == nil {
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(msg.Candidate), &candidate); err != nil {
		log.Printf("Failed to unmarshal viewer ICE candidate: %v", err)
		return
	}
	if err := pc.AddICECandidate(candidate); err != nil {
		log.Printf("Failed to add viewer ICE candidate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Server) notifyViewers(msg SignalMessage) {
	s.mu.RLock()
	viewers := make([]*Viewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		viewers = append(viewers, v)
	}
	s.mu.RUnlock()

	for _, v := range viewers {
		v.SendJSON(msg)
	}
}

func (s *Server) negotiateWaitingViewers() {
	s.mu.RLock()
	viewers := make([]*Viewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		if v.PC == nil {
			viewers = append(viewers, v)
		}
	}
	s.mu.RUnlock()

	for _, v := range viewers {
		s.negotiateViewer(v)
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	srv := NewServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/ws", srv.handleWS)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// CORS wrapper
	handler := corsMiddleware(mux)

	log.Printf("CloudProxy SFU server starting on :%s", port)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
