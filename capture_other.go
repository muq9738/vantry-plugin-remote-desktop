//go:build !darwin && !windows

package remotedesktop

// NewCapturer is unavailable on this platform. Supply a Capturer to host a
// synthetic or externally captured display; clients and relays are portable.
func NewCapturer() (Capturer, error) { return nil, ErrPlatform }

func NewInputSink() (InputSink, error) { return nil, ErrPlatform }
