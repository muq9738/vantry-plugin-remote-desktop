package rtc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	root "github.com/muq9738/vantry-plugin-remote-desktop"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

func TestTicketAndPrivateRelayValidation(t *testing.T) {
	token := strings.Repeat("secret", 8)
	want := Ticket{"relay-host", "session_1", token}
	got, err := ParseTicket(want.String())
	if err != nil || got != want {
		t.Fatalf("ticket roundtrip: %v", err)
	}
	for _, raw := range []string{"http://relay-host/session?token=" + token, "vantry-rtc://-option/session?token=" + token, "vantry-rtc://relay-host/bad/path?token=" + token, "vantry-rtc://relay-host/session?token=x"} {
		if _, err := ParseTicket(raw); err == nil {
			t.Fatal("accepted malformed ticket")
		}
	}
	for _, host := range []string{"-oProxyCommand=x", "relay-host;touch x", "relay-host\nother", ""} {
		if host == "" {
			continue
		}
		if _, err := NewSSHRelay(SSHConfig{Host: host}); err == nil {
			t.Fatalf("accepted SSH argument %q", host)
		}
	}
	if err := (SessionInfo{ICETransportPolicy: "all"}).validateICE(); err == nil {
		t.Fatal("accepted non-relayed transport")
	}
	info := SessionInfo{ICETransportPolicy: "relay", ICEServers: []webrtc.ICEServer{{URLs: []string{"turn:127.0.0.1:3479?transport=tcp"}, Username: "username", Credential: "password"}}}
	if err := info.validateICE(); err != nil {
		t.Fatal(err)
	}
}

func TestMacHostOfferNegotiatesConstrainedHighVideo(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS hardware encoder profile")
	}
	video, err := Quality("low")
	if err != nil {
		t.Fatal(err)
	}
	host, err := newPeer(context.Background(), SessionInfo{Video: video, Transport: "lan", ICETransportPolicy: "all"}, "host", "", nil, nil, peerCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	offer, err := host.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}

	// The viewer advertises the packetization mode and lower receive level used by WebKit.
	media := &webrtc.MediaEngine{}
	err = media.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f",
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := webrtc.NewAPI(webrtc.WithMediaEngine(media)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	if err := viewer.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := viewer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := answer.Unmarshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range parsed.MediaDescriptions {
		if section.MediaName.Media == "video" && section.MediaName.Port.Value != 0 {
			if fmtp, ok := section.Attribute("fmtp"); ok && strings.Contains(fmtp, "profile-level-id=640c") {
				return
			}
		}
	}
	t.Fatal("viewer did not negotiate Constrained High video")
}
func TestH264AssemblyUsesMarkerWithoutExtraFrameDelay(t *testing.T) {
	data := append([]byte{0, 0, 0, 1, 0x67, 0x64, 0, 0x2a, 0, 0, 0, 1, 0x68, 0xee, 0, 0, 0, 1, 0x65}, bytes.Repeat([]byte{0x55}, 12000)...)
	packetizer := rtp.NewPacketizer(500, 102, 1, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	packets := packetizer.Packetize(data, 3000)
	capture := time.Now().Truncate(time.Microsecond)
	ext, _ := rtp.NewAbsCaptureTimeExtension(capture).Marshal()
	for _, p := range packets {
		_ = p.SetExtension(1, ext)
	}
	assembler := newAccessUnits(1)
	order := append([]*rtp.Packet{packets[0], packets[len(packets)-1]}, packets[1:len(packets)-1]...)
	var frame *EncodedFrame
	for _, p := range order {
		f, need, err := assembler.push(p, time.Now())
		if err != nil || need {
			t.Fatalf("complete reordered frame rejected: %v %v", need, err)
		}
		if f != nil {
			frame = f
		}
	}
	if frame == nil || !frame.KeyFrame || !containsNAL(frame.Data, 7) || !containsNAL(frame.Data, 8) {
		t.Fatal("complete IDR not emitted at its marker")
	}
	if delta := frame.CapturedAt.Sub(capture); delta > 2*time.Microsecond || delta < -2*time.Microsecond {
		t.Fatalf("capture timestamp lost: %v", delta)
	}
	if repeat, _, _ := assembler.push(packets[0], time.Now()); repeat != nil {
		t.Fatal("replayed an old frame")
	}
}
func TestH264MissingPacketsAreBoundedAndRequestIDR(t *testing.T) {
	a := newAccessUnits(0)
	now := time.Now()
	first := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1, Timestamp: 100}, Payload: []byte{0x7c, 0x85, 1}}
	if f, _, err := a.push(first, now); f != nil || err != nil {
		t.Fatal("incomplete frame emitted")
	}
	next := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 4, Timestamp: 200, Marker: true}, Payload: []byte{0x61, 1}}
	_, need, err := a.push(next, now.Add(70*time.Millisecond))
	if !need || err != nil || len(a.groups) > 3 {
		t.Fatalf("missing reference chain not repaired: %v %v", need, err)
	}
}
func TestNativeFramingRejectsOversizeAndTruncation(t *testing.T) {
	var encoded bytes.Buffer
	if err := writeNative(&encoded, nativeH264, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	kind, data, err := readNative(&encoded)
	if err != nil || kind != nativeH264 || string(data) != "payload" {
		t.Fatal("native roundtrip")
	}
	var header [5]byte
	header[0] = nativeRGBA
	binary.BigEndian.PutUint32(header[1:], MaxNativePayload+1)
	if _, _, err := readNative(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversize native payload accepted")
	}
	binary.BigEndian.PutUint32(header[1:], 100)
	if _, _, err := readNative(bytes.NewReader(header[:])); !errors.Is(err, io.EOF) {
		t.Fatalf("truncated frame: %v", err)
	}
}
func TestFrameLeaseIsImmutableAndReleasedOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &ClientSession{ctx: ctx, cancel: cancel, started: time.Now(), wake: make(chan struct{}, 1)}
	var releases [3]atomic.Int32
	frames := make([]DecodedFrame, 3)
	for i := range frames {
		index := i
		var once sync.Once
		frames[i] = DecodedFrame{Image: image.NewRGBA(image.Rect(0, 0, 16, 16)), Release: func() { once.Do(func() { releases[index].Add(1) }) }}
	}
	c.onDecoded(frames[0])
	one, seq, _ := c.LatestFrame()
	again, seqAgain, _ := c.LatestFrame()
	if one != again || seq != seqAgain {
		t.Fatal("polling a static image copied or advanced it")
	}
	c.onDecoded(frames[1])
	if releases[0].Load() != 0 {
		t.Fatal("leased pixels reused before next request")
	}
	c.onDecoded(frames[2])
	if releases[1].Load() != 1 {
		t.Fatal("stale unpresented frame was retained")
	}
	two, _, _ := c.LatestFrame()
	if two == one || releases[0].Load() != 1 {
		t.Fatal("old lease not replaced")
	}
	_ = c.Close()
	for i := range releases {
		if releases[i].Load() != 1 {
			t.Fatalf("frame%d release=%d", i, releases[i].Load())
		}
	}
}

type recordingInput struct {
	mouse  []root.MouseEvent
	keys   []root.KeyEvent
	wheels []root.WheelEvent
}

func (s *recordingInput) Mouse(_ context.Context, e root.MouseEvent) error {
	s.mouse = append(s.mouse, e)
	return nil
}
func (s *recordingInput) Wheel(_ context.Context, e root.WheelEvent) error {
	s.wheels = append(s.wheels, e)
	return nil
}
func (s *recordingInput) Key(_ context.Context, e root.KeyEvent) error {
	s.keys = append(s.keys, e)
	return nil
}
func TestInputRejectsStaleMotionAndReleasesKeysOnDisconnect(t *testing.T) {
	sink := &recordingInput{}
	h := &Host{ctx: context.Background(), cfg: HostConfig{InputSink: sink}, keys: map[int]uint32{}, buttons: map[int]inputMessage{}}
	p := &peer{ctx: context.Background()}
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 2, Kind: "down", X: .2, Y: .3, Button: 0})
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 1, Kind: "move", X: .8, Y: .9})
	h.handleInput(p, inputMessage{Type: "key", Kind: "down", KeyCode: 56, Modifiers: 1 << 17})
	h.releaseInputs()
	if len(sink.mouse) != 2 || sink.mouse[1].Kind != "up" || sink.mouse[1].X != .2 {
		t.Fatalf("motion cleanup: %+v", sink.mouse)
	}
	if len(sink.keys) != 2 || sink.keys[1].Kind != "up" || sink.keys[1].Modifiers != 0 {
		t.Fatalf("modifier stuck after disconnect: %+v", sink.keys)
	}
}
func TestSSHStreamDeadlineClosesBlockedTransport(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan struct{})
	c := &sshConn{r: client, w: client, done: done, cancel: func() { _ = client.Close(); close(done) }}
	if err := c.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil || time.Since(start) > time.Second {
		t.Fatal("SSH deadline did not interrupt the blocked pipe")
	}
	_ = c.Close()
}
func TestNativeHardwareEncoderDecoderBridge(t *testing.T) {
	helper := os.Getenv("VANTRY_REMOTE_HELPER")
	if helper == "" {
		helper = filepath.Join("..", "native", "bin", "vantry-remote-helper")
	}
	absolute, err := filepath.Abs(helper)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(absolute); err != nil {
		t.Skip("build the native helper for hardware integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	v, _ := Quality("low")
	v.Width, v.Height, v.FPS = 320, 180, 30
	source, err := NewPatternSource(ctx, absolute, v, 45, false)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var count atomic.Int32
	var bad atomic.Bool
	decoder, err := NewNativeDecoder(ctx, absolute, func(frame DecodedFrame) {
		if frame.Image != nil {
			if frame.Image.Bounds().Dx() != 320 || frame.Image.Bounds().Dy() != 180 {
				bad.Store(true)
			}
			count.Add(1)
			frame.Release()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	for {
		frame, err := source.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = decoder.Decode(ctx, frame); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for count.Load() < 40 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count.Load() < 40 || bad.Load() {
		t.Fatalf("native pipeline frames=%d invalidDimensions=%v", count.Load(), bad.Load())
	}
}
