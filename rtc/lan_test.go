package rtc

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	root "github.com/muq9738/vantry-plugin-remote-desktop"
)

type idleLANSource struct{}

func (idleLANSource) Next(ctx context.Context) (EncodedFrame, error) {
	<-ctx.Done()
	return EncodedFrame{}, ctx.Err()
}
func (idleLANSource) RequestKeyFrame() error { return nil }
func (idleLANSource) SetBitrate(int) error   { return nil }
func (idleLANSource) Close() error           { return nil }

type idleLANDecoder struct{}

func (idleLANDecoder) Decode(context.Context, EncodedFrame) error { return nil }
func (idleLANDecoder) Close() error                               { return nil }

type lanInput struct{ keys chan root.KeyEvent }

func (s lanInput) Mouse(context.Context, root.MouseEvent) error { return nil }
func (s lanInput) Wheel(context.Context, root.WheelEvent) error { return nil }
func (s lanInput) Key(ctx context.Context, event root.KeyEvent) error {
	select {
	case s.keys <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLANConnectionNeedsNoSSHOrRelayAndDeliversInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	sink := lanInput{keys: make(chan root.KeyEvent, 8)}
	host, err := StartHost(ctx, HostConfig{LANListenAddr: "127.0.0.1:0", InputSink: sink, SourceFactory: func(context.Context, VideoConfig) (EncodedSource, error) { return idleLANSource{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if !strings.HasPrefix(host.Ticket(), "vantry-lan://") || len(host.DirectAddrs()) != 1 {
		t.Fatal("default host did not advertise a LAN link")
	}
	client, err := Dial(ctx, ConnectConfig{Ticket: host.Ticket(), DecoderFactory: func(context.Context, func(DecodedFrame)) (FrameDecoder, error) { return idleLANDecoder{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _, stats := client.LatestFrame()
	if !stats.Connected || stats.Mode != "direct-webrtc" || stats.RelayHost != "" || stats.Transport != "lan" {
		t.Fatalf("LAN unexpectedly used an external relay: %+v", stats)
	}
	if len(client.p.pc.GetConfiguration().ICEServers) != 0 {
		t.Fatal("LAN attempted STUN or TURN")
	}
	if err := client.SendKey("down", 10, "", 0); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sink.keys:
		if event.Kind != "down" || event.KeyCode != 10 {
			t.Fatal("input was changed")
		}
	case <-ctx.Done():
		t.Fatal("LAN data channel did not deliver input")
	}
	ticket := host.Ticket()
	_ = client.Close()
	_ = host.Close()
	parsed, err := ParseLANTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := newLANClient(parsed)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if _, err = probe.Get(ctx, parsed.SessionID, parsed.Token); err == nil {
		t.Fatal("closed sharing remained reachable")
	}
}

func TestLANSignalingAuthenticatesAndKeepsHostTokenPrivate(t *testing.T) {
	server, err := newLANServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	video, _ := Quality("low")
	info, err := server.Create(context.Background(), video)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := ParseLANTicket(server.Ticket(info.SessionID, info.ViewerToken))
	if err != nil {
		t.Fatal(err)
	}
	client, err := newLANClient(ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	got, err := client.Get(context.Background(), ticket.SessionID, ticket.Token)
	if err != nil || got.SessionID != info.SessionID || got.HostToken != "" || got.ViewerToken != "" {
		t.Fatalf("private credentials leaked or session failed: %v", err)
	}
	if _, err := client.Get(context.Background(), ticket.SessionID, strings.Repeat("x", 43)); err == nil {
		t.Fatal("wrong token accepted")
	}
	response, err := client.client.Get("https://" + client.address + "/v1/sessions/" + info.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("anonymous request could inspect sharing")
	}
	if _, err := client.Signal(context.Background(), ticket.SessionID, "host", ticket.Token); err == nil {
		t.Fatal("viewer token impersonated the host")
	}
	other, err := newLANServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ticket.Certificate = other.der
	untrusted, err := newLANClient(ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.Close()
	if _, err := untrusted.Get(context.Background(), ticket.SessionID, ticket.Token); err == nil {
		t.Fatal("a different certificate was accepted")
	}
}

// This runs the production hardware encoder/decoder with generated pixels,
// without requesting screen access or sending input to the operating system.
func TestLANHardwareH264WithoutExternalRelay(t *testing.T) {
	helper := os.Getenv("VANTRY_REMOTE_HELPER")
	if helper == "" {
		t.Skip("set VANTRY_REMOTE_HELPER for native LAN integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	host, err := StartHost(ctx, HostConfig{LANListenAddr: "127.0.0.1:0", Quality: "low", InputSink: lanInput{keys: make(chan root.KeyEvent, 8)}, SourceFactory: func(ctx context.Context, video VideoConfig) (EncodedSource, error) {
		return NewPatternSource(ctx, helper, video, 120, false)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	client, err := Dial(ctx, ConnectConfig{Ticket: host.Ticket(), HelperPath: helper})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		frame, seq, stats := client.LatestFrame()
		if seq >= 3 && frame != nil {
			if frame.Bounds().Dx() != 1280 || frame.Bounds().Dy() != 720 || stats.Mode != "direct-webrtc" || stats.Codec != "h264" || !stats.Hardware || stats.RelayHost != "" {
				t.Fatalf("native LAN media differs from the requested route: %s %s %v", stats.Mode, stats.Codec, frame.Bounds())
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no native LAN frames: %v", client.Err())
		case <-ticker.C:
		}
	}
}

func TestLANLinkAndEmptyRelayNeverSelectAPrivateDefault(t *testing.T) {
	if relay, err := NewSSHRelay(SSHConfig{}); err == nil {
		relay.Close()
		t.Fatal("empty relay selected a private server")
	}
	for _, address := range []string{"8.8.8.8:443", "example.com:443", "127.0.0.1:0", "127.0.0.1:70000"} {
		if _, err := ParseLANTicket((LANTicket{Address: address, SessionID: "test", Token: strings.Repeat("t", 40)}).String()); err == nil {
			t.Fatal("invalid LAN address accepted")
		}
	}
	if err := (SessionInfo{Transport: "lan", ICETransportPolicy: "all"}).validateICE(); err != nil {
		t.Fatal(err)
	}
}
