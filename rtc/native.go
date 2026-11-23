package rtc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	nativeHello byte = 1
	nativeH264  byte = 2
	nativeRGBA  byte = 3
	nativeError byte = 127
)

type NativeInfo struct {
	Protocol            int    `json:"protocol"`
	Mode                string `json:"mode"`
	Codec               string `json:"codec"`
	Hardware            bool   `json:"hardware"`
	ScreenCaptureAccess string `json:"screenCaptureAccess"`
	Width               int    `json:"width"`
	Height              int    `json:"height"`
	FPS                 int    `json:"fps"`
	ProfileLevelID      string `json:"profileLevelId"`
}
type NativePermissionError struct{ Message string }

func (e *NativePermissionError) Error() string { return e.Message }

func readNative(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxNativePayload {
		return 0, nil, errors.New("rtc: native frame exceeds size limit")
	}
	b := make([]byte, int(n))
	_, err := io.ReadFull(r, b)
	return h[0], b, err
}
func writeNative(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > MaxNativePayload {
		return errors.New("rtc: native frame exceeds size limit")
	}
	var h [5]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	if err := writeFull(w, h[:]); err != nil {
		return err
	}
	return writeFull(w, payload)
}
func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func nativeFailure(b []byte) error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) != nil {
		return errors.New("rtc: native video operation failed")
	}
	if e.Code == "screen_recording" || e.Code == "screen_permission" || e.Code == "screen_permission_denied" || e.Code == "permission_denied" {
		return &NativePermissionError{e.Message}
	}
	return fmt.Errorf("rtc: native %s: %s", e.Code, e.Message)
}
func helperPath(path string) (string, error) {
	if path == "" {
		path = os.Getenv("VANTRY_REMOTE_HELPER")
	}
	if path == "" {
		return "", errors.New("rtc: native helper not packaged; expected remote-desktop/vantry-remote-helper")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode()&0111 == 0) {
		return "", errors.New("rtc: native video helper is missing or not executable")
	}
	return path, nil
}
func ProbeNative(ctx context.Context, path string) (NativeInfo, error) {
	path, err := helperPath(path)
	if err != nil {
		return NativeInfo{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, path, "probe")
	configureChildProcess(command)
	command.Stderr = io.Discard
	out, err := command.Output()
	if err != nil {
		return NativeInfo{}, errors.New("rtc: native video capability probe failed")
	}
	reader := bufio.NewReaderSize(bytes.NewReader(out), 4096)
	for {
		kind, b, err := readNative(reader)
		if err != nil {
			return NativeInfo{}, err
		}
		if kind == nativeError {
			return NativeInfo{}, nativeFailure(b)
		}
		if kind != nativeHello {
			continue
		}
		var info NativeInfo
		if err = json.Unmarshal(b, &info); err != nil {
			return info, err
		}
		if info.Protocol != 1 || info.Codec != "h264" || !info.Hardware {
			return info, errors.New("rtc: hardware H.264 is unavailable")
		}
		return info, nil
	}
}

type nativeProcess struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	in       io.WriteCloser
	out      *bufio.Reader
	stdout   io.ReadCloser
	done     chan struct{}
	once     sync.Once
	waitOnce sync.Once
	writes   sync.Mutex
	waitErr  error
}

func startNative(ctx context.Context, path, mode string, args ...string) (*nativeProcess, error) {
	path, err := helperPath(path)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(life, path, append([]string{mode}, args...)...)
	configureChildProcess(cmd)
	cmd.Stderr = io.Discard
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, errors.New("rtc: native video helper could not start")
	}
	p := &nativeProcess{cmd: cmd, cancel: cancel, in: in, out: bufio.NewReaderSize(out, 64<<10), stdout: out, done: make(chan struct{})}
	return p, nil
}
func (p *nativeProcess) wait() { p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait(); close(p.done) }) }
func (p *nativeProcess) Close() error {
	p.once.Do(func() {
		_ = p.in.Close()
		p.cancel()
		_ = p.stdout.Close()
		go p.wait()
		select {
		case <-p.done:
		case <-time.After(2500 * time.Millisecond):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return nil
}
func (p *nativeProcess) control(v any) error {
	p.writes.Lock()
	defer p.writes.Unlock()
	return writeFull(p.in, append(jsonData(v), '\n'))
}

type NativeSource struct {
	p    *nativeProcess
	Info NativeInfo
}

func NewNativeSource(ctx context.Context, path string, v VideoConfig, patternFrames int) (*NativeSource, error) {
	mode := "capture"
	if patternFrames > 0 {
		mode = "pattern"
	}
	return startNativeSource(ctx, path, v, mode, patternFrames, false)
}
func NewPatternSource(ctx context.Context, path string, v VideoConfig, frames int, noise bool) (*NativeSource, error) {
	return startNativeSource(ctx, path, v, "pattern", frames, noise)
}
func startNativeSource(ctx context.Context, path string, v VideoConfig, mode string, patternFrames int, noise bool) (*NativeSource, error) {
	args := []string{"--width", strconv.Itoa(v.Width), "--height", strconv.Itoa(v.Height), "--fps", strconv.Itoa(v.FPS), "--bitrate", strconv.Itoa(v.Bitrate), "--queue", "2"}
	if mode == "pattern" {
		args = append(args, "--frames", strconv.Itoa(patternFrames))
	} else {
		args = append(args, "--display", strconv.Itoa(int(v.DisplayID)))
	}
	if noise {
		args = append(args, "--noise")
	}
	p, err := startNative(ctx, path, mode, args...)
	if err != nil {
		return nil, err
	}
	s := &NativeSource{p: p}
	kind, b, err := readNative(p.out)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	if kind == nativeError {
		_ = p.Close()
		return nil, nativeFailure(b)
	}
	if kind != nativeHello || json.Unmarshal(b, &s.Info) != nil || s.Info.Protocol != 1 || s.Info.Codec != "h264" || !s.Info.Hardware {
		_ = p.Close()
		return nil, errors.New("rtc: native encoder did not confirm hardware H.264")
	}
	// The helper reports the actual SPS. Its auto-selected level may be lower than the offer.
	if v.ProfileLevelID == h264ConstrainedHighProfileLevelID &&
		(len(s.Info.ProfileLevelID) != 6 || !strings.EqualFold(s.Info.ProfileLevelID[:4], v.ProfileLevelID[:4])) {
		_ = p.Close()
		return nil, errors.New("rtc: native encoder did not confirm H.264 Constrained High")
	}
	return s, nil
}
func (s *NativeSource) Next(ctx context.Context) (EncodedFrame, error) {
	for {
		if ctx.Err() != nil {
			return EncodedFrame{}, ctx.Err()
		}
		kind, b, err := readNative(s.p.out)
		if err != nil {
			s.p.wait()
			return EncodedFrame{}, err
		}
		if kind == nativeError {
			return EncodedFrame{}, nativeFailure(b)
		}
		if kind == nativeHello {
			continue
		}
		if kind != nativeH264 || len(b) < 10 {
			return EncodedFrame{}, errors.New("rtc: invalid native H.264 frame")
		}
		return EncodedFrame{CapturedAt: time.UnixMicro(int64(binary.BigEndian.Uint64(b))), KeyFrame: b[8] != 0, Data: b[9:]}, nil
	}
}
func (s *NativeSource) RequestKeyFrame() error {
	return s.p.control(map[string]any{"action": "keyframe"})
}
func (s *NativeSource) SetBitrate(bitrate int) error {
	return s.p.control(map[string]any{"action": "bitrate", "bitrate": bitrate})
}
func (s *NativeSource) Close() error { return s.p.Close() }

type NativeDecoder struct {
	p               *nativeProcess
	ready           chan error
	done            chan struct{}
	callback        func(DecodedFrame)
	writes          sync.Mutex
	once            sync.Once
	free            chan []byte
	eventMu         sync.Mutex
	keyFrameRequest func()
}

func NewNativeDecoder(ctx context.Context, path string, callback func(DecodedFrame)) (*NativeDecoder, error) {
	p, err := startNative(ctx, path, "decode", "--queue", "1", "--events")
	if err != nil {
		return nil, err
	}
	d := &NativeDecoder{p: p, callback: callback, ready: make(chan error, 1), done: make(chan struct{}), free: make(chan []byte, 4)}
	for i := 0; i < 4; i++ {
		d.free <- nil
	}
	go d.read()
	select {
	case err := <-d.ready:
		if err != nil {
			_ = d.Close()
			return nil, err
		}
		return d, nil
	case <-ctx.Done():
		_ = d.Close()
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		_ = d.Close()
		return nil, errors.New("rtc: native decoder startup timed out")
	}
}
func (d *NativeDecoder) SetKeyFrameRequest(fn func()) {
	d.eventMu.Lock()
	d.keyFrameRequest = fn
	d.eventMu.Unlock()
}
func (d *NativeDecoder) read() {
	defer close(d.done)
	defer d.p.Close()
	hello := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(d.p.out, header[:]); err != nil {
			if !hello {
				d.ready <- err
			}
			return
		}
		kind, n := header[0], binary.BigEndian.Uint32(header[1:])
		if n > MaxNativePayload {
			if !hello {
				d.ready <- errors.New("rtc: native frame too large")
			}
			return
		}
		if kind != nativeRGBA {
			b := make([]byte, int(n))
			if _, err := io.ReadFull(d.p.out, b); err != nil {
				if !hello {
					d.ready <- err
				}
				return
			}
			if kind == nativeError {
				if !hello {
					d.ready <- nativeFailure(b)
				}
				return
			}
			if kind == nativeHello {
				var info NativeInfo
				if json.Unmarshal(b, &info) != nil || info.Protocol != 1 || info.Codec != "h264" || !info.Hardware {
					if !hello {
						d.ready <- errors.New("rtc: decoder did not confirm hardware acceleration")
					}
					return
				}
				if !hello {
					hello = true
					d.ready <- nil
				}
			} else if kind == 4 {
				var event struct {
					Event string `json:"event"`
				}
				if json.Unmarshal(b, &event) == nil && event.Event == "need_keyframe" {
					d.eventMu.Lock()
					fn := d.keyFrameRequest
					d.eventMu.Unlock()
					if fn != nil {
						fn()
					}
				}
			}
			continue
		}
		if !hello || n < 16 {
			return
		}
		var prefix [16]byte
		if _, err := io.ReadFull(d.p.out, prefix[:]); err != nil {
			return
		}
		width, height := int(binary.BigEndian.Uint32(prefix[8:12])), int(binary.BigEndian.Uint32(prefix[12:16]))
		size := int(n) - 16
		if width < 1 || height < 1 || width > 8192 || height > 8192 || int64(width)*int64(height)*4 != int64(size) {
			return
		}
		var pixels []byte
		select {
		case pixels = <-d.free:
		default:
			if _, err := io.CopyN(io.Discard, d.p.out, int64(size)); err != nil {
				return
			}
			if d.callback != nil {
				d.callback(DecodedFrame{Dropped: true})
			}
			continue
		}
		if cap(pixels) < size {
			pixels = make([]byte, size)
		} else {
			pixels = pixels[:size]
		}
		if _, err := io.ReadFull(d.p.out, pixels); err != nil {
			return
		}
		var released sync.Once
		release := func() { released.Do(func() { d.free <- pixels }) }
		frame := DecodedFrame{CapturedAt: time.UnixMicro(int64(binary.BigEndian.Uint64(prefix[:8]))), DecodedAt: time.Now(), Image: &image.RGBA{Pix: pixels, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}, Release: release}
		if d.callback != nil {
			d.callback(frame)
		} else {
			release()
		}
	}
}
func (d *NativeDecoder) Decode(ctx context.Context, frame EncodedFrame) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case <-d.done:
		return errors.New("rtc: native decoder stopped")
	default:
	}
	var prefix [9]byte
	binary.BigEndian.PutUint64(prefix[:8], uint64(frame.CapturedAt.UnixMicro()))
	if frame.KeyFrame {
		prefix[8] = 1
	}
	var header [5]byte
	header[0] = nativeH264
	if len(frame.Data)+9 > MaxNativePayload {
		return errors.New("rtc: encoded frame too large")
	}
	binary.BigEndian.PutUint32(header[1:], uint32(len(frame.Data)+9))
	d.writes.Lock()
	defer d.writes.Unlock()
	if err := writeFull(d.p.in, header[:]); err != nil {
		return err
	}
	if err := writeFull(d.p.in, prefix[:]); err != nil {
		return err
	}
	return writeFull(d.p.in, frame.Data)
}
func (d *NativeDecoder) Close() error { d.once.Do(func() { _ = d.p.Close(); <-d.done }); return nil }
