package remotedesktop

import (
	"context"
	"image"
)

// DisplayInfo includes native pixel dimensions and global desktop coordinates
// in points (which can differ on Retina displays).
type DisplayInfo struct {
	ID     uint32  `json:"id"`
	Name   string  `json:"name"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Main   bool    `json:"main"`
	Scale  float64 `json:"scale"`
}

// Capturer transfers ownership of each captured image to the caller until
// Release, when implemented. Host serializes calls to both methods.
type Capturer interface {
	Displays(context.Context) ([]DisplayInfo, error)
	Capture(ctx context.Context, displayID uint32, scale float64) (*image.RGBA, error)
}

// FrameReleaser lets a capturer recycle frame buffers after encoding.
type FrameReleaser interface {
	Release(*image.RGBA)
}

// InputSink receives validated input. Host serializes calls across viewers.
type InputSink interface {
	Mouse(context.Context, MouseEvent) error
	Wheel(context.Context, WheelEvent) error
	Key(context.Context, KeyEvent) error
}
