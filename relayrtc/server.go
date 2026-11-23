package relayrtc

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type session struct {
	id, hostToken, viewerToken string
	video                      json.RawMessage
	touched                    time.Time
	peers                      map[string]*peer
}

type SessionInfo struct {
	SessionID          string          `json:"sessionId"`
	HostToken          string          `json:"hostToken,omitempty"`
	ViewerToken        string          `json:"viewerToken,omitempty"`
	ICEServers         []ICEServer     `json:"iceServers"`
	ICETransportPolicy string          `json:"iceTransportPolicy"`
	ExpiresAt          time.Time       `json:"expiresAt"`
	SessionExpiresAt   time.Time       `json:"sessionExpiresAt"`
	Video              json.RawMessage `json:"video"`
	Transport          string          `json:"transport,omitempty"`
	TurnTCPPort        int             `json:"turnTcpPort,omitempty"`
}

type Server struct {
	config    Config
	mu        sync.Mutex
	sessions  map[string]*session
	now       func() time.Time
	closed    chan struct{}
	closeOnce sync.Once
	workers   sync.WaitGroup
}

func New(config Config) (*Server, error) {
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}
	s := &Server{config: config, sessions: map[string]*session{}, now: time.Now, closed: make(chan struct{})}
	s.workers.Add(1)
	go func() { defer s.workers.Done(); s.expiryLoop() }()
	return s, nil
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.mu.Lock()
		var peers []*peer
		for _, room := range s.sessions {
			for _, p := range room.peers {
				peers = append(peers, p)
			}
		}
		s.sessions = map[string]*session{}
		s.mu.Unlock()
		for _, p := range peers {
			p.close()
		}
		s.workers.Wait()
	})
	return nil
}

func (s *Server) expired(room *session) bool {
	return room.peers["host"] == nil && s.now().Sub(room.touched) > time.Duration(s.config.SessionIdleSecs)*time.Second
}

func (s *Server) expire() {
	s.mu.Lock()
	var peers []*peer
	for id, room := range s.sessions {
		if s.expired(room) {
			delete(s.sessions, id)
			for _, p := range room.peers {
				peers = append(peers, p)
			}
		}
	}
	s.mu.Unlock()
	for _, p := range peers {
		p.close()
	}
}

func (s *Server) expiryLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ticker.C:
			s.expire()
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	select {
	case <-s.closed:
		http.Error(w, "signaling unavailable", 503)
		return
	default:
	}
	if r.URL.Path == "/v1/health" && r.Method == http.MethodGet {
		writeJSON(w, map[string]string{"state": "ready"})
		return
	}
	if r.URL.Path == "/v1/sessions" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		s.create(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")
	if len(parts) < 1 || parts[0] == "" || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	room := s.sessions[parts[0]]
	if room != nil && s.expired(room) {
		room = nil
	}
	s.mu.Unlock()
	if room == nil {
		http.NotFound(w, r)
		return
	}
	role := authorizedRole(r, room)
	if role == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	if len(parts) == 2 {
		if parts[1] != "signal" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("role") != role {
			http.Error(w, "role mismatch", 403)
			return
		}
		s.signal(w, r, room, role)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		if s.sessions[room.id] != room {
			s.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		room.touched = s.now()
		info := s.info(room, role, false)
		s.mu.Unlock()
		writeJSON(w, info)
	case http.MethodDelete:
		if role != "host" {
			http.Error(w, "host authorization required", 403)
			return
		}
		s.mu.Lock()
		delete(s.sessions, room.id)
		var peers []*peer
		for _, p := range room.peers {
			peers = append(peers, p)
		}
		s.mu.Unlock()
		for _, p := range peers {
			p.close()
		}
		writeJSON(w, map[string]bool{"deleted": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func authorizedRole(r *http.Request, room *session) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(value), []byte(room.hostToken)) == 1 {
		return "host"
	}
	if subtle.ConstantTimeCompare([]byte(value), []byte(room.viewerToken)) == 1 {
		return "viewer"
	}
	return ""
}

func (s *Server) info(room *session, role string, tokens bool) SessionInfo {
	expires := s.now().Add(time.Duration(s.config.CredentialSecs) * time.Second).UTC().Truncate(time.Second)
	info := SessionInfo{SessionID: room.id, ICEServers: []ICEServer{s.config.iceServer(room.id, role, expires)}, ICETransportPolicy: "relay", ExpiresAt: expires, SessionExpiresAt: room.touched.Add(time.Duration(s.config.SessionIdleSecs) * time.Second).UTC(), Video: append(json.RawMessage(nil), room.video...)}
	info.Transport, info.TurnTCPPort = s.config.Transport, s.config.TurnTCPPort
	if tokens {
		info.HostToken, info.ViewerToken = room.hostToken, room.viewerToken
	}
	return info
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Video json.RawMessage `json:"video"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid session request", 400)
		return
	}
	if len(input.Video) == 0 {
		input.Video = json.RawMessage(`{}`)
	}
	var video map[string]json.RawMessage
	if json.Unmarshal(input.Video, &video) != nil || video == nil {
		http.Error(w, "video must be an object", 400)
		return
	}
	id, err := randomToken(15)
	if err != nil {
		http.Error(w, "session creation failed", 500)
		return
	}
	host, err := randomToken(32)
	if err != nil {
		http.Error(w, "session creation failed", 500)
		return
	}
	viewer, err := randomToken(32)
	if err != nil {
		http.Error(w, "session creation failed", 500)
		return
	}
	s.expire()
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		http.Error(w, "signaling unavailable", 503)
		return
	default:
	}
	if len(s.sessions) >= s.config.MaxSessions {
		s.mu.Unlock()
		http.Error(w, "session capacity reached", 503)
		return
	}
	room := &session{id: id, hostToken: host, viewerToken: viewer, video: append(json.RawMessage(nil), input.Video...), touched: s.now(), peers: map[string]*peer{}}
	s.sessions[id] = room
	info := s.info(room, "host", true)
	s.mu.Unlock()
	writeJSON(w, info)
}

type peer struct {
	conn *wsConn
	out  chan []byte
	done chan struct{}
	once sync.Once
}

func (p *peer) close() { p.once.Do(func() { close(p.done); _ = p.conn.Close() }) }
func (p *peer) enqueue(data []byte) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.out <- data:
		return true
	default:
		p.close()
		return false
	}
}
func (p *peer) event(value any) { data, _ := json.Marshal(value); p.enqueue(data) }
func (p *peer) writeLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	defer p.close()
	for {
		select {
		case <-p.done:
			return
		case data := <-p.out:
			if p.conn.writeFrame(1, data) != nil {
				return
			}
		case <-ticker.C:
			if p.conn.writeFrame(9, nil) != nil {
				return
			}
		}
	}
}
func opposite(role string) string {
	if role == "host" {
		return "viewer"
	}
	return "host"
}

func (s *Server) signal(w http.ResponseWriter, r *http.Request, room *session, role string) {
	conn, err := upgrade(w, r)
	if err != nil {
		return
	}
	p := &peer{conn: conn, out: make(chan []byte, 32), done: make(chan struct{})}
	s.mu.Lock()
	if s.sessions[room.id] != room {
		s.mu.Unlock()
		p.close()
		return
	}
	previous, other := room.peers[role], room.peers[opposite(role)]
	room.peers[role] = p
	room.touched = s.now()
	s.workers.Add(2)
	s.mu.Unlock()
	defer s.workers.Done()
	if previous != nil {
		previous.close()
		if other != nil {
			other.event(map[string]any{"type": "peer", "connected": false})
		}
	}
	go func() { defer s.workers.Done(); p.writeLoop() }()
	if other != nil {
		event := map[string]any{"type": "peer", "connected": true}
		other.event(event)
		p.event(event)
	}
	defer func() {
		p.close()
		s.mu.Lock()
		var notify *peer
		if s.sessions[room.id] == room && room.peers[role] == p {
			delete(room.peers, role)
			room.touched = s.now()
			notify = room.peers[opposite(role)]
		}
		s.mu.Unlock()
		if notify != nil {
			notify.event(map[string]any{"type": "peer", "connected": false})
		}
	}()
	for {
		data, err := conn.readMessage()
		if err != nil {
			return
		}
		if !validSignal(data, role) {
			p.event(map[string]string{"type": "error", "error": "invalid signaling message"})
			continue
		}
		s.mu.Lock()
		if s.sessions[room.id] != room || room.peers[role] != p {
			s.mu.Unlock()
			return
		}
		room.touched = s.now()
		target := room.peers[opposite(role)]
		s.mu.Unlock()
		if target == nil || !target.enqueue(data) {
			p.event(map[string]string{"type": "error", "error": "peer unavailable"})
		}
		var kind struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &kind)
		if kind.Type == "bye" {
			return
		}
	}
}

func validSignal(data []byte, role string) bool {
	var message struct {
		Type      string          `json:"type"`
		SDP       string          `json:"sdp"`
		Candidate json.RawMessage `json:"candidate"`
	}
	if json.Unmarshal(data, &message) != nil {
		return false
	}
	switch message.Type {
	case "offer":
		return role == "host" && message.SDP != ""
	case "answer":
		return role == "viewer" && message.SDP != ""
	case "ice":
		if string(message.Candidate) == "null" {
			return true
		}
		var value map[string]json.RawMessage
		return len(message.Candidate) > 0 && json.Unmarshal(message.Candidate, &value) == nil && value != nil
	case "bye":
		return true
	default:
		return false
	}
}
