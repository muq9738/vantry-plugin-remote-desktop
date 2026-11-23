//go:build darwin

package remotedesktop

import (
	"context"
	"errors"
	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
	"math"
	"sync"
	"time"
	"unicode/utf16"
)

const inputPermissionMessage = "需要开启 macOS「辅助功能」权限（系统设置 → 隐私与安全性 → 辅助功能）"

type clickTracker struct {
	at      time.Time
	point   cgPoint
	display uint32
	count   int64
}
type nativeInputSink struct {
	api           *coreGraphics
	mouseMu       sync.Mutex
	clicks        [3]clickTracker
	clickInterval time.Duration
}

func NewInputSink() (InputSink, error) {
	api, err := loadGraphics()
	if err != nil {
		return nil, err
	}
	if api.canPost != nil && !api.canPost() {
		return nil, errors.New(inputPermissionMessage)
	}
	interval, err := systemDoubleClickInterval()
	if err != nil {
		return nil, err
	}
	return &nativeInputSink{api: api, clickInterval: interval}, nil
}

func systemDoubleClickInterval() (time.Duration, error) {
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_LAZY|purego.RTLD_LOCAL); err != nil {
		return 0, err
	}
	interval := objc.Send[float64](objc.ID(objc.GetClass("NSEvent")), objc.RegisterName("doubleClickInterval"))
	return time.Duration(interval * float64(time.Second)), nil
}
func (s *nativeInputSink) allowed(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.api.canPost != nil && !s.api.canPost() {
		return errors.New(inputPermissionMessage)
	}
	return nil
}

func (s *nativeInputSink) post(event uintptr) error {
	if event == 0 {
		return errors.New(inputPermissionMessage)
	}
	defer s.api.release(event)
	s.api.postEvent(0, event) // kCGHIDEventTap
	return nil
}

func (s *nativeInputSink) Mouse(ctx context.Context, e MouseEvent) error {
	f, err := jsonFrame(FrameInputMouse, e)
	if err == nil {
		err = validateInput(f)
	}
	if err != nil {
		return err
	}
	if err := s.allowed(ctx); err != nil {
		return err
	}
	if e.DisplayID == 0 {
		e.DisplayID = s.api.mainDisplay()
	}
	bounds := s.api.displayBounds(e.DisplayID)
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return errors.New("remote desktop: display is no longer active")
	}
	point := cgPoint{bounds.X + e.X*math.Max(0, bounds.Width-1), bounds.Y + e.Y*math.Max(0, bounds.Height-1)}
	var kind uint32
	switch e.Kind {
	case "move":
		kind = 5
	case "down":
		kind = []uint32{1, 3, 25}[e.Button]
	case "up":
		kind = []uint32{2, 4, 26}[e.Button]
	case "drag":
		kind = []uint32{6, 7, 27}[e.Button]
	}
	s.mouseMu.Lock()
	defer s.mouseMu.Unlock()
	event := s.api.mouseEvent(0, kind, point, uint32(e.Button))
	if event == 0 {
		return errors.New(inputPermissionMessage)
	}
	if e.Kind == "down" || e.Kind == "up" {
		click := &s.clicks[e.Button]
		now := time.Now()
		if e.Kind == "down" {
			if click.display == e.DisplayID && now.Sub(click.at) <= s.clickInterval && math.Hypot(point.X-click.point.X, point.Y-click.point.Y) <= 4 {
				click.count++
			} else {
				click.count = 1
			}
			click.at, click.point, click.display = now, point, e.DisplayID
		}
		s.api.setInteger(event, 1, max(click.count, 1)) // kCGMouseEventClickState
	}
	return s.post(event)
}

func (s *nativeInputSink) Wheel(ctx context.Context, e WheelEvent) error {
	f, err := jsonFrame(FrameInputWheel, e)
	if err == nil {
		err = validateInput(f)
	}
	if err != nil {
		return err
	}
	if err := s.allowed(ctx); err != nil {
		return err
	}
	// Browser and client deltas are positive down/right; CoreGraphics is the
	// opposite. Pixel units avoid multiplying browser deltas by line height.
	return s.post(s.api.scrollEvent(0, 0, 2, -int32(math.Round(e.DY)), -int32(math.Round(e.DX))))
}

func (s *nativeInputSink) Key(ctx context.Context, e KeyEvent) error {
	f, err := jsonFrame(FrameInputKey, e)
	if err == nil {
		err = validateInput(f)
	}
	if err != nil {
		return err
	}
	if err := s.allowed(ctx); err != nil {
		return err
	}
	event := s.api.keyEvent(0, uint16(e.KeyCode), e.Kind == "down")
	if event == 0 {
		return errors.New(inputPermissionMessage)
	}
	defer s.api.release(event)
	s.api.setFlags(event, uint64(e.Modifiers))
	if e.Text != "" && e.Kind == "down" {
		text := utf16.Encode([]rune(e.Text))
		s.api.keyUnicode(event, uintptr(len(text)), &text[0])
	}
	s.api.postEvent(0, event)
	return nil
}
