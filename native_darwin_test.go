//go:build darwin

package remotedesktop

import (
	"context"
	"errors"
	"image"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/ebitengine/purego"
)

func TestNativePixelConversionScalingAndRelease(t *testing.T) {
	for _, alpha := range []uint32{2, 6} {
		t.Run(map[uint32]string{2: "BGRA", 6: "BGRX"}[alpha], func(t *testing.T) {
			// Four native pixels and row padding, with intentionally unused alpha.
			pixels := []byte{30, 20, 10, 0, 60, 50, 40, 0, 0, 0, 0, 0,
				90, 80, 70, 0, 120, 110, 100, 0, 0, 0, 0, 0}
			var released []uintptr
			api := &coreGraphics{
				mainDisplay: func() uint32 { return 9 },
				createImage: func(id uint32) uintptr {
					if id != 9 {
						t.Fatalf("display ID = %d", id)
					}
					return 1
				},
				imageWidth: func(uintptr) uintptr { return 2 }, imageHeight: func(uintptr) uintptr { return 2 },
				bytesPerRow: func(uintptr) uintptr { return 12 }, bitsPerPixel: func(uintptr) uintptr { return 32 }, bitsPerComp: func(uintptr) uintptr { return 8 },
				bitmapInfo: func(uintptr) uint32 { return 0x2000 | alpha }, dataProvider: func(uintptr) uintptr { return 2 }, copyData: func(uintptr) uintptr { return 3 },
				dataLen: func(uintptr) int64 { return int64(len(pixels)) }, dataPtr: func(uintptr) *byte { return &pixels[0] },
				release: func(ref uintptr) { released = append(released, ref) },
			}
			capture := &nativeCapturer{api: api}
			frame, err := capture.Capture(context.Background(), 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := []byte{10, 20, 30, 255, 40, 50, 60, 255, 70, 80, 90, 255, 100, 110, 120, 255}
			if !reflect.DeepEqual(frame.Pix, want) || !reflect.DeepEqual(released, []uintptr{3, 1}) {
				t.Fatalf("pixels=%v released=%v", frame.Pix, released)
			}
			capture.Release(frame)
			for _, scale := range []float64{.75, .5} {
				frame, err := capture.Capture(context.Background(), 9, scale)
				if err != nil || frame.Rect.Size() != image.Pt(1, 1) {
					t.Fatalf("scaled frame: %v %v", frame, err)
				}
				capture.Release(frame)
			}
			api.dataLen = func(uintptr) int64 { return 2 }
			if _, err := capture.Capture(context.Background(), 9, 1); err == nil {
				t.Fatal("incomplete data accepted")
			}
			if !reflect.DeepEqual(released[len(released)-2:], []uintptr{3, 1}) {
				t.Fatal("error path leaked native data")
			}
			api.createImage = func(uint32) uintptr { return 0 }
			if _, err := capture.Capture(context.Background(), 9, 1); err == nil || err.Error() != screenPermissionMessage {
				t.Fatalf("permission error: %v", err)
			}
		})
	}
}

func TestNativeInputCoordinatesAndUnicode(t *testing.T) {
	var kinds []uint32
	var points []cgPoint
	var buttons []uint32
	var posted, released int
	var vertical, horizontal int32
	var text []uint16
	var keyCode uint16
	var keyDown bool
	var flags uint64
	api := &coreGraphics{
		mainDisplay: func() uint32 { return 7 },
		displayBounds: func(id uint32) cgRect {
			if id != 7 {
				t.Fatalf("display = %d", id)
			}
			return cgRect{-1920, -100, 1920, 1080}
		},
		mouseEvent: func(_ uintptr, kind uint32, point cgPoint, button uint32) uintptr {
			kinds, points, buttons = append(kinds, kind), append(points, point), append(buttons, button)
			return 20
		},
		scrollEvent: func(_ uintptr, units, wheels uint32, dy, dx int32) uintptr {
			if units != 0 || wheels != 2 {
				t.Fatal("wrong scroll units or wheel count")
			}
			vertical, horizontal = dy, dx
			return 21
		},
		keyEvent: func(_ uintptr, code uint16, down bool) uintptr { keyCode, keyDown = code, down; return 22 },
		keyUnicode: func(_ uintptr, count uintptr, ptr *uint16) {
			text = append(text, *ptr)
			if count != 4 {
				t.Fatalf("UTF-16 length=%d", count)
			}
		},
		setFlags: func(_ uintptr, value uint64) { flags = value },
		setInteger: func(_ uintptr, field uint32, value int64) {
			if field != 1 || value < 1 {
				t.Fatal("invalid click state")
			}
		},
		postEvent: func(uint32, uintptr) { posted++ }, release: func(uintptr) { released++ }, canPost: func() bool { return true },
	}
	sink := &nativeInputSink{api: api}
	for _, e := range []MouseEvent{{Kind: "move"}, {Kind: "down", X: 1, Y: 1, Button: 1}, {Kind: "up", Button: 2}, {Kind: "drag", Button: 2}, {Kind: "drag", Button: 0}, {Kind: "drag", Button: 1}} {
		if err := sink.Mouse(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(kinds, []uint32{5, 3, 26, 27, 6, 7}) || points[0] != (cgPoint{-1920, -100}) || points[1] != (cgPoint{-1, 979}) || buttons[1] != 1 {
		t.Fatalf("kinds=%v points=%v buttons=%v", kinds, points, buttons)
	}
	if err := sink.Wheel(context.Background(), WheelEvent{DX: 3.4, DY: -5.6}); err != nil {
		t.Fatal(err)
	}
	if vertical != 6 || horizontal != -3 {
		t.Fatalf("wheel = %d %d", vertical, horizontal)
	}
	if err := sink.Key(context.Background(), KeyEvent{Kind: "down", KeyCode: 12, Text: "你好😀", Modifiers: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if keyCode != 12 || !keyDown || flags != 1<<20 || text[0] != utf16.Encode([]rune("你好😀"))[0] {
		t.Fatal("keyboard event lost data")
	}
	if posted != 8 || released != posted {
		t.Fatalf("posted=%d released=%d", posted, released)
	}
	api.canPost = func() bool { return false }
	if err := sink.Key(context.Background(), KeyEvent{Kind: "up"}); err == nil || !strings.Contains(err.Error(), "辅助功能") {
		t.Fatalf("permission error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Mouse(ctx, MouseEvent{Kind: "move"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled input: %v", err)
	}
}

func TestNativeScrollBindingWithoutPosting(t *testing.T) {
	api, err := loadGraphics()
	if err != nil {
		t.Fatal(err)
	}
	event := api.scrollEvent(0, 0, 2, 12, -3)
	if event == 0 {
		t.Fatal("CoreGraphics could not construct a scroll event")
	}
	defer api.release(event)
	cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_LAZY|purego.RTLD_LOCAL)
	if err != nil {
		t.Fatal(err)
	}
	defer purego.Dlclose(cg)
	var getField func(uintptr, uint32) int64
	purego.RegisterLibFunc(&getField, cg, "CGEventGetIntegerValueField")
	if dy, dx := getField(event, 96), getField(event, 97); dy != 12 || dx != -3 {
		t.Fatalf("native scroll arguments: dy=%d dx=%d", dy, dx)
	}
	// CGRect is returned as four floating-point values on Apple arm64.
	id := api.mainDisplay()
	if id != 0 {
		bounds := api.displayBounds(id)
		if bounds.Width <= 0 || bounds.Height <= 0 {
			t.Fatalf("invalid native display bounds: %+v", bounds)
		}
	}
}
