package remotedesktop

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnresponsiveDirectHandshakeFallsBack(t *testing.T) {
	ctx := testContext(t)
	relay, base := testRelay(t)
	host, _, _ := testHost(t, ctx, HostConfig{RelayURL: base})
	if err := relay.WaitForHost(ctx, host.SessionID()); err != nil {
		t.Fatal(err)
	}
	stalled, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		conn, err := stalled.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	cfg := host.ConnectConfig()
	cfg.DirectAddrs, cfg.DirectTimeout = []string{stalled.Addr().String()}, 40*time.Millisecond
	started := time.Now()
	client := testDial(t, ctx, cfg)
	if time.Since(started) > time.Second {
		t.Fatal("unresponsive authentication outlived the direct timeout")
	}
	_ = stalled.Close()
	receive(t, ctx, disconnected)
	eventually(t, ctx, func() bool {
		frame, _, stats := client.LatestFrame()
		return frame != nil && stats.Mode == "relay-http"
	})
}

func TestHTTP2RelayAndHostDefaultQuality(t *testing.T) {
	ctx := testContext(t)
	relay := NewRelayServer()
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			requests.Add(1)
		}
		relay.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(func() { _ = relay.Close(); server.Close() })
	host, _, _ := testHost(t, ctx, HostConfig{RelayURL: server.URL, HTTPClient: server.Client(), Quality: "low"})
	if err := relay.WaitForHost(ctx, host.SessionID()); err != nil {
		t.Fatal(err)
	}
	cfg := host.ConnectConfig()
	cfg.DirectAddrs, cfg.Quality = nil, ""
	client := testDial(t, ctx, cfg)
	eventually(t, ctx, func() bool {
		frame, _, stats := client.LatestFrame()
		return frame != nil && stats.Width == 64 && stats.Height == 40 && stats.RTTMs > 0
	})
	if requests.Load() < 2 {
		t.Fatal("host and client did not use HTTP/2")
	}
}

func TestRelayDisconnectReconnectAndShutdown(t *testing.T) {
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
	relay.mu.Lock()
	previous := relay.sessions[host.SessionID()]
	relay.mu.Unlock()
	previous.close()
	receive(t, ctx, client.Done())
	// The host owns reconnecting its upload after a relay connection is lost.
	eventually(t, ctx, func() bool {
		relay.mu.Lock()
		session := relay.sessions[host.SessionID()]
		relay.mu.Unlock()
		if session == nil || session == previous {
			return false
		}
		select {
		case <-session.ready:
			return true
		default:
			return false
		}
	})
	second := testDial(t, ctx, cfg)
	eventually(t, ctx, func() bool { frame, _, _ := second.LatestFrame(); return frame != nil })
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	receive(t, ctx, second.Done())
}
