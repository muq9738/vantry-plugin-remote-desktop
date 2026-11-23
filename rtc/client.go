package rtc

import (
	"context"
	"errors"
	"image"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

type ClientSession struct {
	ctx         context.Context
	cancel      context.CancelFunc
	p           *peer
	signal      *SignalConn
	relay       Relay
	ownedRelay  bool
	decoder     FrameDecoder
	mu          sync.Mutex
	frame       DecodedFrame
	leased      DecodedFrame
	presented   uint64
	seq         uint64
	stats       Stats
	started     time.Time
	ages        []float64
	ageIndex    int
	offset      time.Duration
	bestRTT     time.Duration
	clockAt     time.Time
	callback    func()
	wake        chan struct{}
	bytes       atomic.Uint64
	inputSeq    atomic.Uint64
	mouseSendMu sync.Mutex
	buttonSeq   atomic.Uint64
	ssrc        atomic.Uint32
	err         error
	closeOnce   sync.Once
	wg          sync.WaitGroup
}

func Dial(ctx context.Context, cfg ConnectConfig) (*ClientSession, error) {
	dialStarted := time.Now()
	if ctx == nil {
		return nil, errors.New("rtc: viewer context is required")
	}
	var ticket Ticket
	var err error
	relay := cfg.Relay
	if strings.HasPrefix(strings.TrimSpace(cfg.Ticket), "vantry-lan:") {
		lan, parseErr := ParseLANTicket(cfg.Ticket)
		if parseErr != nil {
			return nil, parseErr
		}
		ticket.SessionID, ticket.Token = lan.SessionID, lan.Token
		if relay == nil {
			relay, err = newLANClient(lan)
		}
	} else {
		ticket, err = ParseTicket(cfg.Ticket)
		if err == nil && relay == nil {
			host := strings.TrimSpace(cfg.SSHHost)
			if host == "" {
				host = ticket.SSHHost
			}
			relay, err = NewSSHRelay(SSHConfig{Host: host})
		}
	}
	if err != nil {
		return nil, err
	}
	established := false
	defer func() {
		if !established && cfg.Relay == nil {
			if owner, ok := relay.(interface{ Close() }); ok {
				owner.Close()
			}
		}
	}()
	info, err := relay.Get(ctx, ticket.SessionID, ticket.Token)
	if err != nil {
		return nil, err
	}
	if err = validateSession(info, false); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	mode, relayHost := "relay-webrtc", ticket.SSHHost
	if cfg.SSHHost != "" {
		relayHost = cfg.SSHHost
	}
	if info.Transport == "lan" {
		mode, relayHost = "direct-webrtc", ""
	}
	c := &ClientSession{ctx: life, cancel: cancel, relay: relay, started: dialStarted, wake: make(chan struct{}, 1), ownedRelay: cfg.Relay == nil, stats: Stats{Mode: mode, Codec: "h264", RelayHost: relayHost, Transport: info.Transport}}
	factory := cfg.DecoderFactory
	if factory == nil {
		factory = func(ctx context.Context, onFrame func(DecodedFrame)) (FrameDecoder, error) {
			return NewNativeDecoder(ctx, cfg.HelperPath, onFrame)
		}
	}
	c.decoder, err = factory(life, c.onDecoded)
	if err != nil {
		cancel()
		return nil, err
	}
	c.stats.Hardware = cfg.DecoderFactory == nil
	signal, err := relay.Signal(life, ticket.SessionID, "viewer", ticket.Token)
	if err != nil {
		_ = c.decoder.Close()
		cancel()
		return nil, err
	}
	c.signal = signal
	callbacks := peerCallbacks{
		connected: func() { c.mu.Lock(); c.stats.Connected = true; c.mu.Unlock() },
		ended:     func(err error) { c.setError(err); go c.Close() },
		track:     c.receive,
		input:     c.handleInput,
	}
	c.p, err = newPeer(life, info, "viewer", ticket.Token, relay, signal, callbacks)
	if err != nil {
		_ = signal.Close()
		_ = c.decoder.Close()
		cancel()
		return nil, err
	}
	if native, ok := c.decoder.(interface{ SetKeyFrameRequest(func()) }); ok {
		native.SetKeyFrameRequest(c.requestKeyFrame)
	}
	c.wg.Add(3)
	go c.signaling()
	go c.tick()
	go c.notify()
	ready, cancelReady := context.WithTimeout(life, 30*time.Second)
	defer cancelReady()
	if err = c.p.waitReady(ready); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err = c.p.waitInputs(ready); err != nil {
		_ = c.Close()
		return nil, err
	}
	c.wg.Add(1)
	go func() { defer c.wg.Done(); c.p.renewLoop() }()
	context.AfterFunc(life, func() { _ = c.Close() })
	established = true
	return c, nil
}
func (c *ClientSession) signaling() {
	defer c.wg.Done()
	for {
		m, err := c.signal.Read()
		if err != nil {
			if c.ctx.Err() == nil {
				c.setError(errors.New("rtc: signaling disconnected"))
				go c.Close()
			}
			return
		}
		if m.Type == "bye" || (m.Type == "peer" && !m.Connected) {
			c.setError(errors.New("rtc: remote desktop host disconnected"))
			go c.Close()
			return
		}
		if err = c.p.handle(m); err != nil {
			c.setError(err)
			go c.Close()
			return
		}
	}
}
func (c *ClientSession) receive(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	if track.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}
	c.ssrc.Store(uint32(track.SSRC()))
	extID := uint8(0)
	for _, ext := range receiver.GetParameters().HeaderExtensions {
		if ext.URI == CaptureTimeURI {
			extID = uint8(ext.ID)
		}
	}
	assembler := newAccessUnits(extID)
	waitingKey := true
	lastPLI := time.Time{}
	request := func() {
		if time.Since(lastPLI) > 200*time.Millisecond {
			lastPLI = time.Now()
			c.requestKeyFrame()
		}
	}
	request()
	for {
		_ = track.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		packet, _, err := track.ReadRTP()
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				if assembler.expire(time.Now()) {
					waitingKey = true
					c.mu.Lock()
					c.stats.Dropped++
					c.mu.Unlock()
				}
				if waitingKey {
					request()
				}
				if c.ctx.Err() == nil {
					continue
				}
			}
			return
		}
		c.bytes.Add(uint64(len(packet.Payload)))
		frame, needKey, err := assembler.push(packet, time.Now())
		if err != nil {
			c.setError(err)
			request()
			waitingKey = true
			continue
		}
		if needKey {
			waitingKey = true
			request()
		}
		if frame == nil {
			continue
		}
		if waitingKey && !frame.KeyFrame {
			request()
			continue
		}
		if frame.KeyFrame {
			waitingKey = false
		}
		if err = c.decoder.Decode(c.ctx, *frame); err != nil {
			if c.ctx.Err() == nil {
				c.setError(err)
			}
			waitingKey = true
			request()
		}
	}
}
func (c *ClientSession) requestKeyFrame() {
	if c.p != nil {
		if ssrc := c.ssrc.Load(); ssrc != 0 {
			c.p.requestKeyFrame(ssrc)
		}
	}
}
func (c *ClientSession) onDecoded(frame DecodedFrame) {
	c.mu.Lock()
	if frame.Dropped {
		c.stats.Dropped++
		c.mu.Unlock()
		return
	}
	if frame.Image == nil {
		c.mu.Unlock()
		return
	}
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		if frame.Release != nil {
			frame.Release()
		}
		return
	}
	if c.frame.Image != nil && c.frame.Image != c.leased.Image && c.frame.Release != nil {
		c.frame.Release()
	}
	c.frame = frame
	c.seq++
	c.stats.Frames = c.seq
	c.stats.Width = frame.Image.Bounds().Dx()
	c.stats.Height = frame.Image.Bounds().Dy()
	if c.stats.FirstFrameMs == 0 {
		c.stats.FirstFrameMs = float64(time.Since(c.started).Microseconds()) / 1000
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *ClientSession) handleInput(m inputMessage) {
	if m.Type != "pong" || m.T0 <= 0 || m.T1 <= 0 || m.T2 < m.T1 {
		return
	}
	now := time.Now().UnixMicro()
	rtt := time.Duration((now-m.T0)-(m.T2-m.T1)) * time.Microsecond
	if rtt < 0 || rtt > 5*time.Second {
		return
	}
	offset := time.Duration(((m.T1-m.T0)+(m.T2-now))/2) * time.Microsecond
	c.mu.Lock()
	if c.bestRTT == 0 || rtt < c.bestRTT || time.Since(c.clockAt) > time.Minute {
		c.bestRTT = rtt
		c.offset = offset
		c.clockAt = time.Now()
		c.stats.ClockOffsetMs = float64(offset.Microseconds()) / 1000
		c.stats.ClockSynchronized = true
	}
	c.stats.RTTMs = float64(rtt.Microseconds()) / 1000
	c.mu.Unlock()
}
func (c *ClientSession) tick() {
	defer c.wg.Done()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
			_ = c.p.sendInput(inputMessage{Type: "ping", T0: time.Now().UnixMicro()}, false)
			c.mu.Lock()
			s := c.stats
			c.mu.Unlock()
			c.p.candidateStats(&s)
			c.mu.Lock()
			c.stats.LocalCandidateType = s.LocalCandidateType
			c.stats.RemoteCandidateType = s.RemoteCandidateType
			c.stats.CandidateProtocol = s.CandidateProtocol
			c.mu.Unlock()
		}
	}
}
func (c *ClientSession) notify() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
			c.mu.Lock()
			fn := c.callback
			c.mu.Unlock()
			if fn != nil {
				fn()
			}
		}
	}
}
func (c *ClientSession) OnFrame(fn func()) { c.mu.Lock(); c.callback = fn; c.mu.Unlock() }

// LatestFrame returns the same immutable image when no new decoder frame exists;
// polling a static desktop never copies a full RGBA frame.
func (c *ClientSession) LatestFrame() (*image.RGBA, uint64, Stats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leased.Image != c.frame.Image {
		if c.leased.Release != nil {
			c.leased.Release()
		}
		c.leased = c.frame
	}
	if c.stats.ClockSynchronized && !c.frame.CapturedAt.IsZero() && c.presented != c.seq {
		c.presented = c.seq
		age := float64((time.Since(c.frame.CapturedAt) + c.offset).Microseconds()) / 1000
		if age >= 0 && age < 10000 {
			c.stats.FrameAgeMs = age
			if len(c.ages) < 300 {
				c.ages = append(c.ages, age)
			} else {
				c.ages[c.ageIndex%300] = age
				c.ageIndex++
			}
			sorted := append([]float64(nil), c.ages...)
			sort.Float64s(sorted)
			c.stats.FrameAgeP50Ms = sorted[len(sorted)/2]
			c.stats.FrameAgeP95Ms = sorted[(len(sorted)-1)*95/100]
		}
	}
	s := c.stats
	s.Connected = c.ctx.Err() == nil && s.Connected
	elapsed := time.Since(c.started).Seconds()
	if elapsed > 0 {
		s.FPS = float64(c.seq) / elapsed
		s.Kbps = float64(c.bytes.Load()) * 8 / elapsed / 1000
	}
	if c.err != nil {
		s.Error = c.err.Error()
	}
	return c.leased.Image, c.seq, s
}
func (c *ClientSession) SendMouse(kind string, x, y float64, button int) error {
	c.mouseSendMu.Lock()
	defer c.mouseSendMu.Unlock()
	motion := kind == "move" || kind == "drag"
	seq := c.inputSeq.Add(1)
	message := inputMessage{Type: "mouse", Seq: seq, ButtonSeq: c.buttonSeq.Load(), Kind: kind, X: x, Y: y, Button: button}
	err := c.p.sendInput(message, motion)
	if err == nil && !motion {
		c.buttonSeq.Store(seq)
	}
	if err != nil && !motion {
		c.setError(err)
		go c.Close()
	}
	return err
}
func (c *ClientSession) SendWheel(dx, dy float64) error {
	return c.p.sendInput(inputMessage{Type: "wheel", Seq: c.inputSeq.Add(1), DX: dx, DY: dy}, true)
}
func (c *ClientSession) SendKey(kind string, code int, text string, modifiers uint32) error {
	err := c.p.sendInput(inputMessage{Type: "key", Seq: c.inputSeq.Add(1), Kind: kind, KeyCode: code, Text: text, Modifiers: modifiers}, false)
	if err != nil {
		c.setError(err)
		go c.Close()
	}
	return err
}
func (c *ClientSession) SetQuality(quality string) error {
	if _, err := Quality(quality); err != nil {
		return err
	}
	return c.p.sendInput(inputMessage{Type: "quality", Quality: quality}, false)
}
func (c *ClientSession) setError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}
func (c *ClientSession) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *ClientSession) Done() <-chan struct{} { return c.ctx.Done() }
func (c *ClientSession) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.signal != nil {
			_ = c.signal.Close()
		}
		if c.p != nil {
			c.p.Close()
		}
		if c.decoder != nil {
			_ = c.decoder.Close()
		}
		c.mu.Lock()
		if c.frame.Image != c.leased.Image && c.frame.Release != nil {
			c.frame.Release()
		}
		if c.leased.Release != nil {
			c.leased.Release()
		}
		c.frame = DecodedFrame{}
		c.leased = DecodedFrame{}
		c.mu.Unlock()
		if closer, ok := c.relay.(interface{ Close() }); ok && c.ownedRelay {
			closer.Close()
		}
	})
	return nil
}
