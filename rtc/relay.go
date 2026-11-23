package rtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SSHConfig reuses OpenSSH identity and host-key checking. ConfigFile is
// optional and references an existing client configuration, never copied keys.
type SSHConfig struct {
	Host        string
	ConfigFile  string
	Binary      string
	RelayBinary string
	SocketPath  string
}

type SSHRelay struct {
	cfg          SSHConfig
	client       *http.Client
	transport    *http.Transport
	ctx          context.Context
	cancel       context.CancelFunc
	bridgeMu     sync.Mutex
	turnListener net.Listener
	turnPort     int
	turnConns    map[net.Conn]struct{}
	closed       bool
	bridgeWG     sync.WaitGroup
}

func NewSSHRelay(cfg SSHConfig) (*SSHRelay, error) {
	cfg.Host = strings.TrimSpace(cfg.Host)
	if !validSSH(cfg.Host) {
		return nil, errors.New("rtc: configure your own relay SSH host, or use LAN sharing")
	}
	if cfg.Binary == "" {
		cfg.Binary = "ssh"
	}
	if cfg.RelayBinary == "" {
		cfg.RelayBinary = "/opt/vantry-rtc/bin/relayrtc"
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = "/run/vantry-rtc/signaling.sock"
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &SSHRelay{cfg: cfg, ctx: ctx, cancel: cancel, turnConns: map[net.Conn]struct{}{}}
	r.transport = &http.Transport{DialContext: r.dial, MaxIdleConns: 4, MaxConnsPerHost: 8, IdleConnTimeout: time.Minute, ResponseHeaderTimeout: 15 * time.Second}
	r.client = &http.Client{Transport: r.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return r, nil
}
func (r *SSHRelay) Close()       { r.closeTransport() }
func quoteShell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func remotePath(s string) string {
	if strings.HasPrefix(s, "~/") {
		return `"$HOME"/` + quoteShell(strings.TrimPrefix(s, "~/"))
	}
	return quoteShell(s)
}
func (r *SSHRelay) sshArgs() []string {
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15"}
	if r.cfg.ConfigFile != "" {
		args = append(args, "-F", r.cfg.ConfigFile)
	}
	return args
}
func (r *SSHRelay) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	command := "exec " + remotePath(r.cfg.RelayBinary) + " proxy --socket " + remotePath(r.cfg.SocketPath)
	return r.openSSH(ctx, append(r.sshArgs(), "--", r.cfg.Host, command))
}
func (r *SSHRelay) openSSH(ctx context.Context, args []string) (net.Conn, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	life, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(life, r.cfg.Binary, args...)
	configureChildProcess(cmd)
	reader, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	writer, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.New("rtc: cannot connect to the configured SSH relay")
	}
	c := &sshConn{r: reader, w: writer, cancel: cancel, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(c.done) }()
	if ctx.Err() != nil {
		_ = c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

type sshConn struct {
	r               io.ReadCloser
	w               io.WriteCloser
	cancel          context.CancelFunc
	done            chan struct{}
	once            sync.Once
	deadlineMu      sync.Mutex
	readTimer       *time.Timer
	writeTimer      *time.Timer
	readAt          time.Time
	writeAt         time.Time
	readActive      bool
	writeActive     bool
	readGeneration  uint64
	writeGeneration uint64
	closed          bool
}

func (c *sshConn) begin(read bool) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	at := c.writeAt
	if read {
		at = c.readAt
	}
	if !at.IsZero() && !time.Now().Before(at) {
		return os.ErrDeadlineExceeded
	}
	if read {
		c.readActive = true
	} else {
		c.writeActive = true
	}
	c.armLocked(read)
	return nil
}
func (c *sshConn) end(read bool) {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if read {
		c.readActive = false
	} else {
		c.writeActive = false
	}
	c.armLocked(read)
}
func (c *sshConn) Read(b []byte) (int, error) {
	if err := c.begin(true); err != nil {
		return 0, err
	}
	n, err := c.r.Read(b)
	c.end(true)
	return n, err
}
func (c *sshConn) Write(b []byte) (int, error) {
	if err := c.begin(false); err != nil {
		return 0, err
	}
	n, err := c.w.Write(b)
	c.end(false)
	return n, err
}
func (c *sshConn) Close() error {
	c.once.Do(func() {
		c.deadlineMu.Lock()
		c.closed = true
		c.readActive = false
		c.writeActive = false
		c.armLocked(true)
		c.armLocked(false)
		c.deadlineMu.Unlock()
		c.cancel()
		_ = c.w.Close()
		_ = c.r.Close()
	})
	<-c.done
	return nil
}

// A net.Conn deadline bounds active I/O and future I/O; it must never kill a
// healthy idle stream. In particular WebSocket's automatic Pong leaves its
// deadline installed after a successful write.
func (c *sshConn) armLocked(read bool) {
	slot, at, active, generation := &c.writeTimer, c.writeAt, c.writeActive, &c.writeGeneration
	if read {
		slot, at, active, generation = &c.readTimer, c.readAt, c.readActive, &c.readGeneration
	}
	*generation++
	version := *generation
	if *slot != nil {
		(*slot).Stop()
		*slot = nil
	}
	if !active || at.IsZero() || c.closed {
		return
	}
	*slot = time.AfterFunc(max(time.Until(at), 0), func() {
		c.deadlineMu.Lock()
		valid := !c.closed
		if read {
			valid = valid && c.readActive && c.readGeneration == version
		} else {
			valid = valid && c.writeActive && c.writeGeneration == version
		}
		c.deadlineMu.Unlock()
		if valid {
			_ = c.Close()
		}
	})
}
func (c *sshConn) SetDeadline(at time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readAt, c.writeAt = at, at
	c.armLocked(true)
	c.armLocked(false)
	return nil
}
func (c *sshConn) SetReadDeadline(at time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.readAt = at
	c.armLocked(true)
	return nil
}
func (c *sshConn) SetWriteDeadline(at time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.writeAt = at
	c.armLocked(false)
	return nil
}

type sshAddr string

func (a sshAddr) Network() string       { return "ssh-unix" }
func (a sshAddr) String() string        { return string(a) }
func (c *sshConn) LocalAddr() net.Addr  { return sshAddr("local") }
func (c *sshConn) RemoteAddr() net.Addr { return sshAddr("configured-relay") }

func (r *SSHRelay) request(ctx context.Context, method, path, token string, input, output any) error {
	var body io.Reader
	if input != nil {
		body = bytes.NewReader(jsonData(input))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://vantry-relay"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("rtc: your relay is unavailable; check SSH authentication and the relay service")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("rtc: your relay rejected the request (HTTP %d)", resp.StatusCode)
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}
func (r *SSHRelay) Create(ctx context.Context, v VideoConfig) (SessionInfo, error) {
	var info SessionInfo
	err := r.request(ctx, "POST", "/v1/sessions", "", map[string]any{"video": v}, &info)
	if err == nil {
		err = validateSession(info, true)
		if err == nil && info.Transport == "lan" {
			err = errors.New("rtc: configured relay returned a LAN session")
		}
	}
	if err == nil {
		err = r.prepareTURN(&info)
	}
	return info, err
}
func (r *SSHRelay) Get(ctx context.Context, id, token string) (SessionInfo, error) {
	if !safeID.MatchString(id) {
		return SessionInfo{}, errors.New("rtc: invalid session identity")
	}
	var info SessionInfo
	err := r.request(ctx, "GET", "/v1/sessions/"+id, token, nil, &info)
	if err == nil {
		err = validateSession(info, false)
		if err == nil && info.Transport == "lan" {
			err = errors.New("rtc: configured relay returned a LAN session")
		}
	}
	if err == nil {
		err = r.prepareTURN(&info)
	}
	return info, err
}
func (r *SSHRelay) Delete(ctx context.Context, id, token string) error {
	if !safeID.MatchString(id) {
		return errors.New("rtc: invalid session identity")
	}
	return r.request(ctx, "DELETE", "/v1/sessions/"+id, token, nil, nil)
}
func (r *SSHRelay) Signal(ctx context.Context, id, role, token string) (*SignalConn, error) {
	if !safeID.MatchString(id) || (role != "host" && role != "viewer") {
		return nil, errors.New("rtc: invalid signaling role")
	}
	dialer := websocket.Dialer{NetDialContext: r.dial, HandshakeTimeout: 15 * time.Second}
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	address := "ws://vantry-relay/v1/sessions/" + url.PathEscape(id) + "/signal?role=" + role
	conn, resp, err := dialer.DialContext(ctx, address, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, errors.New("rtc: relay signaling connection failed")
	}
	conn.SetReadLimit(64 << 10)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return NewSignalConn(conn, func() { stop() }), nil
}
func validateSession(info SessionInfo, host bool) error {
	if !safeID.MatchString(info.SessionID) {
		return errors.New("rtc: relay returned an invalid session")
	}
	if host && (len(info.HostToken) < 32 || len(info.ViewerToken) < 32) {
		return errors.New("rtc: relay returned incomplete session authentication")
	}
	return info.validateICE()
}
