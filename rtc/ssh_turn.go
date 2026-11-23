package rtc

import (
	"errors"
	"io"
	"net"
	"strconv"
)

// prepareTURN maps the configured relay's loopback TURN/TCP route to a
// retained loopback listener on this machine. Media still traverses real TURN
// allocations and WebRTC DTLS-SRTP; SSH carries the TCP control/data stream when
// the cloud's public TURN port is not reachable. No system network is changed.
func (r *SSHRelay) prepareTURN(info *SessionInfo) error {
	if info.Transport != "ssh-tcp" {
		return nil
	}
	if info.TurnTCPPort < 1 || info.TurnTCPPort > 65535 {
		return errors.New("rtc: invalid relay loopback TURN port")
	}
	r.bridgeMu.Lock()
	defer r.bridgeMu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.turnListener != nil && r.turnPort != info.TurnTCPPort {
		return errors.New("rtc: active TURN route changed; reconnect the session")
	}
	if r.turnListener == nil {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		r.turnListener, r.turnPort = listener, info.TurnTCPPort
		r.bridgeWG.Add(1)
		go r.acceptTURN(listener, info.TurnTCPPort)
	}
	address := r.turnListener.Addr().String()
	for i := range info.ICEServers {
		info.ICEServers[i].URLs = []string{"turn:" + address + "?transport=tcp"}
	}
	return nil
}
func (r *SSHRelay) acceptTURN(listener net.Listener, port int) {
	defer r.bridgeWG.Done()
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		r.bridgeMu.Lock()
		if r.closed {
			r.bridgeMu.Unlock()
			_ = local.Close()
			return
		}
		r.turnConns[local] = struct{}{}
		r.bridgeWG.Add(1)
		r.bridgeMu.Unlock()
		go r.copyTURN(local, port)
	}
}
func (r *SSHRelay) copyTURN(local net.Conn, port int) {
	defer r.bridgeWG.Done()
	var remote net.Conn
	defer func() {
		_ = local.Close()
		if remote != nil {
			_ = remote.Close()
		}
		r.bridgeMu.Lock()
		delete(r.turnConns, local)
		if remote != nil {
			delete(r.turnConns, remote)
		}
		r.bridgeMu.Unlock()
	}()
	args := append(r.sshArgs(), "-W", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "--", r.cfg.Host)
	var err error
	remote, err = r.openSSH(r.ctx, args)
	if err != nil {
		return
	}
	r.bridgeMu.Lock()
	if r.closed {
		r.bridgeMu.Unlock()
		return
	}
	r.turnConns[remote] = struct{}{}
	r.bridgeMu.Unlock()
	sent := make(chan struct{})
	go func() { _, _ = io.Copy(remote, local); _ = remote.Close(); _ = local.Close(); close(sent) }()
	_, _ = io.Copy(local, remote)
	_ = local.Close()
	_ = remote.Close()
	<-sent
}
func (r *SSHRelay) closeTransport() {
	r.bridgeMu.Lock()
	if r.closed {
		r.bridgeMu.Unlock()
		return
	}
	r.closed = true
	r.cancel()
	if r.turnListener != nil {
		_ = r.turnListener.Close()
	}
	connections := make([]net.Conn, 0, len(r.turnConns))
	for connection := range r.turnConns {
		connections = append(connections, connection)
	}
	r.bridgeMu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
	r.transport.CloseIdleConnections()
	r.bridgeWG.Wait()
}
