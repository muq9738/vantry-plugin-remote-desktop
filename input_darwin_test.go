//go:build darwin

package remotedesktop

import (
	"context"
	"testing"
	"time"
)

func TestNativeMouseAddsClickStateForDoubleClick(t *testing.T) {
	var counts []int64
	api := &coreGraphics{mainDisplay: func() uint32 { return 1 }, displayBounds: func(uint32) cgRect { return cgRect{Width: 1000, Height: 1000} }, canPost: func() bool { return true }, mouseEvent: func(uintptr, uint32, cgPoint, uint32) uintptr { return 42 }, setInteger: func(_ uintptr, field uint32, value int64) {
		if field != 1 {
			t.Fatal("wrong click field")
		}
		counts = append(counts, value)
	}, postEvent: func(uint32, uintptr) {}, release: func(uintptr) {}}
	sink := &nativeInputSink{api: api, clickInterval: 500 * time.Millisecond}
	for _, event := range []MouseEvent{{Kind: "down", X: .5, Y: .5}, {Kind: "up", X: .5, Y: .5}, {Kind: "down", X: .5, Y: .5}, {Kind: "up", X: .5, Y: .5}, {Kind: "down", X: .7, Y: .5}, {Kind: "up", X: .7, Y: .5}} {
		if err := sink.Mouse(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	want := []int64{1, 1, 2, 2, 1, 1}
	if len(counts) != len(want) {
		t.Fatalf("clicks=%v", counts)
	}
	for i := range want {
		if counts[i] != want[i] {
			t.Fatalf("click states=%v want=%v", counts, want)
		}
	}
}
func TestInputSinkReadsSystemDoubleClickIntervalWithoutPosting(t *testing.T) {
	interval, err := systemDoubleClickInterval()
	if err != nil {
		t.Fatal(err)
	}
	if interval <= 0 || interval > 10*time.Second {
		t.Fatalf("unexpected system double click interval: %s", interval)
	}
}
