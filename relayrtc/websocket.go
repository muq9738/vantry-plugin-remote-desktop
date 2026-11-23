package relayrtc

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxSignalBytes = 64 << 10

type wsConn struct {
	net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
}

func headerToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !headerToken(r.Header.Get("Connection"), "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("invalid websocket upgrade")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) != 16 {
		http.Error(w, "invalid websocket key", 400)
		return nil, errors.New("invalid websocket key")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "upgrade unavailable", 500)
		return nil, errors.New("HTTP hijacking unavailable")
	}
	conn, buffer, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	// RFC 6455 mandates this accept calculation. It is a wire-protocol
	// handshake, not an application checksum or compatibility gate.
	accept := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, err = fmt.Fprintf(buffer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
	if err == nil {
		err = buffer.Flush()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &wsConn{Conn: conn, reader: buffer.Reader}, nil
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	header := []byte{0x80 | op}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(len(payload)))
	}
	buffers := net.Buffers{header, payload}
	_, err := buffers.WriteTo(c.Conn)
	return err
}

func (c *wsConn) frame() (byte, bool, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return 0, false, nil, err
	}
	op, fin := header[0]&15, header[0]&0x80 != 0
	if header[0]&0x70 != 0 || header[1]&0x80 == 0 {
		return 0, false, nil, errors.New("invalid websocket frame")
	}
	n := uint64(header[1] & 127)
	if n == 126 {
		var size [2]byte
		if _, err := io.ReadFull(c.reader, size[:]); err != nil {
			return 0, false, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(size[:]))
	}
	if n == 127 {
		var size [8]byte
		if _, err := io.ReadFull(c.reader, size[:]); err != nil {
			return 0, false, nil, err
		}
		n = binary.BigEndian.Uint64(size[:])
	}
	if n > maxSignalBytes || op >= 8 && (!fin || n > 125) {
		return 0, false, nil, errors.New("websocket frame limit")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
		return 0, false, nil, err
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return 0, false, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, fin, payload, nil
}

func (c *wsConn) readMessage() ([]byte, error) {
	var message []byte
	fragmented := false
	for {
		_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
		op, fin, payload, err := c.frame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 8:
			_ = c.writeFrame(8, payload)
			return nil, io.EOF
		case 9:
			if err = c.writeFrame(10, payload); err != nil {
				return nil, err
			}
			continue
		case 10:
			continue
		case 1:
			if fragmented {
				return nil, errors.New("nested websocket message")
			}
			message = append(message, payload...)
			fragmented = !fin
		case 0:
			if !fragmented {
				return nil, errors.New("unexpected websocket continuation")
			}
			message = append(message, payload...)
			fragmented = !fin
		default:
			return nil, errors.New("signaling requires text websocket messages")
		}
		if len(message) > maxSignalBytes {
			return nil, errors.New("signaling message limit")
		}
		if !fragmented {
			if !utf8.Valid(message) {
				return nil, errors.New("invalid signaling UTF-8")
			}
			return message, nil
		}
	}
}
