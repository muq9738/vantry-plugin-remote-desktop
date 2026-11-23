package remotedesktop

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type animatedCapturer struct {
	count    atomic.Int64
	released atomic.Int64
	static   bool
}

func (c *animatedCapturer) Displays(context.Context) ([]DisplayInfo, error) {
	return []DisplayInfo{{ID: 7, Name: "Synthetic", Width: 128, Height: 80, Main: true, Scale: 1}}, nil
}

func (c *animatedCapturer) Capture(ctx context.Context, id uint32, scale float64) (*image.RGBA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id != 7 {
		return nil, errors.New("unexpected display")
	}
	n := c.count.Add(1)
	if c.static {
		n = 1
	}
	frame := image.NewRGBA(image.Rect(0, 0, int(128*scale), int(80*scale)))
	for y := 0; y < frame.Rect.Dy(); y++ {
		for x := 0; x < frame.Rect.Dx(); x++ {
			frame.SetRGBA(x, y, color.RGBA{R: uint8(n * 31), G: uint8(x * 2), B: uint8(y * 3), A: 255})
		}
	}
	return frame, nil
}

func (c *animatedCapturer) Release(*image.RGBA) { c.released.Add(1) }

type recordingSink struct {
	mouse chan MouseEvent
	wheel chan WheelEvent
	key   chan KeyEvent
}

func newSink() *recordingSink {
	return &recordingSink{mouse: make(chan MouseEvent, 64), wheel: make(chan WheelEvent, 64), key: make(chan KeyEvent, 64)}
}
func (s *recordingSink) Mouse(_ context.Context, e MouseEvent) error { s.mouse <- e; return nil }
func (s *recordingSink) Wheel(_ context.Context, e WheelEvent) error { s.wheel <- e; return nil }
func (s *recordingSink) Key(_ context.Context, e KeyEvent) error     { s.key <- e; return nil }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testHost(t *testing.T, ctx context.Context, cfg HostConfig) (*Host, *animatedCapturer, *recordingSink) {
	t.Helper()
	capture, sink := &animatedCapturer{}, newSink()
	cfg.Capturer, cfg.InputSink = capture, sink
	host, err := StartHost(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host, capture, sink
}

func testRelay(t *testing.T) (*RelayServer, string) {
	t.Helper()
	relay := NewRelayServer()
	server := httptest.NewServer(relay)
	t.Cleanup(func() {
		_ = relay.Close()
		server.Close()
	})
	return relay, server.URL
}

func testDial(t *testing.T, ctx context.Context, cfg ConnectConfig) *ClientSession {
	t.Helper()
	client, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func eventually(t *testing.T, ctx context.Context, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("condition did not become true:", ctx.Err())
		case <-ticker.C:
		}
	}
}

func receive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal("event was not delivered:", ctx.Err())
		var zero T
		return zero
	}
}

func checkStreaming(t *testing.T, ctx context.Context, client *ClientSession, mode string, sink *recordingSink) {
	t.Helper()
	notified := make(chan struct{}, 1)
	client.OnFrame(func() {
		client.LatestFrame()
		select {
		case notified <- struct{}{}:
		default:
		}
	})
	receive(t, ctx, notified)
	eventually(t, ctx, func() bool {
		frame, seq, stats := client.LatestFrame()
		return frame != nil && seq >= 2 && stats.RTTMs > 0
	})
	frame, seq, stats := client.LatestFrame()
	if stats.Mode != mode || stats.Width != 96 || stats.Height != 60 || stats.FPS <= 0 || stats.Kbps <= 0 || frame.Rect.Size() != image.Pt(96, 60) {
		t.Fatalf("unexpected frame or stats: bounds=%v seq=%d stats=%+v", frame.Rect, seq, stats)
	}
	if client.Display().ID != 7 {
		t.Fatalf("display metadata missing: %+v", client.Display())
	}
	pixels := append([]byte(nil), frame.Pix...)
	for _, event := range []MouseEvent{{Kind: "move", X: .2, Y: .3}, {Kind: "down", X: .4, Y: .5, Button: 1}, {Kind: "drag", X: .5, Y: .6, Button: 1}, {Kind: "up", X: 1, Y: 0, Button: 2}} {
		if err := client.SendMouse(event.Kind, event.X, event.Y, event.Button); err != nil {
			t.Fatal(err)
		}
		got := receive(t, ctx, sink.mouse)
		event.DisplayID = 7
		if got != event {
			t.Fatalf("mouse: got %+v want %+v", got, event)
		}
	}
	if err := client.SendWheel(-2.5, 13); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, ctx, sink.wheel); got != (WheelEvent{DX: -2.5, DY: 13}) {
		t.Fatalf("wheel: %+v", got)
	}
	for _, kind := range []string{"down", "up"} {
		if err := client.SendKey(kind, 12, "你好", 1<<20); err != nil {
			t.Fatal(err)
		}
		if got := receive(t, ctx, sink.key); got != (KeyEvent{Kind: kind, KeyCode: 12, Text: "你好", Modifiers: 1 << 20}) {
			t.Fatalf("key: %+v", got)
		}
	}
	if err := client.SetQuality("low"); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool {
		_, current, stats := client.LatestFrame()
		return current > seq && stats.Width == 64 && stats.Height == 40
	})
	if !bytes.Equal(pixels, frame.Pix) {
		t.Fatal("retained frame was modified during buffer reuse")
	}
	if err := client.SetQuality("ultra"); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool {
		_, _, stats := client.LatestFrame()
		return stats.Width == 128 && stats.Height == 80
	})
}

func TestDirectAuthenticatedRaceAndInput(t *testing.T) {
	ctx := testContext(t)
	host, capture, sink := testHost(t, ctx, HostConfig{})
	otherHost, _, _ := testHost(t, ctx, HostConfig{})
	stalled, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	loserClosed := make(chan struct{})
	go func() {
		defer close(loserClosed)
		conn, err := stalled.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	cfg := host.ConnectConfig()
	cfg.DirectAddrs = []string{otherHost.Addr().String(), stalled.Addr().String(), "127.0.0.1:1", host.Addr().String()}
	cfg.RelayURL = "http://127.0.0.1:1"
	client := testDial(t, ctx, cfg)
	checkStreaming(t, ctx, client, "direct", sink)
	_ = stalled.Close()
	receive(t, ctx, loserClosed)
	if capture.released.Load() < 2 {
		t.Fatal("capture buffers were not returned")
	}
	// Exercise concurrent UI reads and callbacks while both internal buffers turn.
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 40 {
				if frame, _, _ := client.LatestFrame(); frame != nil {
					_ = frame.RGBAAt(2, 2)
				}
			}
		}()
	}
	readers.Wait()
}

func TestHTTPRelayFallbackAndInput(t *testing.T) {
	ctx := testContext(t)
	relay, base := testRelay(t)
	host, _, sink := testHost(t, ctx, HostConfig{RelayURL: base})
	if err := relay.WaitForHost(ctx, host.SessionID()); err != nil {
		t.Fatal(err)
	}
	cfg := host.ConnectConfig()
	cfg.DirectAddrs = []string{"127.0.0.1:1"}
	client := testDial(t, ctx, cfg)
	checkStreaming(t, ctx, client, "relay-http", sink)
	_, _, stats := client.LatestFrame()
	u, _ := url.Parse(base)
	if stats.RelayHost != u.Host {
		t.Fatalf("relay host = %q", stats.RelayHost)
	}
	cfg.DirectAddrs = nil
	second := testDial(t, ctx, cfg)
	eventually(t, ctx, func() bool {
		frame, _, stats := second.LatestFrame()
		return frame != nil && stats.Mode == "relay-http" && stats.RTTMs > 0
	})
	_ = second.Close()
	eventually(t, ctx, func() bool { _, seq, _ := client.LatestFrame(); return seq >= 5 })
}

// The test executable provides a real stdio/TCP proxy and reverse forward. It
// accepts only the expected OpenSSH arguments and never contacts an SSH server.
func init() {
	if os.Getenv("VANTRY_RD_TEST_SSH") != "1" {
		return
	}
	args := os.Args[1:]
	if len(args) == 5 && args[0] == "-o" && args[1] == "BatchMode=yes" && args[2] == "-W" && args[4] == "relay-host" {
		conn, err := net.Dial("tcp", args[3])
		if err != nil {
			os.Exit(2)
		}
		go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.Close() }()
		_, _ = io.Copy(os.Stdout, conn)
		_ = conn.Close()
		os.Exit(0)
	}
	if len(args) == 10 && strings.Join(args[:8], " ") == "-N -o BatchMode=yes -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -R" && args[9] == "relay-host" {
		parts := strings.Split(args[8], ":")
		if len(parts) != 4 || parts[0] != "127.0.0.1" || parts[2] != "127.0.0.1" {
			os.Exit(3)
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(parts[0], parts[1]))
		if err != nil {
			os.Exit(4)
		}
		for {
			incoming, err := listener.Accept()
			if err != nil {
				os.Exit(5)
			}
			go func() {
				defer incoming.Close()
				outgoing, err := net.Dial("tcp", net.JoinHostPort(parts[2], parts[3]))
				if err != nil {
					return
				}
				defer outgoing.Close()
				go func() { _, _ = io.Copy(outgoing, incoming); _ = outgoing.Close() }()
				_, _ = io.Copy(incoming, outgoing)
			}()
		}
	}
	os.Exit(6)
}

func TestSSHRelayFallbackThroughRelayHost(t *testing.T) {
	t.Setenv("VANTRY_RD_TEST_SSH", "1")
	ctx := testContext(t)
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remotePort := reservation.Addr().(*net.TCPAddr).Port
	_ = reservation.Close()
	host, _, sink := testHost(t, ctx, HostConfig{RelaySSHHost: "relay-host", RemotePort: remotePort, SSHCommand: command})
	eventually(t, ctx, func() bool {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort)), 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err == nil
	})
	cfg := host.ConnectConfig()
	cfg.DirectAddrs = []string{"127.0.0.1:1"}
	client := testDial(t, ctx, cfg)
	checkStreaming(t, ctx, client, "relay-ssh", sink)
	_, _, stats := client.LatestFrame()
	if stats.RelayHost != "relay-host" {
		t.Fatalf("relay host = %q", stats.RelayHost)
	}
	_ = client.Close()
	_ = host.Close()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(remotePort)), 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("managed reverse forward survived host Close")
	}
}

func TestInvalidBearerRejected(t *testing.T) {
	ctx := testContext(t)
	relay, base := testRelay(t)
	host, _, _ := testHost(t, ctx, HostConfig{RelayURL: base})
	if err := relay.WaitForHost(ctx, host.SessionID()); err != nil {
		t.Fatal(err)
	}
	wrong, _ := newToken(32)
	cfg := host.ConnectConfig()
	cfg.Token, cfg.RelayURL = wrong, ""
	if client, err := Dial(ctx, cfg); !errors.Is(err, ErrUnauthorized) || client != nil {
		t.Fatalf("direct accepted invalid credentials: %v", err)
	}
	cfg.DirectAddrs, cfg.RelayURL = nil, base
	if client, err := Dial(ctx, cfg); !errors.Is(err, ErrUnauthorized) || client != nil {
		t.Fatalf("HTTP accepted invalid credentials: %v", err)
	}
	for _, kind := range []string{"view", "mjpeg", "input", "host"} {
		method := http.MethodGet
		if kind == "input" || kind == "host" {
			method = http.MethodPost
		}
		req, _ := http.NewRequestWithContext(ctx, method, base+"/rd/"+kind+"/"+host.SessionID(), nil)
		req.Header.Set("Authorization", "Bearer "+wrong)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s returned %d", kind, resp.StatusCode)
		}
	}
	cfg = host.ConnectConfig()
	cfg.DirectAddrs = nil
	client, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("valid client failed after rejection: %v; host: %v", err, host.Err())
	}
	t.Cleanup(func() { _ = client.Close() })
	eventually(t, ctx, func() bool {
		if err := client.Err(); err != nil {
			t.Fatalf("valid client disconnected: %v; host: %v", err, host.Err())
		}
		frame, _, _ := client.LatestFrame()
		return frame != nil
	})
}

func TestMobileViewerMJPEGAndInputs(t *testing.T) {
	ctx := testContext(t)
	relay, base := testRelay(t)
	host, _, sink := testHost(t, ctx, HostConfig{RelayURL: base})
	if err := relay.WaitForHost(ctx, host.SessionID()); err != nil {
		t.Fatal(err)
	}
	get := func(kind string) *http.Response {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rd/"+kind+"/"+host.SessionID()+"?token="+url.QueryEscape(host.Token()), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Fatalf("%s returned %d", kind, resp.StatusCode)
		}
		return resp
	}
	page := get("view")
	body, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	for _, fragment := range []string{"viewport", "touch-action:none", "pointerdown", "keydown", "mjpeg/", "input/", "Authorization", "Bearer ", host.Token()} {
		if !bytes.Contains(body, []byte(fragment)) {
			t.Fatalf("viewer missing %q", fragment)
		}
	}
	if page.Header.Get("Referrer-Policy") != "no-referrer" || page.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("viewer headers allow credential persistence")
	}
	stream := get("mjpeg")
	defer stream.Body.Close()
	mediaType, params, err := mime.ParseMediaType(stream.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/x-mixed-replace" {
		t.Fatalf("invalid MJPEG type: %v %q", err, mediaType)
	}
	reader := multipart.NewReader(stream.Body, params["boundary"])
	for range 2 {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatal("MJPEG part is not JPEG")
		}
		frame, err := jpeg.Decode(part)
		if err != nil || frame.Bounds().Size() != image.Pt(96, 60) {
			t.Fatalf("invalid MJPEG frame: %v", err)
		}
		_ = part.Close()
	}
	for _, input := range []WebInput{{Type: "mouse", Kind: "down", X: .25, Y: .75, Button: 1}, {Type: "wheel", DX: 4, DY: -8}, {Type: "key", Kind: "down", Text: "手机⌨️", KeyCode: 0}} {
		data, _ := json.Marshal(input)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rd/input/"+host.SessionID(), bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+host.Token())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("input returned %d", resp.StatusCode)
		}
	}
	if got := receive(t, ctx, sink.mouse); got != (MouseEvent{Kind: "down", X: .25, Y: .75, Button: 1, DisplayID: 7}) {
		t.Fatalf("mobile mouse = %+v", got)
	}
	if got := receive(t, ctx, sink.wheel); got != (WheelEvent{DX: 4, DY: -8}) {
		t.Fatalf("mobile wheel = %+v", got)
	}
	if got := receive(t, ctx, sink.key); got.Text != "手机⌨️" {
		t.Fatalf("mobile text = %+v", got)
	}
	for _, data := range []string{`{"type":"mouse","kind":"down","x":2,"y":0}`, `{"type":"key","kind":"paste"}`, `{"type":"wheel","dy":1} {}`, `{"type":"wheel","extra":1}`} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rd/input/"+host.SessionID(), strings.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+host.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid input returned %d", resp.StatusCode)
		}
	}
}

func TestStaticFramesSkipEncodingAndQualityRefreshes(t *testing.T) {
	ctx := testContext(t)
	capture := &animatedCapturer{static: true}
	host, err := StartHost(ctx, HostConfig{Capturer: capture, InputSink: newSink()})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	client := testDial(t, ctx, host.ConnectConfig())
	eventually(t, ctx, func() bool { return capture.count.Load() >= 6 })
	_, seq, _ := client.LatestFrame()
	if seq != 1 {
		t.Fatalf("static display sent %d frames", seq)
	}
	if err := client.SetQuality("ultra"); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool { _, seq, stats := client.LatestFrame(); return seq >= 2 && stats.Width == 128 })
}

func TestCloseFromCallbackAndContextCancellation(t *testing.T) {
	ctx := testContext(t)
	host, _, _ := testHost(t, ctx, HostConfig{})
	client := testDial(t, ctx, host.ConnectConfig())
	closed := make(chan struct{}, 1)
	client.OnFrame(func() {
		_ = client.Close()
		select {
		case closed <- struct{}{}:
		default:
		}
	})
	receive(t, ctx, closed)
	if err := client.SendWheel(0, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed session accepted input: %v", err)
	}
	child, cancel := context.WithCancel(ctx)
	second := testDial(t, child, host.ConnectConfig())
	cancel()
	receive(t, ctx, second.Done())
	_ = second.Close()
}

func TestProtocolBoundsAndPresets(t *testing.T) {
	for _, want := range []QualityPreset{{"ultra", 1, 85, 30}, {"balanced", .75, 72, 24}, {"low", .5, 55, 15}} {
		got, err := Quality(want.Name)
		if err != nil || got != want {
			t.Fatalf("quality: %+v %v", got, err)
		}
	}
	if _, err := Quality("unknown"); err == nil {
		t.Fatal("unknown quality accepted")
	}
	for kind := FrameHello; kind <= FramePong; kind++ {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, kind, []byte{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		if got := buf.Bytes(); got[0] != byte(kind) || binary.BigEndian.Uint32(got[1:5]) != 3 {
			t.Fatalf("incorrect wire header: %v", got[:5])
		}
		frame, err := ReadFrame(&buf)
		if err != nil || frame.Type != kind || !bytes.Equal(frame.Payload, []byte{1, 2, 3}) {
			t.Fatalf("packet: %+v %v", frame, err)
		}
	}
	var header [5]byte
	header[0] = byte(FrameVideoJPEG)
	binary.BigEndian.PutUint32(header[1:], maxFrameBytes+1)
	if _, err := ReadFrame(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{255, 0, 0, 0, 0})); err == nil {
		t.Fatal("unknown frame type accepted")
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{byte(FrameVideoJPEG), 0, 0, 0, 1})); err == nil {
		t.Fatal("truncated frame accepted")
	}
	a, b := image.NewRGBA(image.Rect(3, 4, 11, 12)), image.NewRGBA(image.Rect(0, 0, 8, 8))
	if !sampledEqual(a, b) {
		t.Fatal("equal frames with different origins differ")
	}
	b.Pix[4] = 1
	if sampledEqual(a, b) || sampledEqual(a, nil) {
		t.Fatal("changed frame considered static")
	}
	bad := &ClientSession{}
	if err := bad.decodeJPEG([]byte("invalid")); err == nil {
		t.Fatal("invalid JPEG accepted")
	}
}

func TestHostCloseDisconnectsRelayViewers(t *testing.T) {
	ctx := testContext(t)
	relay, base := testRelay(t)
	host, _, _ := testHost(t, ctx, HostConfig{RelayURL: base})
	if err := relay.WaitForHost(ctx, host.SessionID()); err != nil {
		t.Fatal(err)
	}
	cfg := host.ConnectConfig()
	cfg.DirectAddrs = nil
	client := testDial(t, ctx, cfg)
	eventually(t, ctx, func() bool { frame, _, _ := client.LatestFrame(); return frame != nil })
	_ = host.Close()
	receive(t, ctx, client.Done())
	_ = relay.Close()
	if _, err := relayEndpoint("file:///tmp/relay", "client", "session"); err == nil {
		t.Fatal("non-HTTP relay accepted")
	}
	if err := sshOptions("-oProxyCommand=anything", 1); err == nil {
		t.Fatal("SSH option accepted as host")
	}
	if _, err := relayEndpoint(base, "client", "../session"); err == nil {
		t.Fatal("invalid session accepted")
	}
}

func ExampleQuality() {
	preset, _ := Quality("balanced")
	fmt.Println(preset.Scale, preset.JPEGQuality, preset.FPS)
	// Output: 0.75 72 24
}
