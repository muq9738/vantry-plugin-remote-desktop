//go:build windows

package remotedesktop

// The legacy RGBA capturer is not the Windows media path. Windows RTC capture
// is provided by the native DXGI/Media Foundation helper.
func NewCapturer() (Capturer, error) { return nil, ErrPlatform }
