package rtc

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	root "github.com/muq9738/vantry-plugin-remote-desktop"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

type Host struct {
	cfg           HostConfig
	ctx           context.Context
	cancel        context.CancelFunc
	info          SessionInfo
	signal        *SignalConn
	relay         Relay
	ownedRelay    bool
	mu            sync.Mutex
	current       *peer
	source        EncodedSource
	requested     VideoConfig
	quality       chan VideoConfig
	sourceMu      sync.Mutex
	inputMu       sync.Mutex
	keys          map[int]uint32
	buttons       map[int]inputMessage
	lastMouse     inputMessage
	lastMouseSeq  uint64
	lastButtonSeq uint64
	pendingMotion *inputMessage
	err           error
	once          sync.Once
	wg            sync.WaitGroup
}

func StartHost(ctx context.Context, cfg HostConfig) (*Host, error) {
	if ctx == nil {
		return nil, errors.New("rtc: host context is required")
	}
	video, err := Quality(cfg.Quality)
	if err != nil {
		return nil, err
	}
	video.DisplayID = cfg.DisplayID
	if cfg.SourceFactory == nil {
		info, err := ProbeNative(ctx, cfg.HelperPath)
		if err != nil {
			return nil, err
		}
		if info.ScreenCaptureAccess != "granted" {
			return nil, &NativePermissionError{"请在系统设置的隐私与安全性中授予 Vantry 屏幕录制权限，再重新开启被控。"}
		}
		cfg.SourceFactory = func(ctx context.Context, v VideoConfig) (EncodedSource, error) {
			return NewNativeSource(ctx, cfg.HelperPath, v, 0)
		}
	}
	if cfg.InputSink == nil {
		cfg.InputSink, err = root.NewInputSink()
		if err != nil {
			return nil, err
		}
	}
	relay := cfg.Relay
	if relay == nil {
		if strings.TrimSpace(cfg.SSHHost) == "" {
			relay, err = newLANServer(cfg.LANListenAddr)
		} else {
			relay, err = NewSSHRelay(SSHConfig{Host: cfg.SSHHost})
		}
		if err != nil {
			return nil, err
		}
	}
	established := false
	defer func() {
		if !established && cfg.Relay == nil {
			if owner, ok := relay.(interface{ Close() }); ok {
				owner.Close()
			}
		}
	}()
	info, err := relay.Create(ctx, video)
	if err != nil {
		return nil, err
	}
	info.Video = video
	signal, err := relay.Signal(ctx, info.SessionID, "host", info.HostToken)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = relay.Delete(cleanup, info.SessionID, info.HostToken)
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	h := &Host{cfg: cfg, ownedRelay: cfg.Relay == nil, ctx: life, cancel: cancel, info: info, signal: signal, relay: relay, requested: video, quality: make(chan VideoConfig, 1), keys: map[int]uint32{}, buttons: map[int]inputMessage{}}
	h.wg.Add(1)
	go h.run()
	context.AfterFunc(life, func() { _ = h.Close() })
	established = true
	return h, nil
}
func (h *Host) Ticket() string {
	if lan, ok := h.relay.(*LANRelay); ok {
		return lan.Ticket(h.info.SessionID, h.info.ViewerToken)
	}
	return (Ticket{h.cfg.SSHHost, h.info.SessionID, h.info.ViewerToken}).String()
}
func (h *Host) DirectAddrs() []string {
	if lan, ok := h.relay.(*LANRelay); ok {
		return append([]string(nil), lan.addrs...)
	}
	return nil
}
func (h *Host) ListenAddr() string {
	if lan, ok := h.relay.(*LANRelay); ok && lan.listener != nil {
		return lan.listener.Addr().String()
	}
	return ""
}
func (h *Host) SessionID() string { return h.info.SessionID }
func (h *Host) Err() error        { h.mu.Lock(); defer h.mu.Unlock(); return h.err }
func (h *Host) Connected() bool {
	h.mu.Lock()
	p := h.current
	h.mu.Unlock()
	if p == nil {
		return false
	}
	return p.pc.ConnectionState() == webrtc.PeerConnectionStateConnected
}
func (h *Host) run() {
	defer h.wg.Done()
	for {
		message, err := h.signal.Read()
		if err != nil {
			if h.ctx.Err() == nil {
				h.record(errors.New("rtc: signaling disconnected"))
				h.cancel()
			}
			return
		}
		if message.Type == "peer" {
			if !message.Connected {
				h.closePeer()
				continue
			}
			h.mu.Lock()
			p := h.current
			h.mu.Unlock()
			if p == nil {
				p, err = h.createPeer()
				if err != nil {
					h.record(err)
					continue
				}
			}
		}
		h.mu.Lock()
		p := h.current
		h.mu.Unlock()
		if p == nil {
			continue
		}
		if err = p.handle(message); err != nil {
			h.record(err)
			h.closePeer()
		}
	}
}
func (h *Host) createPeer() (*peer, error) {
	info, err := h.relay.Get(h.ctx, h.info.SessionID, h.info.HostToken)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current != nil {
		return h.current, nil
	}
	info.Video = h.requested
	var p *peer
	callbacks := peerCallbacks{
		connected: func() {
			if p.ctx.Err() != nil {
				return
			}
			h.wg.Add(2)
			go func() { defer h.wg.Done(); h.capture(p) }()
			go func() { defer h.wg.Done(); h.readRTCP(p) }()
		},
		input: func(m inputMessage) { h.handleInput(p, m) },
		bitrate: func(rate int) {
			h.mu.Lock()
			maximum := h.requested.Bitrate
			h.mu.Unlock()
			if rate > maximum {
				rate = maximum
			}
			h.sourceMu.Lock()
			source := h.source
			h.sourceMu.Unlock()
			if source != nil {
				_ = source.SetBitrate(rate)
			}
		},
		ended: func(err error) { h.record(err); go h.closeSpecific(p) },
	}
	p, err = newPeer(h.ctx, info, "host", h.info.HostToken, h.relay, h.signal, callbacks)
	if err != nil {
		return nil, err
	}
	h.current = p
	h.wg.Add(1)
	go func() { defer h.wg.Done(); p.renewLoop() }()
	return p, nil
}
func (h *Host) record(err error) {
	if err == nil || h.ctx.Err() != nil {
		return
	}
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
}
func (h *Host) closeSpecific(expected *peer) {
	h.mu.Lock()
	p := h.current
	if p != expected {
		h.mu.Unlock()
		return
	}
	h.current = nil
	h.mu.Unlock()
	if p != nil {
		p.Close()
	}
	h.stopSource()
	h.releaseInputs()
}
func (h *Host) closePeer() {
	h.mu.Lock()
	p := h.current
	h.current = nil
	h.mu.Unlock()
	if p != nil {
		p.Close()
	}
	h.stopSource()
	h.releaseInputs()
}
func (h *Host) stopSource() {
	h.sourceMu.Lock()
	source := h.source
	h.source = nil
	h.sourceMu.Unlock()
	if source != nil {
		_ = source.Close()
	}
}
func (h *Host) readRTCP(p *peer) {
	for {
		packets, _, err := p.sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, packet := range packets {
			switch packet.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				h.sourceMu.Lock()
				s := h.source
				h.sourceMu.Unlock()
				if s != nil {
					_ = s.RequestKeyFrame()
				}
			}
		}
	}
}
func (h *Host) capture(p *peer) {
	packetizer := rtp.NewPacketizer(1150, 102, 1, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	started := time.Now()
	var baseTimestamp uint32
	firstPacket := true
	for {
		if p.ctx.Err() != nil {
			return
		}
		h.mu.Lock()
		v := h.requested
		h.mu.Unlock()
		source, err := h.cfg.SourceFactory(p.ctx, v)
		if err != nil {
			h.record(err)
			go h.closeSpecific(p)
			return
		}
		h.sourceMu.Lock()
		if p.ctx.Err() != nil {
			h.sourceMu.Unlock()
			_ = source.Close()
			return
		}
		h.source = source
		h.sourceMu.Unlock()
		_ = source.RequestKeyFrame()
		restart := false
		for {
			if p.ctx.Err() != nil {
				_ = source.Close()
				return
			}
			frame, err := source.Next(p.ctx)
			if err != nil {
				select {
				case <-h.quality:
					restart = true
				default:
				}
				if !restart && p.ctx.Err() == nil && !errors.Is(err, io.EOF) {
					h.record(err)
				}
				break
			}
			extensionID := uint8(0)
			for _, ext := range p.sender.GetParameters().HeaderExtensions {
				if ext.URI == CaptureTimeURI {
					extensionID = uint8(ext.ID)
					break
				}
			}
			extension, _ := rtp.NewAbsCaptureTimeExtension(frame.CapturedAt).Marshal()
			packets := packetizer.Packetize(frame.Data, uint32(90000/v.FPS))
			if len(packets) > 0 && firstPacket {
				baseTimestamp = packets[0].Timestamp
				firstPacket = false
			}
			timestamp := baseTimestamp + uint32(time.Since(started).Seconds()*90000)
			for _, packet := range packets {
				packet.Timestamp = timestamp
				if extensionID != 0 {
					_ = packet.SetExtension(extensionID, extension)
				}
				if err = p.video.WriteRTP(packet); err != nil {
					h.record(err)
					break
				}
			}
			if err != nil {
				break
			}
			select {
			case <-h.quality:
				restart = true
			default:
			}
			if restart {
				break
			}
		}
		_ = source.Close()
		h.sourceMu.Lock()
		if h.source == source {
			h.source = nil
		}
		h.sourceMu.Unlock()
		if !restart {
			if p.ctx.Err() == nil {
				h.record(errors.New("rtc: video source stopped"))
				go h.closeSpecific(p)
			}
			return
		}
	}
}
func (h *Host) handleInput(p *peer, m inputMessage) {
	if p == nil || p.ctx.Err() != nil {
		return
	}
	switch m.Type {
	case "ping":
		_ = p.sendInput(inputMessage{Type: "pong", T0: m.T0, T1: time.Now().UnixMicro(), T2: time.Now().UnixMicro()}, false)
		return
	case "restart":
		go func() {
			if err := p.renew(); err != nil {
				h.record(err)
			}
		}()
		return
	case "quality":
		v, err := Quality(m.Quality)
		if err != nil {
			return
		}
		v.DisplayID = h.cfg.DisplayID
		h.mu.Lock()
		h.requested = v
		h.mu.Unlock()
		select {
		case h.quality <- v:
		default:
		}
		h.stopSource()
		return
	}
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	switch m.Type {
	case "mouse":
		h.applyMouseLocked(p, m)
	case "wheel":
		if math.IsNaN(m.DX) || math.IsNaN(m.DY) || math.Abs(m.DX) > 10000 || math.Abs(m.DY) > 10000 {
			return
		}
		if err := h.cfg.InputSink.Wheel(p.ctx, root.WheelEvent{DX: m.DX, DY: m.DY}); err != nil {
			h.record(err)
		}
	case "key":
		if (m.Kind != "down" && m.Kind != "up") || m.KeyCode < 0 || m.KeyCode > 65535 || len(m.Text) > 8192 {
			return
		}
		if err := h.cfg.InputSink.Key(p.ctx, root.KeyEvent{Kind: m.Kind, KeyCode: m.KeyCode, Text: m.Text, Modifiers: m.Modifiers}); err != nil {
			h.record(err)
			return
		}
		if m.Kind == "down" {
			h.keys[m.KeyCode] = m.Modifiers
		} else {
			delete(h.keys, m.KeyCode)
		}
	}
}

// Button transitions stay on the ordered reliable channel. A motion packet
// cannot pass the most recent button transition it depends on, even when the
// unordered channel wins the network race. Only one latest motion is retained.
func (h *Host) applyMouseLocked(p *peer, m inputMessage) {
	if m.Seq == 0 || m.X < 0 || m.X > 1 || m.Y < 0 || m.Y > 1 || math.IsNaN(m.X) || math.IsNaN(m.Y) || m.Button < 0 || m.Button > 2 {
		return
	}
	motion := m.Kind == "move" || m.Kind == "drag"
	if !motion && m.Kind != "down" && m.Kind != "up" {
		return
	}
	if motion {
		if m.ButtonSeq > h.lastButtonSeq {
			if h.pendingMotion == nil || m.Seq > h.pendingMotion.Seq {
				copy := m
				h.pendingMotion = &copy
			}
			return
		}
		if m.Seq <= h.lastMouseSeq {
			return
		}
	}
	if err := h.cfg.InputSink.Mouse(p.ctx, root.MouseEvent{Kind: m.Kind, X: m.X, Y: m.Y, Button: m.Button, DisplayID: h.cfg.DisplayID}); err != nil {
		h.record(err)
		return
	}
	if m.Seq > h.lastMouseSeq {
		h.lastMouse, h.lastMouseSeq = m, m.Seq
	}
	if m.Kind == "down" {
		h.buttons[m.Button] = m
	} else if m.Kind == "up" {
		delete(h.buttons, m.Button)
	}
	if !motion {
		h.lastButtonSeq = m.Seq
		if next := h.pendingMotion; next != nil && next.ButtonSeq <= h.lastButtonSeq {
			h.pendingMotion = nil
			h.applyMouseLocked(p, *next)
		}
	}
}
func (h *Host) releaseInputs() {
	h.inputMu.Lock()
	defer h.inputMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for code := range h.keys {
		_ = h.cfg.InputSink.Key(ctx, root.KeyEvent{Kind: "up", KeyCode: code, Modifiers: 0})
	}
	for button := range h.buttons {
		_ = h.cfg.InputSink.Mouse(ctx, root.MouseEvent{Kind: "up", Button: button, X: h.lastMouse.X, Y: h.lastMouse.Y, DisplayID: h.cfg.DisplayID})
	}
	if releaser, ok := h.cfg.InputSink.(interface{ Release(context.Context) error }); ok {
		_ = releaser.Release(ctx)
	}
	h.keys = map[int]uint32{}
	h.buttons = map[int]inputMessage{}
	h.lastMouseSeq = 0
	h.lastButtonSeq = 0
	h.pendingMotion = nil
}
func (h *Host) Close() error {
	h.once.Do(func() {
		h.cancel()
		_ = h.signal.Close()
		h.closePeer()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = h.relay.Delete(ctx, h.info.SessionID, h.info.HostToken)
		cancel()
		h.wg.Wait()
		if closer, ok := h.relay.(interface{ Close() }); ok && h.ownedRelay {
			closer.Close()
		}
	})
	return nil
}
