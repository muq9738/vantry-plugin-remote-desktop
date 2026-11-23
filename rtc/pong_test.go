package rtc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Simulate a stalled SSH stdin while reads can still receive a WebSocket Ping.
type blockedPongWriter struct {
	net.Conn
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (w *blockedPongWriter) Write(b []byte) (int, error) {
	if len(b) > 0 && b[0]&15 == websocket.PongMessage {
		close(w.started)
		<-w.closed
		return 0, net.ErrClosed
	}
	return w.Conn.Write(b)
}
func (w *blockedPongWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return w.Conn.Close()
}

// Schedule automatic Pong after JSON's write unlocks Gorilla's write mutex,
// but before Signal.Send's deferred underlying deadline reset.
type pongInterleave struct {
	*sshConn
	armed  atomic.Bool
	writer *blockedPongWriter
}

func (c *pongInterleave) SetWriteDeadline(at time.Time) error {
	if at.IsZero() && c.armed.Load() {
		select {
		case <-c.writer.started:
		case <-time.After(2 * time.Second):
			return context.DeadlineExceeded
		}
	}
	return c.sshConn.SetWriteDeadline(at)
}

func TestSignalSendPreservesConcurrentAutomaticPongDeadline(t *testing.T) {
	upgrade := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrade.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if _, _, err = ws.ReadMessage(); err != nil {
			return
		}
		_ = ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second))
		_, _, _ = ws.ReadMessage()
	}))
	defer server.Close()
	var observed *pongInterleave
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		writer := &blockedPongWriter{Conn: raw, started: make(chan struct{}), closed: make(chan struct{})}
		done := make(chan struct{})
		stream := &sshConn{r: raw, w: writer, done: done, cancel: func() { _ = raw.Close(); close(done) }}
		observed = &pongInterleave{sshConn: stream, writer: writer}
		return observed, nil
	}}
	ws, _, err := dialer.Dial("ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	readDone := make(chan struct{})
	go func() { defer close(readDone); _, _, _ = ws.ReadMessage() }()
	defer func() { _ = ws.Close(); <-readDone }()
	observed.armed.Store(true)
	if err = NewSignalConn(ws, nil).Send(context.Background(), SignalMessage{Type: "peer", Connected: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed.writer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic Pong did not reach the blocked writer")
	}
	select {
	case <-readDone:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Signal.Send removed automatic Pong's 1s deadline: write and signal reader remain blocked after 1.5s")
	}
}
