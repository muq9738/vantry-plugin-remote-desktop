package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type inputMessage struct {
	Type      string  `json:"type"`
	Seq       uint64  `json:"seq,omitempty"`
	ButtonSeq uint64  `json:"buttonSeq,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	DX        float64 `json:"dx,omitempty"`
	DY        float64 `json:"dy,omitempty"`
	Button    int     `json:"button,omitempty"`
	KeyCode   int     `json:"keyCode,omitempty"`
	Text      string  `json:"text,omitempty"`
	Modifiers uint32  `json:"modifiers,omitempty"`
	Quality   string  `json:"quality,omitempty"`
	T0        int64   `json:"t0,omitempty"`
	T1        int64   `json:"t1,omitempty"`
	T2        int64   `json:"t2,omitempty"`
}

type peerCallbacks struct {
	connected func()
	ended     func(error)
	track     func(*webrtc.TrackRemote, *webrtc.RTPReceiver)
	input     func(inputMessage)
	bitrate   func(int)
}

type peer struct {
	ctx              context.Context
	cancel           context.CancelFunc
	pc               *webrtc.PeerConnection
	signal           *SignalConn
	role             string
	info             SessionInfo
	relay            Relay
	token            string
	cb               peerCallbacks
	mu               sync.Mutex
	control          *webrtc.DataChannel
	motion           *webrtc.DataChannel
	pendingICE       []webrtc.ICECandidateInit
	video            *webrtc.TrackLocalStaticRTP
	sender           *webrtc.RTPSender
	connected        chan struct{}
	inputsReady      chan struct{}
	inputsOnce       sync.Once
	readyOnce        sync.Once
	closeOnce        sync.Once
	offerMu          sync.Mutex
	offerOutstanding bool
	errMu            sync.Mutex
	err              error
}

func newPeer(ctx context.Context, info SessionInfo, role, token string, relay Relay, signal *SignalConn, callbacks peerCallbacks) (*peer, error) {
	if err := info.validateICE(); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	p := &peer{ctx: life, cancel: cancel, info: info, role: role, token: token, relay: relay, signal: signal, cb: callbacks, connected: make(chan struct{}), inputsReady: make(chan struct{})}
	media := &webrtc.MediaEngine{}
	profile := info.Video.ProfileLevelID
	if profile == "" {
		profile = h264HighProfileLevelID
	}
	if len(profile) != 6 {
		cancel()
		return nil, errors.New("rtc: invalid H.264 profile")
	}
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile, RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}, {Type: "transport-cc"}}}
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: codec, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
		cancel()
		return nil, err
	}
	if err := media.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: CaptureTimeURI}, webrtc.RTPCodecTypeVideo); err != nil {
		cancel()
		return nil, err
	}
	registry := &interceptor.Registry{}
	initial := info.Video.Bitrate
	if initial < 250000 {
		initial = 4000000
	}
	factory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(initial), gcc.SendSideBWEMinBitrate(250000), gcc.SendSideBWEMaxBitrate(12000000))
	})
	if err != nil {
		cancel()
		return nil, err
	}
	factory.OnNewPeerConnection(func(_ string, estimator cc.BandwidthEstimator) {
		estimator.OnTargetBitrateChange(func(rate int) {
			if callbacks.bitrate != nil {
				callbacks.bitrate(rate)
			}
		})
	})
	registry.Add(factory)
	if err = webrtc.RegisterDefaultInterceptors(media, registry); err != nil {
		cancel()
		return nil, err
	}
	if err = webrtc.ConfigureTWCCSender(media, registry); err != nil {
		cancel()
		return nil, err
	}
	if err = webrtc.ConfigureTWCCHeaderExtensionSender(media, registry); err != nil {
		cancel()
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	policy := webrtc.ICETransportPolicyRelay
	if info.Transport == "lan" {
		policy = webrtc.ICETransportPolicyAll
		settings.SetIPFilter(lanIP)
	} else {
		settings.SetRelayAcceptanceMinWait(0)
	}
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings), webrtc.WithMediaEngine(media), webrtc.WithInterceptorRegistry(registry)).NewPeerConnection(webrtc.Configuration{ICEServers: info.ICEServers, ICETransportPolicy: policy, BundlePolicy: webrtc.BundlePolicyMaxBundle})
	if err != nil {
		cancel()
		return nil, err
	}
	p.pc = pc
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		var candidate *webrtc.ICECandidateInit
		if c != nil {
			v := c.ToJSON()
			candidate = &v
		}
		if err := signal.Send(life, SignalMessage{Type: "ice", Candidate: candidate}); err != nil && life.Err() == nil {
			p.fail(errors.New("rtc: ICE signaling failed"))
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			if life.Err() != nil {
				return
			}
			p.readyOnce.Do(func() {
				close(p.connected)
				if p.cb.connected != nil {
					p.cb.connected()
				}
			})
		case webrtc.PeerConnectionStateFailed:
			p.fail(errors.New("rtc: WebRTC connection failed"))
		case webrtc.PeerConnectionStateClosed:
			if life.Err() == nil {
				p.fail(ErrClosed)
			}
		}
	})
	if callbacks.track != nil {
		pc.OnTrack(callbacks.track)
	}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) { p.bindChannel(dc) })
	if role == "host" {
		track, err := webrtc.NewTrackLocalStaticRTP(codec, "desktop-video", "vantry-desktop")
		if err != nil {
			p.Close()
			return nil, err
		}
		p.video = track
		sender, err := pc.AddTrack(track)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.sender = sender
		reliable, err := pc.CreateDataChannel("control", nil)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.bindChannel(reliable)
		ordered := false
		retransmits := uint16(0)
		motion, err := pc.CreateDataChannel("motion", &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retransmits})
		if err != nil {
			p.Close()
			return nil, err
		}
		p.bindChannel(motion)
	} else {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			p.Close()
			return nil, err
		}
	}
	return p, nil
}
func (p *peer) bindChannel(dc *webrtc.DataChannel) {
	p.mu.Lock()
	switch dc.Label() {
	case "control":
		p.control = dc
	case "motion":
		p.motion = dc
	default:
		p.mu.Unlock()
		_ = dc.Close()
		return
	}
	p.mu.Unlock()
	dc.OnOpen(p.checkInputsReady)
	p.checkInputsReady()
	dc.OnMessage(func(v webrtc.DataChannelMessage) {
		if len(v.Data) > 16384 {
			return
		}
		var m inputMessage
		if json.Unmarshal(v.Data, &m) != nil {
			return
		}
		if p.cb.input != nil {
			p.cb.input(m)
		}
	})
}
func (p *peer) checkInputsReady() {
	p.mu.Lock()
	ready := p.control != nil && p.motion != nil && p.control.ReadyState() == webrtc.DataChannelStateOpen && p.motion.ReadyState() == webrtc.DataChannelStateOpen
	p.mu.Unlock()
	if ready {
		p.inputsOnce.Do(func() { close(p.inputsReady) })
	}
}
func (p *peer) waitInputs(ctx context.Context) error {
	select {
	case <-p.inputsReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	case <-time.After(10 * time.Second):
		return errors.New("rtc: input channel negotiation timed out")
	}
}
func (p *peer) sendInput(m inputMessage, motion bool) error {
	p.mu.Lock()
	dc := p.control
	if motion {
		dc = p.motion
	}
	p.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return errors.New("rtc: input channel is not ready")
	}
	if dc.BufferedAmount() > 64<<10 {
		if motion {
			return nil
		}
		return errors.New("rtc: control channel is congested")
	}
	return dc.SendText(string(jsonData(m)))
}
func (p *peer) offer(restart bool) error {
	p.offerMu.Lock()
	defer p.offerMu.Unlock()
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	if p.offerOutstanding {
		if restart {
			return errors.New("rtc: negotiation is still pending")
		}
		return nil
	}
	offer, err := p.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: restart})
	if err != nil {
		return err
	}
	if err = p.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	p.offerOutstanding = true
	return p.signal.Send(p.ctx, SignalMessage{Type: "offer", SDP: offer.SDP})
}
func (p *peer) remoteDescription(desc webrtc.SessionDescription) error {
	if err := p.pc.SetRemoteDescription(desc); err != nil {
		return err
	}
	p.mu.Lock()
	pending := p.pendingICE
	p.pendingICE = nil
	p.mu.Unlock()
	for _, c := range pending {
		if err := p.pc.AddICECandidate(c); err != nil {
			return err
		}
	}
	return nil
}
func (p *peer) handle(m SignalMessage) error {
	switch m.Type {
	case "peer":
		if m.Connected && p.role == "host" {
			return p.offer(false)
		}
	case "offer":
		if p.role != "viewer" {
			return errors.New("rtc: unexpected offer")
		}
		if err := p.remoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}); err != nil {
			return err
		}
		answer, err := p.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err = p.pc.SetLocalDescription(answer); err != nil {
			return err
		}
		return p.signal.Send(p.ctx, SignalMessage{Type: "answer", SDP: answer.SDP})
	case "answer":
		if p.role != "host" {
			return errors.New("rtc: unexpected answer")
		}
		if err := p.remoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
			return err
		}
		p.offerMu.Lock()
		p.offerOutstanding = false
		p.offerMu.Unlock()
	case "ice":
		candidate := webrtc.ICECandidateInit{}
		if m.Candidate != nil {
			candidate = *m.Candidate
		}
		p.mu.Lock()
		if p.pc.RemoteDescription() == nil {
			if len(p.pendingICE) >= 128 {
				p.mu.Unlock()
				return errors.New("rtc: too many pending ICE candidates")
			}
			p.pendingICE = append(p.pendingICE, candidate)
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()
		return p.pc.AddICECandidate(candidate)
	case "bye":
		return ErrClosed
	}
	return nil
}
func (p *peer) renew() error {
	info, err := p.relay.Get(p.ctx, p.info.SessionID, p.token)
	if err != nil {
		return err
	}
	cfg := p.pc.GetConfiguration()
	cfg.ICEServers = info.ICEServers
	if err = p.pc.SetConfiguration(cfg); err != nil {
		return err
	}
	if p.role == "host" {
		err = p.offer(true)
	} else {
		err = p.sendInput(inputMessage{Type: "restart"}, false)
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.info.ExpiresAt = info.ExpiresAt
	p.mu.Unlock()
	return nil
}
func (p *peer) renewLoop() {
	if p.info.Transport == "lan" {
		<-p.ctx.Done()
		return
	}
	for {
		p.mu.Lock()
		expires := p.info.ExpiresAt
		p.mu.Unlock()
		delay := time.Until(expires) - time.Minute
		if expires.IsZero() {
			delay = 8 * time.Minute
		}
		if delay < 5*time.Second {
			delay = 5 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-p.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		for p.renew() != nil {
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}
func (p *peer) fail(err error) {
	p.errMu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.errMu.Unlock()
	if p.cb.ended != nil {
		p.cb.ended(err)
	}
}
func (p *peer) Err() error { p.errMu.Lock(); defer p.errMu.Unlock(); return p.err }
func (p *peer) Close() {
	p.closeOnce.Do(func() {
		p.cancel()
		if p.pc != nil {
			_ = p.pc.Close()
		}
	})
}
func (p *peer) waitReady(ctx context.Context) error {
	select {
	case <-p.connected:
		return nil
	case <-p.ctx.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(25 * time.Second):
		return errors.New("rtc: WebRTC connection negotiation timed out")
	}
}
func (p *peer) requestKeyFrame(ssrc uint32) {
	_ = p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
}
func (p *peer) candidateStats(s *Stats) {
	report := p.pc.GetStats()
	for _, v := range report {
		pair, ok := v.(webrtc.ICECandidatePairStats)
		if !ok || !pair.Nominated || pair.State != webrtc.StatsICECandidatePairStateSucceeded {
			continue
		}
		if local, ok := report[pair.LocalCandidateID].(webrtc.ICECandidateStats); ok {
			s.LocalCandidateType = local.CandidateType.String()
			s.CandidateProtocol = local.Protocol
		}
		if remote, ok := report[pair.RemoteCandidateID].(webrtc.ICECandidateStats); ok {
			s.RemoteCandidateType = remote.CandidateType.String()
		}
		if s.RTTMs == 0 {
			s.RTTMs = pair.CurrentRoundTripTime * 1000
		}
		return
	}
}
func validateVideo(v VideoConfig) error {
	if v.Width < 16 || v.Width > 8192 || v.Height < 16 || v.Height > 8192 || v.FPS < 1 || v.FPS > 120 || v.Bitrate < 100000 || v.Bitrate > 50000000 {
		return fmt.Errorf("rtc: invalid video configuration")
	}
	return nil
}
