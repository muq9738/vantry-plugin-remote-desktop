package remotedesktop

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type ConnectConfig struct {
	DirectAddrs   []string
	DirectTimeout time.Duration // defaults to 700ms for the whole authenticated race
	Token         string
	SessionID     string
	Quality       string
	RelayURL      string
	RelaySSHHost  string
	RemotePort    int
	SSHCommand    string // executable path; arguments are passed without a shell
	HTTPClient    *http.Client
}

type SessionStats struct {
	Mode      string
	RelayHost string
	RTTMs     float64
	FPS       float64
	Width     int
	Height    int
	Kbps      float64
}

// ClientSession decodes into two internal buffers. LatestFrame returns an owned
// snapshot so consumers may retain it while more frames arrive.
type ClientSession struct {
	stream *packetStream
	ctx    context.Context
	cancel context.CancelFunc
	token  string
	wg     sync.WaitGroup
	once   sync.Once
	done   chan struct{}
	notify chan struct{}

	mu       sync.RWMutex
	front    *image.RGBA
	back     *image.RGBA
	seq      uint64
	stats    SessionStats
	display  DisplayInfo
	callback func()
	lastErr  error
	ping     int64
	started  time.Time
	bytes    uint64
}

// Dial tries all direct addresses concurrently, including authentication, before
// trying the HTTP relay or the SSH stdio route. ctx controls session lifetime.
func Dial(ctx context.Context, cfg ConnectConfig) (*ClientSession, error) {
	if ctx == nil {
		return nil, errors.New("remote desktop: context is required")
	}
	if !validToken(cfg.Token) {
		return nil, ErrUnauthorized
	}
	if _, err := Quality(cfg.Quality); err != nil {
		return nil, err
	}
	if cfg.DirectTimeout <= 0 {
		cfg.DirectTimeout = 700 * time.Millisecond
	}
	stream, directErr := dialDirect(ctx, cfg)
	mode, relayHost := "direct", ""
	if stream == nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var raw io.ReadWriteCloser
		var err error
		switch {
		case cfg.RelayURL != "":
			endpoint, endpointErr := relayEndpoint(cfg.RelayURL, "client", cfg.SessionID)
			if endpointErr != nil {
				return nil, endpointErr
			}
			raw, err = openHTTPStream(ctx, cfg.HTTPClient, endpoint, cfg.Token)
			mode = "relay-http"
			u, _ := url.Parse(cfg.RelayURL)
			relayHost = u.Host
		case cfg.RelaySSHHost != "":
			raw, err = openSSHStream(ctx, cfg.SSHCommand, cfg.RelaySSHHost, cfg.RemotePort)
			mode, relayHost = "relay-ssh", cfg.RelaySSHHost
		default:
			if directErr != nil {
				return nil, directErr
			}
			return nil, errors.New("remote desktop: no connection routes configured")
		}
		if err != nil {
			return nil, err
		}
		stream, err = authenticate(ctx, raw, cfg)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &ClientSession{stream: stream, ctx: ctx, cancel: cancel, token: cfg.Token,
		done: make(chan struct{}), notify: make(chan struct{}, 1), started: time.Now(),
		stats: SessionStats{Mode: mode, RelayHost: relayHost}}
	s.wg.Add(2)
	go s.readLoop()
	go s.pingLoop()
	go s.callbackLoop()
	context.AfterFunc(ctx, func() { _ = s.Close() })
	return s, nil
}

func dialDirect(ctx context.Context, cfg ConnectConfig) (*packetStream, error) {
	if len(cfg.DirectAddrs) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.DirectTimeout)
	defer cancel()
	type result struct {
		stream *packetStream
		err    error
	}
	results := make(chan result)
	for _, addr := range cfg.DirectAddrs {
		go func(addr string) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			var stream *packetStream
			if err == nil {
				stream, err = authenticate(ctx, raw, cfg)
			}
			select {
			case results <- result{stream, err}:
			case <-ctx.Done():
				if stream != nil {
					_ = stream.Close()
				}
			}
		}(addr)
	}
	var failures []error
	for range cfg.DirectAddrs {
		select {
		case r := <-results:
			if r.stream != nil {
				return r.stream, nil
			}
			failures = append(failures, r.err)
		case <-ctx.Done():
			return nil, errors.Join(append(failures, ctx.Err())...)
		}
	}
	return nil, errors.Join(failures...)
}

func authenticate(ctx context.Context, raw io.ReadWriteCloser, cfg ConnectConfig) (*packetStream, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	stream := &packetStream{ReadWriteCloser: raw}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	err := stream.sendJSON(FrameHello, hello{Token: cfg.Token, Quality: cfg.Quality})
	var f Frame
	if err == nil {
		f, err = ReadFrame(raw)
	}
	if err == nil && f.Type != FrameAuthOK {
		err = ErrUnauthorized
	}
	if err == nil && (!stop() || ctx.Err() != nil) {
		err = ctx.Err()
		if err == nil {
			err = context.DeadlineExceeded
		}
	}
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return stream, nil
}

// LatestFrame returns a stable RGBA snapshot, sequence, and current statistics.
func (s *ClientSession) LatestFrame() (*image.RGBA, uint64, SessionStats) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var frame *image.RGBA
	if s.front != nil {
		frame = image.NewRGBA(s.front.Rect)
		copy(frame.Pix, s.front.Pix)
	}
	stats := s.stats
	elapsed := time.Since(s.started).Seconds()
	if elapsed > 0 {
		stats.FPS = float64(s.seq) / elapsed
		stats.Kbps = float64(s.bytes) * 8 / 1000 / elapsed
	}
	return frame, s.seq, stats
}

// Display returns the source display metadata (pixels, origin, and main flag).
func (s *ClientSession) Display() DisplayInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.display
}

// OnFrame installs one coalesced asynchronous notification callback. It may call
// any session method, including Close. A pending frame also triggers it.
func (s *ClientSession) OnFrame(fn func()) {
	s.mu.Lock()
	s.callback = fn
	pending := s.seq > 0
	s.mu.Unlock()
	if pending {
		s.signalFrame()
	}
}

func (s *ClientSession) SendMouse(kind string, normX, normY float64, button int) error {
	return s.sendInput(FrameInputMouse, MouseEvent{Kind: kind, X: normX, Y: normY, Button: button})
}

func (s *ClientSession) SendWheel(dx, dy float64) error {
	return s.sendInput(FrameInputWheel, WheelEvent{DX: dx, DY: dy})
}

func (s *ClientSession) SendKey(kind string, keyCode int, text string, modifiers uint32) error {
	return s.sendInput(FrameInputKey, KeyEvent{Kind: kind, KeyCode: keyCode, Text: text, Modifiers: modifiers})
}

func (s *ClientSession) sendInput(kind FrameType, value any) error {
	f, err := jsonFrame(kind, value)
	if err == nil {
		err = validateInput(f)
	}
	if err != nil {
		return err
	}
	return s.send(f)
}

func (s *ClientSession) SetQuality(quality string) error {
	preset, err := Quality(quality)
	if err != nil {
		return err
	}
	f, err := jsonFrame(FrameHello, hello{Token: s.token, Quality: preset.Name})
	if err != nil {
		return err
	}
	return s.send(f)
}

func (s *ClientSession) send(f Frame) error {
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	return s.stream.send(f)
}

func (s *ClientSession) Done() <-chan struct{} { return s.done }

func (s *ClientSession) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

func (s *ClientSession) Close() error {
	s.shutdown(nil)
	s.wg.Wait()
	return nil
}

func (s *ClientSession) shutdown(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		s.cancel()
		_ = s.stream.Close()
		close(s.done)
	})
}

func (s *ClientSession) readLoop() {
	defer s.wg.Done()
	for {
		f, err := ReadFrame(s.stream)
		if err != nil {
			s.shutdown(err)
			return
		}
		switch f.Type {
		case FrameVideoJPEG:
			if err := s.decodeJPEG(f.Payload); err != nil {
				s.shutdown(err)
				return
			}
			s.signalFrame()
		case FrameDisplayInfo:
			var display DisplayInfo
			if len(f.Payload) > 4096 || json.Unmarshal(f.Payload, &display) != nil {
				s.shutdown(errors.New("remote desktop: invalid display metadata"))
				return
			}
			s.mu.Lock()
			s.display = display
			s.mu.Unlock()
		case FramePong:
			if len(f.Payload) == 8 {
				sent := int64(binary.BigEndian.Uint64(f.Payload))
				s.mu.Lock()
				if sent == s.ping && sent > 0 {
					s.stats.RTTMs = float64(time.Since(time.Unix(0, sent))) / float64(time.Millisecond)
				}
				s.mu.Unlock()
			}
		default:
			s.shutdown(fmt.Errorf("remote desktop: unexpected frame type %d", f.Type))
			return
		}
	}
}

func (s *ClientSession) decodeJPEG(data []byte) error {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 16384 || cfg.Height > 16384 || int64(cfg.Width)*int64(cfg.Height) > 32<<20 {
		return errors.New("remote desktop: JPEG dimensions exceed supported size")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bounds := image.Rect(0, 0, cfg.Width, cfg.Height)
	if s.back == nil || s.back.Rect != bounds {
		s.back = image.NewRGBA(bounds)
	}
	draw.Draw(s.back, bounds, decoded, decoded.Bounds().Min, draw.Src)
	s.front, s.back = s.back, s.front
	s.seq++
	s.bytes += uint64(len(data) + 5)
	s.stats.Width, s.stats.Height = cfg.Width, cfg.Height
	return nil
}

func (s *ClientSession) pingLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		now := time.Now().UnixNano()
		s.mu.Lock()
		s.ping = now
		s.mu.Unlock()
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], uint64(now))
		if err := s.send(Frame{Type: FramePing, Payload: payload[:]}); err != nil {
			s.shutdown(err)
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *ClientSession) signalFrame() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *ClientSession) callbackLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.notify:
			s.mu.RLock()
			fn := s.callback
			s.mu.RUnlock()
			if fn != nil && s.ctx.Err() == nil {
				fn()
			}
		}
	}
}
