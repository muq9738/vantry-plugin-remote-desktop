package remotedesktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

type HostConfig struct {
	// ListenAddr defaults to 127.0.0.1:0. Use 0.0.0.0:0 to accept LAN and
	// Tailscale connections; loopback listeners only advertise loopback.
	ListenAddr   string
	Capturer     Capturer
	InputSink    InputSink
	DisplayID    uint32
	Quality      string
	SessionID    string
	RelayURL     string
	RelaySSHHost string
	RemotePort   int
	SSHCommand   string
	HTTPClient   *http.Client
}

// Host owns capture, the listener, active connections, and optional relay jobs.
// Close stops and joins all of those jobs. Construct it with StartHost.
type Host struct {
	cfg      HostConfig
	ctx      context.Context
	cancel   context.CancelFunc
	listener net.Listener
	token    string
	session  string
	display  DisplayInfo
	direct   []string

	captureMu sync.Mutex
	inputMu   sync.Mutex
	mu        sync.Mutex
	peers     map[*packetStream]struct{}
	lastErr   error
	wg        sync.WaitGroup
	once      sync.Once
}

func StartHost(ctx context.Context, cfg HostConfig) (*Host, error) {
	if ctx == nil {
		return nil, errors.New("remote desktop: context is required")
	}
	if _, err := Quality(cfg.Quality); err != nil {
		return nil, err
	}
	if cfg.SessionID != "" && !validSessionID(cfg.SessionID) {
		return nil, errors.New("remote desktop: invalid session ID")
	}
	if cfg.RelaySSHHost != "" {
		if err := sshOptions(cfg.RelaySSHHost, cfg.RemotePort); err != nil {
			return nil, err
		}
	}
	var err error
	nativeCapture := cfg.Capturer == nil
	if cfg.Capturer == nil {
		cfg.Capturer, err = NewCapturer()
		if err != nil {
			return nil, err
		}
	}
	if cfg.InputSink == nil {
		cfg.InputSink, err = NewInputSink()
		if err != nil {
			return nil, err
		}
	}
	displays, err := cfg.Capturer.Displays(ctx)
	if err != nil {
		return nil, err
	}
	if len(displays) == 0 {
		return nil, errors.New("remote desktop: no active displays")
	}
	display := displays[0]
	found := cfg.DisplayID == 0
	for _, d := range displays {
		if cfg.DisplayID != 0 && d.ID == cfg.DisplayID || cfg.DisplayID == 0 && d.Main {
			display, found = d, true
		}
	}
	if !found {
		return nil, errors.New("remote desktop: requested display is not active")
	}
	// Verify native screen access before reporting that hosting has started.
	if nativeCapture {
		frame, err := cfg.Capturer.Capture(ctx, display.ID, .5)
		if err != nil {
			return nil, err
		}
		if releaser, ok := cfg.Capturer.(FrameReleaser); ok {
			releaser.Release(frame)
		}
	}
	token, err := newToken(32)
	if err != nil {
		return nil, err
	}
	if cfg.SessionID == "" {
		cfg.SessionID, err = newToken(16)
		if err != nil {
			return nil, err
		}
	}
	if cfg.RelayURL != "" {
		if _, err := relayEndpoint(cfg.RelayURL, "host", cfg.SessionID); err != nil {
			return nil, err
		}
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &Host{cfg: cfg, ctx: ctx, cancel: cancel, listener: listener, token: token,
		session: cfg.SessionID, display: display, peers: make(map[*packetStream]struct{})}
	h.direct = discoverDirectAddrs(listener.Addr())
	// The accept loop remains counted while it can add connection workers.
	h.wg.Add(1)
	go h.acceptLoop()
	if cfg.RelaySSHHost != "" {
		cmd := h.forwardCommand()
		if err := cmd.Start(); err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("remote desktop: start SSH forward: %w", err)
		}
		h.wg.Add(1)
		go h.maintainSSH(cmd)
	}
	if cfg.RelayURL != "" {
		endpoint, _ := relayEndpoint(cfg.RelayURL, "host", h.session)
		stream, err := openHTTPStream(h.ctx, cfg.HTTPClient, endpoint, h.token)
		if err != nil {
			_ = h.Close()
			return nil, err
		}
		h.wg.Add(1)
		go h.maintainHTTP(stream, endpoint)
	}
	context.AfterFunc(ctx, func() { _ = h.Close() })
	return h, nil
}

func (h *Host) Addr() net.Addr        { return h.listener.Addr() }
func (h *Host) Token() string         { return h.token }
func (h *Host) SessionID() string     { return h.session }
func (h *Host) DirectAddrs() []string { return append([]string(nil), h.direct...) }

// ConnectConfig returns credentials and reachable routes for sharing with a viewer.
func (h *Host) ConnectConfig() ConnectConfig {
	return ConnectConfig{DirectAddrs: h.DirectAddrs(), Token: h.token, SessionID: h.session,
		Quality: h.cfg.Quality, RelayURL: h.cfg.RelayURL, RelaySSHHost: h.cfg.RelaySSHHost,
		RemotePort: h.cfg.RemotePort, SSHCommand: h.cfg.SSHCommand, HTTPClient: h.cfg.HTTPClient}
}

// Err reports the last capture, input, or relay failure without including credentials.
func (h *Host) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastErr
}

func (h *Host) recordError(err error) {
	if err == nil || h.ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return
	}
	h.mu.Lock()
	h.lastErr = err
	h.mu.Unlock()
}

func (h *Host) Close() error {
	h.once.Do(func() {
		h.cancel()
		_ = h.listener.Close()
		h.mu.Lock()
		peers := make([]*packetStream, 0, len(h.peers))
		for p := range h.peers {
			peers = append(peers, p)
		}
		h.mu.Unlock()
		for _, p := range peers {
			_ = p.Close()
		}
	})
	h.wg.Wait()
	return nil
}

func (h *Host) acceptLoop() {
	defer h.wg.Done()
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.servePeer(conn)
		}()
	}
}

func (h *Host) servePeer(conn io.ReadWriteCloser) {
	p := &packetStream{ReadWriteCloser: conn}
	h.mu.Lock()
	if h.ctx.Err() != nil {
		h.mu.Unlock()
		_ = p.Close()
		return
	}
	h.peers[p] = struct{}{}
	h.mu.Unlock()
	defer func() {
		_ = p.Close()
		h.mu.Lock()
		delete(h.peers, p)
		h.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(h.ctx, handshakeTimeout)
	stop := context.AfterFunc(ctx, func() { _ = p.Close() })
	f, err := ReadFrame(p)
	var greeting hello
	if err == nil && len(f.Payload) <= 4096 {
		err = json.Unmarshal(f.Payload, &greeting)
	}
	if err != nil || f.Type != FrameHello || !sameToken(h.token, greeting.Token) {
		_ = p.send(Frame{Type: FrameAuthErr})
		stop()
		cancel()
		return
	}
	qualityName := greeting.Quality
	if qualityName == "" {
		qualityName = h.cfg.Quality
	}
	quality, err := Quality(qualityName)
	if err != nil || greeting.DisplayID != 0 && greeting.DisplayID != h.display.ID {
		_ = p.send(Frame{Type: FrameAuthErr})
		stop()
		cancel()
		return
	}
	if err = p.send(Frame{Type: FrameAuthOK}); err == nil {
		err = p.sendJSON(FrameDisplayInfo, h.display)
	}
	stopped := stop()
	cancel()
	if err != nil || !stopped || h.ctx.Err() != nil {
		return
	}
	peerCtx, peerCancel := context.WithCancel(h.ctx)
	defer peerCancel()
	qualities := make(chan QualityPreset, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer p.Close()
		h.recordError(h.sendVideo(peerCtx, p, quality, qualities))
	}()
	for {
		f, err := ReadFrame(p)
		if err != nil {
			break
		}
		switch f.Type {
		case FramePing:
			if len(f.Payload) != 8 || p.send(Frame{Type: FramePong, Payload: f.Payload}) != nil {
				goto finished
			}
		case FrameHello:
			var update hello
			if len(f.Payload) > 4096 || json.Unmarshal(f.Payload, &update) != nil || !sameToken(h.token, update.Token) || update.DisplayID != 0 && update.DisplayID != h.display.ID {
				goto finished
			}
			q, err := Quality(update.Quality)
			if err != nil {
				goto finished
			}
			select {
			case <-qualities:
			default:
			}
			qualities <- q
		case FrameInputMouse, FrameInputWheel, FrameInputKey:
			if err := h.deliverInput(peerCtx, f); err != nil {
				h.recordError(err)
				goto finished
			}
		default:
			goto finished
		}
	}
finished:
	peerCancel()
	_ = p.Close()
	<-done
}

func (h *Host) deliverInput(ctx context.Context, f Frame) error {
	if err := validateInput(f); err != nil {
		return err
	}
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	switch f.Type {
	case FrameInputMouse:
		var e MouseEvent
		_ = json.Unmarshal(f.Payload, &e)
		e.DisplayID = h.display.ID
		return h.cfg.InputSink.Mouse(ctx, e)
	case FrameInputWheel:
		var e WheelEvent
		_ = json.Unmarshal(f.Payload, &e)
		return h.cfg.InputSink.Wheel(ctx, e)
	case FrameInputKey:
		var e KeyEvent
		_ = json.Unmarshal(f.Payload, &e)
		return h.cfg.InputSink.Key(ctx, e)
	}
	return nil
}

func (h *Host) sendVideo(ctx context.Context, p *packetStream, q QualityPreset, updates <-chan QualityPreset) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var previous *image.RGBA
	var sentAt time.Time
	force := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case q = <-updates:
			force = true
		case <-timer.C:
		}
		data, next, err := h.encodeFrame(ctx, q, previous, force || time.Since(sentAt) >= 2*time.Second)
		if err != nil {
			return err
		}
		previous = next
		if data != nil {
			if err := p.send(Frame{Type: FrameVideoJPEG, Payload: data}); err != nil {
				return err
			}
			sentAt, force = time.Now(), false
		}
		timer.Reset(time.Second / time.Duration(q.FPS))
	}
}

func (h *Host) encodeFrame(ctx context.Context, q QualityPreset, previous *image.RGBA, force bool) ([]byte, *image.RGBA, error) {
	h.captureMu.Lock()
	defer h.captureMu.Unlock()
	frame, err := h.cfg.Capturer.Capture(ctx, h.display.ID, q.Scale)
	if err != nil {
		return nil, previous, err
	}
	if frame != nil {
		if releaser, ok := h.cfg.Capturer.(FrameReleaser); ok {
			defer releaser.Release(frame)
		}
	}
	if frame == nil || frame.Rect.Empty() {
		return nil, previous, errors.New("remote desktop: capturer returned an empty frame")
	}
	if !force && sampledEqual(frame, previous) {
		return nil, previous, nil
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, frame, &jpeg.Options{Quality: q.JPEGQuality}); err != nil {
		return nil, previous, err
	}
	bounds := image.Rect(0, 0, frame.Rect.Dx(), frame.Rect.Dy())
	if previous == nil || previous.Rect != bounds {
		previous = image.NewRGBA(bounds)
	}
	draw.Draw(previous, bounds, frame, frame.Rect.Min, draw.Src)
	return encoded.Bytes(), previous, nil
}

func discoverDirectAddrs(addr net.Addr) []string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil
	}
	if !tcp.IP.IsUnspecified() {
		return []string{tcp.String()}
	}
	var result []string
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() {
			result = append(result, net.JoinHostPort(ip.String(), strconv.Itoa(tcp.Port)))
		}
	}
	return result
}

func (h *Host) forwardCommand() *exec.Cmd {
	_, localPort, _ := net.SplitHostPort(h.listener.Addr().String())
	forward := "127.0.0.1:" + strconv.Itoa(h.cfg.RemotePort) + ":127.0.0.1:" + localPort
	return exec.CommandContext(h.ctx, sshCommand(h.cfg.SSHCommand), "-N", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15", "-R", forward, h.cfg.RelaySSHHost)
}

func (h *Host) maintainSSH(cmd *exec.Cmd) {
	defer h.wg.Done()
	for {
		h.recordError(cmd.Wait())
		if !retryDelay(h.ctx) {
			return
		}
		cmd = h.forwardCommand()
		for cmd.Start() != nil {
			if !retryDelay(h.ctx) {
				return
			}
			cmd = h.forwardCommand()
		}
	}
}

func (h *Host) maintainHTTP(stream io.ReadWriteCloser, endpoint string) {
	defer h.wg.Done()
	for {
		h.servePeer(stream)
		if !retryDelay(h.ctx) {
			return
		}
		var err error
		stream, err = openHTTPStream(h.ctx, h.cfg.HTTPClient, endpoint, h.token)
		for err != nil {
			h.recordError(err)
			if !retryDelay(h.ctx) {
				return
			}
			stream, err = openHTTPStream(h.ctx, h.cfg.HTTPClient, endpoint, h.token)
		}
	}
}

func retryDelay(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
