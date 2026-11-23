package relayrtc

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Serve binds only a private Unix socket. No TCP/HTTP control listener exists.
func Serve(ctx context.Context, config Config) error {
	service, err := New(config)
	if err != nil {
		return err
	}
	defer service.Close()
	path := service.config.Socket
	if !filepath.IsAbs(path) {
		return errors.New("signaling socket must be an absolute path")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	httpServer := &http.Server{Handler: service, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = service.Close()
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(shutdown)
		case <-finished:
		}
	}()
	err = httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

// Proxy is the SSH stdio transport. Stdout contains only the Unix-socket
// stream; protocol bodies, authorization and SDP are never application logs.
func Proxy(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-finished:
		}
	}()
	go func() {
		_, _ = io.Copy(conn, in)
		if unix, ok := conn.(*net.UnixConn); ok {
			_ = unix.CloseWrite()
		}
	}()
	_, err = io.Copy(out, conn)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
