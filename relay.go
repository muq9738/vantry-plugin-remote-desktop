package remotedesktop

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RelayServer is an HTTP handler. Serve it behind HTTPS outside a trusted
// network, with streaming buffering disabled in any reverse proxy.
// The first authenticated host reserves its random session ID until disconnect.
type RelayServer struct {
	mu       sync.Mutex
	sessions map[string]*relaySession
	closed   bool
	wg       sync.WaitGroup
}

func NewRelayServer() *RelayServer {
	return &RelayServer{sessions: make(map[string]*relaySession)}
}

func (s *RelayServer) Close() error {
	s.mu.Lock()
	s.closed = true
	sessions := make([]*relaySession, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
	s.wg.Wait()
	return nil
}

type relaySession struct {
	token string
	host  *packetStream
	ready chan struct{}
	done  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	video Frame
	info  Frame
	subs  map[*relaySubscriber]struct{}
}

type relaySubscriber struct {
	video   chan Frame
	control chan Frame
	done    chan struct{}
	once    sync.Once
}

func (sub *relaySubscriber) close() { sub.once.Do(func() { close(sub.done) }) }

func (s *relaySession) close() {
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		for sub := range s.subs {
			sub.close()
		}
		s.mu.Unlock()
		_ = s.host.Close()
	})
}

func (s *relaySession) subscribe() *relaySubscriber {
	sub := &relaySubscriber{video: make(chan Frame, 1), control: make(chan Frame, 16), done: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		sub.close()
		return sub
	default:
	}
	s.subs[sub] = struct{}{}
	if s.info.Type != 0 {
		sub.control <- s.info
	}
	if s.video.Type != 0 {
		sub.video <- s.video
	}
	return sub
}

func (s *relaySession) unsubscribe(sub *relaySubscriber) {
	s.mu.Lock()
	delete(s.subs, sub)
	s.mu.Unlock()
	sub.close()
}

func (s *relaySession) publish(f Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Type == FrameVideoJPEG {
		s.video = f
	} else if f.Type == FrameDisplayInfo {
		s.info = f
	}
	for sub := range s.subs {
		if f.Type == FrameVideoJPEG {
			select {
			case <-sub.video:
			default:
			}
			select {
			case sub.video <- f:
			default:
			}
		} else {
			select {
			case sub.control <- f:
			default:
				sub.close()
			}
		}
	}
}

func (s *RelayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "relay closed", http.StatusServiceUnavailable)
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "rd" || !validSessionID(parts[2]) {
		http.NotFound(w, r)
		return
	}
	kind, id := parts[1], parts[2]
	if kind == "host" || kind == "client" {
		// A finished or rejected streaming upload cannot be reused as an
		// ordinary keep-alive request on HTTP/1. HTTP/2 ignores this header.
		if r.ProtoMajor == 1 {
			w.Header().Set("Connection", "close")
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		// Enable this before authentication as well: otherwise a streaming
		// request with an invalid token could block while its body is drained.
		controller := http.NewResponseController(w)
		if err := controller.EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "full duplex unavailable", http.StatusInternalServerError)
			return
		}
		defer controller.SetReadDeadline(time.Now())
	}
	if kind == "host" {
		s.serveHost(w, r, id)
		return
	}
	s.mu.Lock()
	session := s.sessions[id]
	s.mu.Unlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}
	token := bearer(r)
	if (kind == "view" || kind == "mjpeg") && token == "" {
		token = r.URL.Query().Get("token")
	}
	if !sameToken(session.token, token) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	select {
	case <-session.ready:
	case <-session.done:
		http.Error(w, "host disconnected", http.StatusServiceUnavailable)
		return
	case <-r.Context().Done():
		return
	}
	switch kind {
	case "client":
		s.serveClient(w, r, session)
	case "view":
		if requireMethod(w, r, http.MethodGet) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
			_ = viewerTemplate.Execute(w, struct{ Session, Token string }{id, token})
		}
	case "mjpeg":
		if requireMethod(w, r, http.MethodGet) {
			s.serveMJPEG(w, r, session)
		}
	case "input":
		if requireMethod(w, r, http.MethodPost) {
			s.serveInput(w, r, session)
		}
	default:
		http.NotFound(w, r)
	}
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, method+" required", http.StatusMethodNotAllowed)
	return false
}

func bearer(r *http.Request) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return value
}

type serverStream struct {
	body       io.ReadCloser
	writer     http.ResponseWriter
	controller *http.ResponseController
	writeMu    sync.Mutex
	closed     atomic.Bool
	once       sync.Once
}

func newServerStream(w http.ResponseWriter, r *http.Request) (*packetStream, error) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	if err := controller.Flush(); err != nil {
		return nil, err
	}
	return &packetStream{ReadWriteCloser: &serverStream{body: r.Body, writer: w, controller: controller}}, nil
}

func (s *serverStream) Read(p []byte) (int, error) { return s.body.Read(p) }
func (s *serverStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return 0, ErrClosed
	}
	_ = s.controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
	n, err := s.writer.Write(p)
	if err == nil {
		err = s.controller.Flush()
	}
	return n, err
}
func (s *serverStream) Close() error {
	s.once.Do(func() {
		s.closed.Store(true)
		_ = s.controller.SetReadDeadline(time.Now())
		_ = s.controller.SetWriteDeadline(time.Now())
		// A viewer can still be forwarding input when the host disconnects.
		// Join its write before the HTTP handler returns its response writer.
		s.writeMu.Lock()
		s.writeMu.Unlock()
		_ = s.body.Close()
	})
	return nil
}

func (s *RelayServer) serveHost(w http.ResponseWriter, r *http.Request, id string) {
	token := bearer(r)
	if !validToken(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "relay closed", http.StatusServiceUnavailable)
		return
	}
	if existing := s.sessions[id]; existing != nil {
		s.mu.Unlock()
		if !sameToken(existing.token, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		} else {
			http.Error(w, "host already connected", http.StatusConflict)
		}
		return
	}
	// Install the stream while holding the map lock so Close always sees it.
	stream, err := newServerStream(w, r)
	if err != nil {
		s.mu.Unlock()
		return
	}
	session := &relaySession{token: token, host: stream, ready: make(chan struct{}), done: make(chan struct{}), subs: make(map[*relaySubscriber]struct{})}
	s.sessions[id] = session
	s.mu.Unlock()
	defer func() {
		session.close()
		s.mu.Lock()
		if s.sessions[id] == session {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
	}()
	timer := time.AfterFunc(handshakeTimeout, func() { _ = stream.Close() })
	err = stream.sendJSON(FrameHello, hello{Token: token})
	var first Frame
	if err == nil {
		first, err = ReadFrame(stream)
	}
	stopped := timer.Stop()
	if err != nil || first.Type != FrameAuthOK || !stopped {
		return
	}
	close(session.ready)
	for {
		f, err := ReadFrame(stream)
		if err != nil {
			return
		}
		if f.Type != FrameVideoJPEG && f.Type != FrameDisplayInfo && f.Type != FramePong {
			return
		}
		session.publish(f)
	}
}

func (s *RelayServer) serveClient(w http.ResponseWriter, r *http.Request, session *relaySession) {
	stream, err := newServerStream(w, r)
	if err != nil {
		return
	}
	defer stream.Close()
	timer := time.AfterFunc(handshakeTimeout, func() { _ = stream.Close() })
	first, err := ReadFrame(stream)
	var greeting hello
	if err == nil && len(first.Payload) <= 4096 {
		err = json.Unmarshal(first.Payload, &greeting)
	}
	stopped := timer.Stop()
	if err != nil || first.Type != FrameHello || !sameToken(session.token, greeting.Token) || greeting.DisplayID != 0 || !stopped {
		_ = stream.send(Frame{Type: FrameAuthErr})
		return
	}
	if _, err := Quality(greeting.Quality); err != nil {
		_ = stream.send(Frame{Type: FrameAuthErr})
		return
	}
	if err := stream.send(Frame{Type: FrameAuthOK}); err != nil {
		return
	}
	if greeting.Quality != "" {
		if err := session.host.send(first); err != nil {
			return
		}
	}
	sub := session.subscribe()
	defer session.unsubscribe(sub)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stream.Close()
		for {
			var f Frame
			select {
			case f = <-sub.control:
			default:
				select {
				case f = <-sub.control:
				case f = <-sub.video:
				case <-sub.done:
					return
				case <-r.Context().Done():
					return
				}
			}
			if err := stream.send(f); err != nil {
				return
			}
		}
	}()
	defer func() {
		sub.close()
		_ = stream.Close()
		<-done
	}()
	for {
		f, err := ReadFrame(stream)
		if err != nil {
			return
		}
		switch f.Type {
		case FrameInputMouse, FrameInputWheel, FrameInputKey:
			if validateInput(f) != nil {
				return
			}
		case FramePing:
			if len(f.Payload) != 8 {
				return
			}
		case FrameHello:
			var update hello
			if len(f.Payload) > 4096 || json.Unmarshal(f.Payload, &update) != nil || !sameToken(session.token, update.Token) || update.DisplayID != 0 {
				return
			}
			if _, err := Quality(update.Quality); err != nil {
				return
			}
		default:
			return
		}
		if err := session.host.send(f); err != nil {
			return
		}
	}
}

func (s *RelayServer) serveMJPEG(w http.ResponseWriter, r *http.Request, session *relaySession) {
	sub := session.subscribe()
	defer session.unsubscribe(sub)
	writer := multipart.NewWriter(w)
	_ = writer.SetBoundary("vantryframe")
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+writer.Boundary())
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	for {
		select {
		case <-sub.done:
			return
		case <-r.Context().Done():
			return
		case <-sub.control:
		case f := <-sub.video:
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			header := make(textproto.MIMEHeader)
			header.Set("Content-Type", "image/jpeg")
			header.Set("Content-Length", strconv.Itoa(len(f.Payload)))
			part, err := writer.CreatePart(header)
			if err != nil {
				return
			}
			if _, err = part.Write(f.Payload); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

// WebInput is the JSON body for POST /rd/input/<session>. Type is mouse, wheel,
// or key; the remaining fields use the same units as the native events.
type WebInput struct {
	Type      string  `json:"type"`
	Kind      string  `json:"kind,omitempty"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	Button    int     `json:"button,omitempty"`
	DX        float64 `json:"dx,omitempty"`
	DY        float64 `json:"dy,omitempty"`
	KeyCode   int     `json:"keyCode,omitempty"`
	Text      string  `json:"text,omitempty"`
	Modifiers uint32  `json:"modifiers,omitempty"`
}

func (s *RelayServer) serveInput(w http.ResponseWriter, r *http.Request, session *relaySession) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	var input WebInput
	var extra any
	if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
		http.Error(w, "invalid input JSON", http.StatusBadRequest)
		return
	}
	var f Frame
	switch input.Type {
	case "mouse":
		f, _ = jsonFrame(FrameInputMouse, MouseEvent{Kind: input.Kind, X: input.X, Y: input.Y, Button: input.Button})
	case "wheel":
		f, _ = jsonFrame(FrameInputWheel, WheelEvent{DX: input.DX, DY: input.DY})
	case "key":
		f, _ = jsonFrame(FrameInputKey, KeyEvent{Kind: input.Kind, KeyCode: input.KeyCode, Text: input.Text, Modifiers: input.Modifiers})
	default:
		http.Error(w, "invalid input type", http.StatusBadRequest)
		return
	}
	if validateInput(f) != nil {
		http.Error(w, "invalid input event", http.StatusBadRequest)
		return
	}
	if err := session.host.send(f); err != nil {
		http.Error(w, "host disconnected", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

//go:embed viewer.html
var viewerHTML string

var viewerTemplate = template.Must(template.New("viewer").Parse(viewerHTML))

// WaitForHost waits until a host's binary handshake has completed. It is useful
// for server health checks and callers that start a host and viewer together.
func (s *RelayServer) WaitForHost(ctx context.Context, id string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		session, closed := s.sessions[id], s.closed
		s.mu.Unlock()
		if closed {
			return ErrClosed
		}
		if session != nil {
			select {
			case <-session.ready:
				return nil
			case <-session.done:
				return ErrClosed
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
