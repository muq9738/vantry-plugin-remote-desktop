//go:build windows

package remotedesktop

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"unicode/utf16"
	"unsafe"
)

func TestWindowsInputNativeLayoutAndUnicode(t *testing.T) {
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		if unsafe.Sizeof(winEvent{}) != 40 || unsafe.Offsetof(winEvent{}.Mouse) != 8 || unsafe.Sizeof(winMonitor{}) != 104 {
			t.Fatal("Windows INPUT/MONITORINFOEX ABI is incorrect")
		}
	}
	text := "你好😀"
	units := utf16.Encode([]rune(text))
	events := windowsUnicodeEvents(text)
	if len(events) != len(units)*2 {
		t.Fatal("Unicode events were truncated")
	}
	for index, unit := range units {
		for step := 0; step < 2; step++ {
			event := events[index*2+step]
			key := *(*winKeyData)(unsafe.Pointer(&event.Mouse))
			if event.Type != 1 || key.VK != 0 || key.Scan != unit || key.Flags != uint32(4|step*2) {
				t.Fatal("Unicode surrogate or key-up flags corrupted")
			}
		}
	}
}
func TestWindowsInputModifierFlagsKeepPhysicalSide(t *testing.T) {
	if got := windowsModifierEvents(map[uint16]bool{0xA1: true}, 1<<17); len(got) != 0 {
		t.Fatal("right Shift synthesized an extra left Shift")
	}
	release := windowsModifierEvents(map[uint16]bool{0xA3: true}, 0)
	if len(release) != 1 {
		t.Fatal("right Control not released")
	}
	key := *(*winKeyData)(unsafe.Pointer(&release[0].Mouse))
	if key.VK != 0xA3 || key.Flags != 3 {
		t.Fatal("right Control lost extended key-up")
	}
	if windowsMacKeys[0] != 0x41 || windowsMacKeys[55] != 0x5B || windowsMacKeys[123] != 0x25 || !windowsExtended(76, 0x0D) {
		t.Fatal("wire Mac codes not translated for Windows")
	}
}
func TestWindowsInputDisplayLookupReusesOneRuntimeCallback(t *testing.T) {
	// No capture or input: repeated readonly monitor lookup must not exhaust
	// the runtime's permanent callback registry after ordinary mouse use.
	for index := 0; index < 2100; index++ {
		_, _ = windowsDisplay(0)
	}
	windowsDisplayQueries.Lock()
	remaining := len(windowsDisplayQueries.items)
	windowsDisplayQueries.Unlock()
	if remaining != 0 {
		t.Fatal("monitor enumeration contexts leaked")
	}
}
func TestWindowsInputUnicodeRepeatStillReleasesPhysicalKey(t *testing.T) {
	var submitted []winEvent
	sink := &windowsInputSink{pressed: map[uint16]bool{}, extended: map[uint16]bool{}, unicodeDown: map[int]bool{}, buttons: map[int]bool{}, dispatch: func(events []winEvent) (int, error) {
		submitted = append(submitted, events...)
		return len(events), nil
	}}
	for _, event := range []KeyEvent{{Kind: "down", KeyCode: 59}, {Kind: "down", KeyCode: 0, Modifiers: 1 << 18}, {Kind: "up", KeyCode: 59}, {Kind: "down", KeyCode: 0, Text: "a"}, {Kind: "up", KeyCode: 0}} {
		if err := sink.Key(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if sink.pressed[0x41] {
		t.Fatal("Unicode repeat swallowed the physical A key-up")
	}
	last := *(*winKeyData)(unsafe.Pointer(&submitted[len(submitted)-1].Mouse))
	if last.VK != 0x41 || last.Flags&2 == 0 {
		t.Fatal("physical key was not released after Unicode text")
	}
}
func TestWindowsInputCancelledActionsDoNotPost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := &windowsInputSink{}
	if err := sink.Key(ctx, KeyEvent{Kind: "down", KeyCode: 0}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := sink.Mouse(ctx, MouseEvent{Kind: "move"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := sink.Wheel(ctx, WheelEvent{DY: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
