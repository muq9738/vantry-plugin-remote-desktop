//go:build windows

package remotedesktop

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var inputUser32 = windows.NewLazySystemDLL("user32.dll")
var winSendInput = inputUser32.NewProc("SendInput")
var winEnumMonitors = inputUser32.NewProc("EnumDisplayMonitors")
var winMonitorInfo = inputUser32.NewProc("GetMonitorInfoW")
var winGetMetrics = inputUser32.NewProc("GetSystemMetrics")
var winThreadDPI = inputUser32.NewProc("SetThreadDpiAwarenessContext")
var winOpenDesktop = inputUser32.NewProc("OpenInputDesktop")
var winCloseDesktop = inputUser32.NewProc("CloseDesktop")

type winPointRect struct{ Left, Top, Right, Bottom int32 }
type winMonitor struct {
	Size         uint32
	Bounds, Work winPointRect
	Flags        uint32
	Device       [32]uint16
}
type winMouseData struct {
	DX, DY            int32
	Data, Flags, Time uint32
	Extra             uintptr
}
type winKeyData struct {
	VK, Scan    uint16
	Flags, Time uint32
	Extra       uintptr
}
type winEvent struct {
	Type  uint32
	Mouse winMouseData
}
type windowsInputSink struct {
	mu          sync.Mutex
	pressed     map[uint16]bool
	extended    map[uint16]bool
	unicodeDown map[int]bool
	dispatch    func([]winEvent) (int, error)
	buttons     map[int]bool
}

// NewInputSink uses ordinary SendInput; it never requests elevation or controls
// a secure desktop. The OS's UIPI boundary still applies to every target.
func NewInputSink() (InputSink, error) {
	if ok, err := ProbeInputAccess(context.Background()); err != nil || !ok {
		return nil, errors.New("Windows 当前桌面不可接收输入，请解锁并切回交互会话")
	}
	return &windowsInputSink{pressed: map[uint16]bool{}, extended: map[uint16]bool{}, unicodeDown: map[int]bool{}, buttons: map[int]bool{}}, nil
}
func ProbeInputAccess(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	handle, _, err := winOpenDesktop.Call(0, 0, 0x0001) // DESKTOP_READOBJECTS, no switch/elevation
	if handle == 0 {
		return false, err
	}
	_, _, _ = winCloseDesktop.Call(handle)
	return true, nil
}
func RequestInputAccess(ctx context.Context) (bool, error) { return ProbeInputAccess(ctx) }
func (s *windowsInputSink) submit(events []winEvent) error {
	if len(events) == 0 {
		return nil
	}
	dispatch := s.dispatch
	if dispatch == nil {
		dispatch = dispatchWindowsInput
	}
	n, err := dispatch(events)
	count := uintptr(max(n, 0))
	for _, event := range events[:min(int(count), len(events))] {
		if event.Type == 1 {
			key := *(*winKeyData)(unsafe.Pointer(&event.Mouse))
			if key.Flags&4 == 0 {
				if key.Flags&2 != 0 {
					delete(s.pressed, key.VK)
					delete(s.extended, key.VK)
				} else {
					s.pressed[key.VK] = true
					s.extended[key.VK] = key.Flags&1 != 0
				}
			}
		} else {
			for button, pair := range [][2]uint32{{2, 4}, {8, 16}, {32, 64}} {
				if event.Mouse.Flags&pair[0] != 0 {
					s.buttons[button] = true
				}
				if event.Mouse.Flags&pair[1] != 0 {
					delete(s.buttons, button)
				}
			}
		}
	}
	if count != uintptr(len(events)) {
		return fmt.Errorf("Windows 未接受全部输入（目标权限或桌面状态不匹配）: %d/%d, %v", count, len(events), err)
	}
	return nil
}
func dispatchWindowsInput(events []winEvent) (int, error) {
	count, _, err := winSendInput.Call(uintptr(len(events)), uintptr(unsafe.Pointer(&events[0])), unsafe.Sizeof(winEvent{}))
	runtime.KeepAlive(events)
	return int(count), err
}
func windowsKey(vk, scan uint16, flags uint32) winEvent {
	event := winEvent{Type: 1}
	*(*winKeyData)(unsafe.Pointer(&event.Mouse)) = winKeyData{VK: vk, Scan: scan, Flags: flags}
	return event
}
func windowsMouse(flags uint32, data uint32) winEvent {
	return winEvent{Mouse: winMouseData{Flags: flags, Data: data}}
}
func windowsDPI() func() {
	runtime.LockOSThread()
	previous, _, _ := winThreadDPI.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
	return func() {
		if previous != 0 {
			_, _, _ = winThreadDPI.Call(previous)
		}
		runtime.UnlockOSThread()
	}
}

type windowsDisplayQuery struct {
	id     uint32
	bounds winPointRect
	found  bool
}

// Go callbacks occupy a permanent runtime slot: register this one once for all
// pointer events and pass the short-lived query through EnumDisplayMonitors.
var windowsDisplayQueries = struct {
	sync.Mutex
	next  uintptr
	items map[uintptr]*windowsDisplayQuery
}{items: map[uintptr]*windowsDisplayQuery{}}
var windowsDisplayCallback = syscall.NewCallback(func(monitor, dc, rect, param uintptr) uintptr {
	windowsDisplayQueries.Lock()
	query := windowsDisplayQueries.items[param]
	windowsDisplayQueries.Unlock()
	if query == nil {
		return 0
	}
	info := winMonitor{Size: uint32(unsafe.Sizeof(winMonitor{}))}
	ok, _, _ := winMonitorInfo.Call(monitor, uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return 1
	}
	name := windows.UTF16ToString(info.Device[:])
	number, _ := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(name), `\\.\DISPLAY`), 10, 32)
	if (query.id == 0 && info.Flags&1 != 0) || (query.id != 0 && uint32(number) == query.id) {
		query.bounds = info.Bounds
		query.found = true
		return 0
	}
	return 1
})

func windowsDisplay(id uint32) (winPointRect, error) {
	query := &windowsDisplayQuery{id: id}
	windowsDisplayQueries.Lock()
	windowsDisplayQueries.next++
	handle := windowsDisplayQueries.next
	windowsDisplayQueries.items[handle] = query
	windowsDisplayQueries.Unlock()
	defer func() {
		windowsDisplayQueries.Lock()
		delete(windowsDisplayQueries.items, handle)
		windowsDisplayQueries.Unlock()
	}()
	_, _, _ = winEnumMonitors.Call(0, 0, windowsDisplayCallback, handle)
	if !query.found {
		return query.bounds, errors.New("Windows 源显示器已不可用")
	}
	return query.bounds, nil
}
func metric(index int32) int32 {
	value, _, _ := winGetMetrics.Call(uintptr(index))
	return int32(value)
}
func (s *windowsInputSink) Mouse(ctx context.Context, event MouseEvent) error {
	frame, err := jsonFrame(FrameInputMouse, event)
	if err == nil {
		err = validateInput(frame)
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	restore := windowsDPI()
	defer restore()
	display, err := windowsDisplay(event.DisplayID)
	if err != nil {
		return err
	}
	left, top, width, height := metric(76), metric(77), metric(78), metric(79) // virtual-screen physical pixels
	if width < 2 || height < 2 {
		return errors.New("Windows 虚拟桌面当前不可用")
	}
	x := float64(display.Left) + event.X*float64(display.Right-display.Left-1)
	y := float64(display.Top) + event.Y*float64(display.Bottom-display.Top-1)
	flags := uint32(0x0001 | 0x8000 | 0x4000) // MOVE | ABSOLUTE | VIRTUALDESK
	data := uint32(0)
	if event.Kind == "down" {
		flags |= []uint32{0x0002, 0x0008, 0x0020}[event.Button]
	}
	if event.Kind == "up" {
		flags |= []uint32{0x0004, 0x0010, 0x0040}[event.Button]
	}
	input := winEvent{Mouse: winMouseData{DX: int32(math.Round((x - float64(left)) * 65535 / float64(width-1))), DY: int32(math.Round((y - float64(top)) * 65535 / float64(height-1))), Flags: flags, Data: data}}
	if err = s.submit([]winEvent{input}); err != nil {
		return err
	}
	if event.Kind == "down" {
		s.buttons[event.Button] = true
	} else if event.Kind == "up" {
		delete(s.buttons, event.Button)
	}
	return nil
}
func (s *windowsInputSink) Wheel(ctx context.Context, event WheelEvent) error {
	frame, err := jsonFrame(FrameInputWheel, event)
	if err == nil {
		err = validateInput(frame)
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []winEvent
	// The wire is pixels; 40 pixels represent the conventional 120-unit notch.
	if event.DY != 0 {
		events = append(events, windowsMouse(0x0800, uint32(int32(math.Round(-event.DY*3)))))
	}
	if event.DX != 0 {
		events = append(events, windowsMouse(0x1000, uint32(int32(math.Round(event.DX*3)))))
	}
	return s.submit(events)
}

var windowsMacKeys = map[int]uint16{
	0: 0x41, 1: 0x53, 2: 0x44, 3: 0x46, 4: 0x48, 5: 0x47, 6: 0x5A, 7: 0x58, 8: 0x43, 9: 0x56, 11: 0x42, 12: 0x51, 13: 0x57, 14: 0x45, 15: 0x52, 16: 0x59, 17: 0x54,
	18: 0x31, 19: 0x32, 20: 0x33, 21: 0x34, 22: 0x36, 23: 0x35, 24: 0xBB, 25: 0x39, 26: 0x37, 27: 0xBD, 28: 0x38, 29: 0x30, 30: 0xDD, 31: 0x4F, 32: 0x55, 33: 0xDB, 34: 0x49, 35: 0x50,
	36: 0x0D, 37: 0x4C, 38: 0x4A, 39: 0xDE, 40: 0x4B, 41: 0xBA, 42: 0xDC, 43: 0xBC, 44: 0xBF, 45: 0x4E, 46: 0x4D, 47: 0xBE, 48: 0x09, 49: 0x20, 50: 0xC0, 51: 0x08, 53: 0x1B,
	54: 0x5C, 55: 0x5B, 56: 0xA0, 57: 0x14, 58: 0xA4, 59: 0xA2, 60: 0xA1, 61: 0xA5, 62: 0xA3,
	65: 0x6E, 67: 0x6A, 69: 0x6B, 71: 0x90, 75: 0x6F, 76: 0x0D, 78: 0x6D, 81: 0xBB, 82: 0x60, 83: 0x61, 84: 0x62, 85: 0x63, 86: 0x64, 87: 0x65, 88: 0x66, 89: 0x67, 91: 0x68, 92: 0x69,
	96: 0x74, 97: 0x75, 98: 0x76, 99: 0x72, 100: 0x77, 101: 0x78, 103: 0x7A, 105: 0x7C, 106: 0x7F, 107: 0x7D, 109: 0x79, 111: 0x7B, 113: 0x7E, 114: 0x2D, 115: 0x24, 116: 0x21, 117: 0x2E, 118: 0x73, 119: 0x23, 120: 0x71, 121: 0x22, 122: 0x70, 123: 0x25, 124: 0x27, 125: 0x28, 126: 0x26,
}

func windowsUnicodeEvents(text string) []winEvent {
	var events []winEvent
	for _, unit := range utf16.Encode([]rune(text)) {
		events = append(events, windowsKey(0, unit, 4), windowsKey(0, unit, 4|2))
	}
	return events
}
func windowsExtended(code int, vk uint16) bool {
	return code == 76 || vk == 0x6F || vk == 0x90 || vk == 0xA3 || vk == 0xA5 || vk == 0x5B || vk == 0x5C || (vk >= 0x21 && vk <= 0x28) || vk == 0x2D || vk == 0x2E
}
func windowsModifierEvents(pressed map[uint16]bool, desired uint32) []winEvent {
	var events []winEvent
	for _, group := range []struct {
		flag        uint32
		left, right uint16
	}{{1 << 17, 0xA0, 0xA1}, {1 << 18, 0xA2, 0xA3}, {1 << 19, 0xA4, 0xA5}, {1 << 20, 0x5B, 0x5C}} {
		allowed := desired&group.flag != 0
		if allowed && !pressed[group.left] && !pressed[group.right] {
			flags := uint32(0)
			if group.left == 0x5B {
				flags = 1
			}
			events = append(events, windowsKey(group.left, 0, flags))
		}
		if !allowed {
			for _, key := range []uint16{group.left, group.right} {
				if pressed[key] {
					flags := uint32(2)
					if windowsExtended(-1, key) {
						flags |= 1
					}
					events = append(events, windowsKey(key, 0, flags))
				}
			}
		}
	}
	return events
}
func (s *windowsInputSink) Key(ctx context.Context, event KeyEvent) error {
	frame, err := jsonFrame(FrameInputKey, event)
	if err == nil {
		err = validateInput(frame)
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Kind == "up" && s.unicodeDown[event.KeyCode] {
		delete(s.unicodeDown, event.KeyCode)
		vk, known := windowsMacKeys[event.KeyCode]
		if !known || !s.pressed[vk] {
			return nil
		}
	}
	var events []winEvent
	if event.Text != "" {
		if event.Kind != "down" {
			return nil
		}
		events = windowsUnicodeEvents(event.Text)
		if err = s.submit(events); err == nil {
			s.unicodeDown[event.KeyCode] = true
		}
		return err
	}
	vk, ok := windowsMacKeys[event.KeyCode]
	if !ok {
		return fmt.Errorf("Windows 不支持此远程键码 %d", event.KeyCode)
	}
	// Modifier key transitions arrive explicitly from the viewport. For callers
	// supplying only CG flags, synchronize ordinary modifier state as well.
	modifier := vk >= 0xA0 && vk <= 0xA5 || vk == 0x5B || vk == 0x5C
	if !modifier {
		events = append(events, windowsModifierEvents(s.pressed, event.Modifiers)...)
	}
	flags := uint32(0)
	if event.Kind == "up" {
		flags |= 2
	}
	if windowsExtended(event.KeyCode, vk) {
		flags |= 1
	}
	events = append(events, windowsKey(vk, 0, flags))
	if err = s.submit(events); err != nil {
		return err
	}
	return nil
}

// Release clears only keys/buttons injected by this sink, never arbitrary local
// input. Host cleanup calls it after normal per-session release events.
func (s *windowsInputSink) Release(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []winEvent
	for key := range s.pressed {
		flags := uint32(2)
		if s.extended[key] {
			flags |= 1
		}
		events = append(events, windowsKey(key, 0, flags))
	}
	for button := range s.buttons {
		events = append(events, windowsMouse([]uint32{0x0004, 0x0010, 0x0040}[button], 0))
	}
	err := s.submit(events)
	if err == nil {
		s.pressed = map[uint16]bool{}
		s.extended = map[uint16]bool{}
		s.buttons = map[int]bool{}
		s.unicodeDown = map[int]bool{}
	}
	return err
}
