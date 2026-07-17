package main

import (
	"embed"
	"encoding/binary"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

//go:embed viewer/dist
var viewerFS embed.FS

// ---------------------------------------------------------------------------
// Binary frame flags (byte 0 of every binary message)
// ---------------------------------------------------------------------------

const (
	flagKeyframe = 0x01 // Contains an IDR NAL unit
	flagSPSPPS   = 0x02 // Contains SPS/PPS codec init data
)

// Maximum binary message size (1 MB).
const maxMessageSize = 1 << 20

// ---------------------------------------------------------------------------
// JSON control messages
// ---------------------------------------------------------------------------

type ControlMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Viewer
// ---------------------------------------------------------------------------

type Viewer struct {
	ID   string
	Conn *websocket.Conn
	mu   sync.Mutex
}

func (v *Viewer) SendJSON(msg ControlMessage) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Conn.WriteJSON(msg)
}

func (v *Viewer) SendBinary(data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Conn.WriteMessage(websocket.BinaryMessage, data)
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type Server struct {
	mu            sync.RWMutex
	publisherConn *websocket.Conn
	viewers       map[string]*Viewer

	// Cached init data for new viewer joins.
	initData     []byte // most recent SPS/PPS frame (binary msg with flag 0x02)
	lastKeyframe []byte // most recent keyframe (binary msg with flag 0x01)

	authToken string
}

func NewServer() *Server {
	token := os.Getenv("AUTH_TOKEN")
	if token == "" {
		token = "cloudproxy-dev-token"
	}
	return &Server{
		viewers:   make(map[string]*Viewer),
		authToken: token,
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

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	id := uuid.New().String()

	// Send welcome
	if err := conn.WriteJSON(ControlMessage{Type: "welcome", ID: id}); err != nil {
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
		s.publisherConn.Close()
	}
	s.publisherConn = conn
	// Clear cached frames from old publisher.
	s.initData = nil
	s.lastKeyframe = nil
	s.mu.Unlock()

	// Notify existing viewers that the publisher is online.
	s.notifyViewers(ControlMessage{Type: "publisher_online"})

	defer func() {
		log.Printf("Publisher disconnected: %s", id)
		s.cleanupPublisher(conn)
	}()

	conn.SetReadLimit(maxMessageSize)

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Publisher read error: %v", err)
			return
		}

		switch msgType {
		case websocket.TextMessage:
			// JSON control message from publisher — just log it.
			log.Printf("Publisher text message: %s", string(data))

		case websocket.BinaryMessage:
			s.handlePublisherBinary(data)
		}
	}
}

func (s *Server) handlePublisherBinary(data []byte) {
	if len(data) < 5 {
		log.Printf("Binary message too short (%d bytes), ignoring", len(data))
		return
	}

	flags := data[0]
	ts := binary.BigEndian.Uint32(data[1:5])

	// Cache SPS/PPS and/or keyframe for new viewer init.
	s.mu.Lock()
	if flags&flagSPSPPS != 0 {
		s.initData = make([]byte, len(data))
		copy(s.initData, data)
	}
	if flags&flagKeyframe != 0 {
		s.lastKeyframe = make([]byte, len(data))
		copy(s.lastKeyframe, data)
	}
	// Snapshot viewers for broadcast.
	viewers := make([]*Viewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		viewers = append(viewers, v)
	}
	s.mu.Unlock()

	if len(viewers) > 0 {
		log.Printf("Broadcasting frame: flags=0x%02x ts=%dms size=%d to %d viewer(s)",
			flags, ts, len(data), len(viewers))
	}

	// Broadcast to all viewers.
	for _, v := range viewers {
		if err := v.SendBinary(data); err != nil {
			log.Printf("Viewer %s write error, removing: %v", v.ID, err)
			s.removeViewer(v.ID)
			v.Conn.Close()
		}
	}
}

func (s *Server) cleanupPublisher(conn *websocket.Conn) {
	s.mu.Lock()
	// Only clean up if this conn is still the active publisher.
	if s.publisherConn != conn {
		s.mu.Unlock()
		conn.Close()
		return
	}
	s.publisherConn = nil
	s.initData = nil
	s.lastKeyframe = nil

	viewers := make([]*Viewer, 0, len(s.viewers))
	for _, v := range s.viewers {
		viewers = append(viewers, v)
	}
	s.mu.Unlock()

	conn.Close()

	for _, v := range viewers {
		v.SendJSON(ControlMessage{Type: "publisher_offline"})
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
	publisherOnline := s.publisherConn != nil
	// Grab cached data while holding the lock.
	var cachedInit []byte
	var cachedKeyframe []byte
	if publisherOnline {
		if s.initData != nil {
			cachedInit = make([]byte, len(s.initData))
			copy(cachedInit, s.initData)
		}
		if s.lastKeyframe != nil {
			cachedKeyframe = make([]byte, len(s.lastKeyframe))
			copy(cachedKeyframe, s.lastKeyframe)
		}
	}
	s.mu.Unlock()

	defer func() {
		log.Printf("Viewer disconnected: %s", id)
		s.removeViewer(id)
		conn.Close()
	}()

	// If the publisher is already online, send cached data to bootstrap the viewer.
	if publisherOnline {
		viewer.SendJSON(ControlMessage{Type: "publisher_online"})
		if cachedInit != nil {
			if err := viewer.SendBinary(cachedInit); err != nil {
				log.Printf("Viewer %s: failed to send cached init data: %v", id, err)
				return
			}
		}
		if cachedKeyframe != nil {
			if err := viewer.SendBinary(cachedKeyframe); err != nil {
				log.Printf("Viewer %s: failed to send cached keyframe: %v", id, err)
				return
			}
		}
	}

	// Read loop — viewers shouldn't send binary, but we read to detect disconnect.
	conn.SetReadLimit(maxMessageSize)
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Viewer %s read error: %v", id, err)
			return
		}

		switch msgType {
		case websocket.TextMessage:
			log.Printf("Viewer %s text message: %s", id, string(data))
		default:
			// Ignore unexpected binary from viewer.
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Server) removeViewer(id string) {
	s.mu.Lock()
	delete(s.viewers, id)
	s.mu.Unlock()
}

func (s *Server) notifyViewers(msg ControlMessage) {
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

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	srv := NewServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/ws", srv.handleWS)

	// Serve the pre-built viewer UI.
	// Strip the "viewer/dist" prefix so the embedded FS root is "/".
	distFS, err := fs.Sub(viewerFS, "viewer/dist")
	if err != nil {
		log.Fatalf("Failed to sub viewer/dist: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(distFS)))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	handler := corsMiddleware(mux)

	log.Printf("CloudProxy WebSocket server starting on :%s", port)
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
