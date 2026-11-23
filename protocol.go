// Package remotedesktop provides screen capture, remote input, and authenticated
// direct or relayed viewing without depending on the desktop UI.
package remotedesktop

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
)

// FrameType identifies a packet in the native stream.
type FrameType byte

const (
	FrameHello FrameType = iota + 1
	FrameAuthOK
	FrameAuthErr
	FrameVideoJPEG
	FrameDisplayInfo
	FrameInputMouse
	FrameInputWheel
	FrameInputKey
	FramePing
	FramePong
)

const maxFrameBytes = 32 << 20

var (
	ErrUnauthorized = errors.New("remote desktop: invalid bearer token")
	ErrClosed       = errors.New("remote desktop: session closed")
	ErrPlatform     = errors.New("remote desktop: local capture and input require macOS")
)

// Frame is encoded as one type byte, a four-byte big-endian size, and Payload.
type Frame struct {
	Type    FrameType
	Payload []byte
}

func WriteFrame(w io.Writer, kind FrameType, payload []byte) error {
	if kind < FrameHello || kind > FramePong || len(payload) > maxFrameBytes {
		return errors.New("remote desktop: invalid frame type or size")
	}
	var header [5]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, payload)
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func ReadFrame(r io.Reader) (Frame, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	kind, size := FrameType(header[0]), binary.BigEndian.Uint32(header[1:])
	if kind < FrameHello || kind > FramePong || size > maxFrameBytes {
		return Frame{}, errors.New("remote desktop: invalid frame type or size")
	}
	payload := make([]byte, int(size))
	_, err := io.ReadFull(r, payload)
	return Frame{Type: kind, Payload: payload}, err
}

// QualityPreset controls capture size, JPEG encoding, and capture frequency.
type QualityPreset struct {
	Name        string
	Scale       float64
	JPEGQuality int
	FPS         int
}

func Quality(name string) (QualityPreset, error) {
	switch name {
	case "ultra":
		return QualityPreset{"ultra", 1, 85, 30}, nil
	case "", "balanced":
		return QualityPreset{"balanced", .75, 72, 24}, nil
	case "low":
		return QualityPreset{"low", .5, 55, 15}, nil
	default:
		return QualityPreset{}, fmt.Errorf("remote desktop: unknown quality %q", name)
	}
}

// MouseEvent uses normalized display coordinates. Buttons are 0=left, 1=right,
// 2=middle; Kind is move, down, up, or drag.
type MouseEvent struct {
	Kind      string  `json:"kind"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Button    int     `json:"button"`
	DisplayID uint32  `json:"displayId,omitempty"`
}

type WheelEvent struct {
	DX float64 `json:"dx"`
	DY float64 `json:"dy"`
}

// KeyEvent uses macOS virtual key codes for physical keys, or UTF-8 Text for
// text entry. Modifiers uses CoreGraphics event flag bits.
type KeyEvent struct {
	Kind      string `json:"kind"`
	KeyCode   int    `json:"keyCode"`
	Text      string `json:"text,omitempty"`
	Modifiers uint32 `json:"modifiers,omitempty"`
}

type hello struct {
	Token     string `json:"token"`
	Quality   string `json:"quality,omitempty"`
	DisplayID uint32 `json:"displayId,omitempty"`
}

func jsonFrame(kind FrameType, value any) (Frame, error) {
	data, err := json.Marshal(value)
	return Frame{Type: kind, Payload: data}, err
}

func newToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validToken(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 32
}

func sameToken(expected, received string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(received)) == 1
}

func validSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func validateInput(f Frame) error {
	if len(f.Payload) > 16<<10 {
		return errors.New("remote desktop: input too large")
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	switch f.Type {
	case FrameInputMouse:
		var e MouseEvent
		if err := json.Unmarshal(f.Payload, &e); err != nil {
			return err
		}
		if (e.Kind != "move" && e.Kind != "down" && e.Kind != "up" && e.Kind != "drag") ||
			!finite(e.X) || !finite(e.Y) || e.X < 0 || e.X > 1 || e.Y < 0 || e.Y > 1 || e.Button < 0 || e.Button > 2 {
			return errors.New("remote desktop: invalid mouse event")
		}
	case FrameInputWheel:
		var e WheelEvent
		if err := json.Unmarshal(f.Payload, &e); err != nil {
			return err
		}
		if !finite(e.DX) || !finite(e.DY) || math.Abs(e.DX) > 32767 || math.Abs(e.DY) > 32767 {
			return errors.New("remote desktop: invalid wheel event")
		}
	case FrameInputKey:
		var e KeyEvent
		if err := json.Unmarshal(f.Payload, &e); err != nil {
			return err
		}
		if (e.Kind != "down" && e.Kind != "up") || e.KeyCode < 0 || e.KeyCode > 65535 || len(e.Text) > 4096 {
			return errors.New("remote desktop: invalid key event")
		}
	default:
		return errors.New("remote desktop: invalid input type")
	}
	return nil
}

// sampledEqual compares a grid of pixels directly. It is a bandwidth heuristic:
// a small change between samples can be missed, so the host also refreshes a
// static frame periodically. Rows and nonzero image origins are respected.
func sampledEqual(a, b *image.RGBA) bool {
	if a == nil || b == nil || a.Bounds().Size() != b.Bounds().Size() {
		return false
	}
	w, h := a.Rect.Dx(), a.Rect.Dy()
	xstep, ystep := max(1, w/128), max(1, h/128)
	for y := 0; y < h; y += ystep {
		for x := 0; x < w; x += xstep {
			i := a.PixOffset(a.Rect.Min.X+x, a.Rect.Min.Y+y)
			j := b.PixOffset(b.Rect.Min.X+x, b.Rect.Min.Y+y)
			if a.Pix[i] != b.Pix[j] || a.Pix[i+1] != b.Pix[j+1] || a.Pix[i+2] != b.Pix[j+2] || a.Pix[i+3] != b.Pix[j+3] {
				return false
			}
		}
	}
	return true
}
