package remotedesktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const handshakeTimeout = 5 * time.Second

// packetStream serializes packets from video, input, and heartbeat goroutines.
type packetStream struct {
	io.ReadWriteCloser
	mu sync.Mutex
}

func (s *packetStream) send(f Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conn, ok := s.ReadWriteCloser.(net.Conn); ok {
		if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
	}
	return WriteFrame(s.ReadWriteCloser, f.Type, f.Payload)
}

func (s *packetStream) sendJSON(kind FrameType, value any) error {
	f, err := jsonFrame(kind, value)
	if err != nil {
		return err
	}
	return s.send(f)
}

func relayEndpoint(base, kind, sessionID string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("remote desktop: relay URL must be an HTTP(S) base URL")
	}
	if !validSessionID(sessionID) {
		return "", errors.New("remote desktop: invalid session ID")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/rd/" + kind + "/" + sessionID
	u.RawPath = ""
	return u.String(), nil
}

// httpStream carries framed packets in a streaming POST and its response.
// HTTP/1 servers must enable full duplex; HTTP/2 supports it directly.
type httpStream struct {
	reader io.ReadCloser
	writer *io.PipeWriter
	cancel context.CancelFunc
	once   sync.Once
}

func (s *httpStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *httpStream) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *httpStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.writer.CloseWithError(ErrClosed)
		_ = s.reader.Close()
	})
	return nil
}

func openHTTPStream(ctx context.Context, client *http.Client, endpoint, token string) (io.ReadWriteCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	if client == nil {
		client = http.DefaultClient
	}
	// Redirects must never move the bearer token or a streaming upload elsewhere.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	timer := time.AfterFunc(handshakeTimeout, cancel)
	resp, err := copyClient.Do(req)
	timer.Stop()
	if err == nil && ctx.Err() != nil {
		_ = resp.Body.Close()
		err = ctx.Err()
	}
	if err == nil && resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			err = ErrUnauthorized
		} else {
			err = fmt.Errorf("remote desktop: relay returned HTTP %d", resp.StatusCode)
		}
	}
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	return &httpStream{reader: resp.Body, writer: writer, cancel: cancel}, nil
}

func sshOptions(host string, port int) error {
	if strings.TrimSpace(host) == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n\x00") || port < 1 || port > 65535 {
		return errors.New("remote desktop: invalid SSH host or remote port")
	}
	return nil
}

func sshCommand(command string) string {
	if command == "" {
		return "ssh"
	}
	return command
}

type sshStream struct {
	reader io.ReadCloser
	writer io.WriteCloser
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (s *sshStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *sshStream) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *sshStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.writer.Close()
		_ = s.reader.Close()
	})
	<-s.done
	return nil
}

func openSSHStream(ctx context.Context, command, host string, port int) (io.ReadWriteCloser, error) {
	if err := sshOptions(host, port); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, sshCommand(command), "-o", "BatchMode=yes", "-W", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), host)
	reader, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	writer, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		_ = reader.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	s := &sshStream{reader: reader, writer: writer, cancel: cancel, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(s.done)
	}()
	return s, nil
}
