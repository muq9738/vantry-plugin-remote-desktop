package rtc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestSuccessfulSSHWriteDeadlineCannotCloseAnIdleReader(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan struct{})
	c := &sshConn{r: client, w: client, done: done, cancel: func() { _ = client.Close(); close(done) }}
	defer c.Close()
	go func() { var b [1]byte; _, _ = server.Read(b[:]) }()
	_ = c.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	go func() { _, _ = server.Write([]byte("y")) }()
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil || b[0] != 'y' {
		t.Fatalf("a completed write's deadline killed idle signaling: %v", err)
	}
	if _, err := c.Write([]byte("z")); err == nil {
		t.Fatal("expired deadline did not apply to a future write")
	}
}

func TestH264NewPFrameCannotSkipAnIncompleteReference(t *testing.T) {
	a := newAccessUnits(0)
	now := time.Now()
	idr := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 100, Timestamp: 1000, Marker: true}, Payload: []byte{0x65, 1}}
	if f, _, err := a.push(idr, now); f == nil || err != nil {
		t.Fatal("initial IDR missing")
	}
	p1 := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 101, Timestamp: 2000}, Payload: []byte{0x7c, 0x81, 1}}
	_, _, _ = a.push(p1, now.Add(time.Millisecond))
	p2 := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 103, Timestamp: 3000, Marker: true}, Payload: []byte{0x61, 1}}
	frame, need, err := a.push(p2, now.Add(2*time.Millisecond))
	if err != nil || frame == nil || !need {
		t.Fatalf("newer P-frame escaped missing-reference recovery: need=%v err=%v", need, err)
	}
	tail := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 102, Timestamp: 2000, Marker: true}, Payload: []byte{0x7c, 0x41, 1}}
	if stale, _, _ := a.push(tail, now.Add(3*time.Millisecond)); stale != nil {
		t.Fatal("old reference replayed after recovery decision")
	}
}
func TestStaticScreenMissingTailExpiresWithoutAnotherPacket(t *testing.T) {
	a := newAccessUnits(0)
	now := time.Now()
	partial := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 10, Timestamp: 1000}, Payload: []byte{0x7c, 0x85, 1}}
	_, _, _ = a.push(partial, now)
	if a.expire(now.Add(20 * time.Millisecond)) {
		t.Fatal("reorder window closed prematurely")
	}
	if !a.expire(now.Add(100*time.Millisecond)) || len(a.groups) != 0 {
		t.Fatal("static missing marker requires a future RTP packet to trigger repair")
	}
}

type renewalRelay struct{ info SessionInfo }

func (r renewalRelay) Create(context.Context, VideoConfig) (SessionInfo, error) { return r.info, nil }
func (r renewalRelay) Get(context.Context, string, string) (SessionInfo, error) { return r.info, nil }
func (r renewalRelay) Signal(context.Context, string, string, string) (*SignalConn, error) {
	return nil, errors.New("unused")
}
func (r renewalRelay) Delete(context.Context, string, string) error { return nil }
func TestReliableMouseReleaseCannotBeDiscardedAfterNewerMotion(t *testing.T) {
	sink := &recordingInput{}
	h := &Host{ctx: context.Background(), cfg: HostConfig{InputSink: sink}, keys: map[int]uint32{}, buttons: map[int]inputMessage{}}
	p := &peer{ctx: context.Background()}
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 1, Kind: "down", X: .2, Y: .3})
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 3, Kind: "move", X: .8, Y: .9})
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 2, Kind: "up", X: .2, Y: .3})
	if len(sink.mouse) != 3 || sink.mouse[2].Kind != "up" || len(h.buttons) != 0 {
		t.Fatalf("reliable release lost across channels: %+v", sink.mouse)
	}
}

func TestMotionWaitsForItsReliableButtonTransition(t *testing.T) {
	sink := &recordingInput{}
	h := &Host{ctx: context.Background(), cfg: HostConfig{InputSink: sink}, keys: map[int]uint32{}, buttons: map[int]inputMessage{}}
	p := &peer{ctx: context.Background()}
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 1, Kind: "down", X: .2, Y: .3})
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 3, ButtonSeq: 2, Kind: "move", X: .8, Y: .9})
	if len(sink.mouse) != 1 {
		t.Fatal("motion passed the button state it depends on")
	}
	h.handleInput(p, inputMessage{Type: "mouse", Seq: 2, Kind: "up", X: .2, Y: .3})
	if len(sink.mouse) != 3 || sink.mouse[1].Kind != "up" || sink.mouse[2].Kind != "move" || sink.mouse[2].X != .8 || len(h.buttons) != 0 {
		t.Fatalf("cursor/button order incorrect: %+v", sink.mouse)
	}
}

func TestFailedICERestartDoesNotAdvanceCredentialExpiry(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	oldExpiry := time.Now().Add(time.Minute)
	fresh := time.Now().Add(10 * time.Minute)
	info := SessionInfo{SessionID: "test", ICETransportPolicy: "relay", ExpiresAt: fresh, ICEServers: []webrtc.ICEServer{{URLs: []string{"turn:127.0.0.1:3479?transport=tcp"}, Username: "user", Credential: "pass"}}}
	p := &peer{ctx: context.Background(), pc: pc, role: "viewer", info: SessionInfo{SessionID: "test", ExpiresAt: oldExpiry}, relay: renewalRelay{info: info}}
	if err = p.renew(); err == nil {
		t.Fatal("restart unexpectedly succeeded without an open control channel")
	}
	if !p.info.ExpiresAt.Equal(oldExpiry) {
		t.Fatal("failed restart deferred retry until the new TURN expiry")
	}
	pending := &peer{ctx: context.Background(), offerOutstanding: true}
	if err = pending.offer(true); err == nil {
		t.Fatal("an unsent restart was reported as accepted")
	}
}
