//go:build darwin

package remotedesktop

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	xdraw "golang.org/x/image/draw"
)

const screenPermissionMessage = "需要开启 macOS「屏幕录制」权限（系统设置 → 隐私与安全性 → 屏幕录制）"

type cgPoint struct{ X, Y float64 }
type cgRect struct{ X, Y, Width, Height float64 }

type coreGraphics struct {
	activeDisplays func(uint32, *uint32, *uint32) int32
	mainDisplay    func() uint32
	displayBounds  func(uint32) cgRect
	pixelsWide     func(uint32) uintptr
	pixelsHigh     func(uint32) uintptr
	createImage    func(uint32) uintptr
	imageWidth     func(uintptr) uintptr
	imageHeight    func(uintptr) uintptr
	bytesPerRow    func(uintptr) uintptr
	bitsPerPixel   func(uintptr) uintptr
	bitsPerComp    func(uintptr) uintptr
	bitmapInfo     func(uintptr) uint32
	dataProvider   func(uintptr) uintptr
	copyData       func(uintptr) uintptr
	dataPtr        func(uintptr) *byte
	dataLen        func(uintptr) int64
	release        func(uintptr)
	mouseEvent     func(uintptr, uint32, cgPoint, uint32) uintptr
	scrollEvent    func(uintptr, uint32, uint32, int32, int32) uintptr
	keyEvent       func(uintptr, uint16, bool) uintptr
	keyUnicode     func(uintptr, uintptr, *uint16)
	setFlags       func(uintptr, uint64)
	setInteger     func(uintptr, uint32, int64)
	postEvent      func(uint32, uintptr)
	canPost        func() bool
	requestPost    func() bool
}

var graphics struct {
	once sync.Once
	api  *coreGraphics
	err  error
}

func loadGraphics() (*coreGraphics, error) {
	graphics.once.Do(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				graphics.err = fmt.Errorf("remote desktop: bind CoreGraphics: %v", recovered)
			}
		}()
		cg, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_LAZY|purego.RTLD_LOCAL)
		if err != nil {
			graphics.err = err
			return
		}
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_LOCAL)
		if err != nil {
			graphics.err = err
			return
		}
		a := &coreGraphics{}
		purego.RegisterLibFunc(&a.activeDisplays, cg, "CGGetActiveDisplayList")
		purego.RegisterLibFunc(&a.mainDisplay, cg, "CGMainDisplayID")
		purego.RegisterLibFunc(&a.displayBounds, cg, "CGDisplayBounds")
		purego.RegisterLibFunc(&a.pixelsWide, cg, "CGDisplayPixelsWide")
		purego.RegisterLibFunc(&a.pixelsHigh, cg, "CGDisplayPixelsHigh")
		purego.RegisterLibFunc(&a.createImage, cg, "CGDisplayCreateImage")
		purego.RegisterLibFunc(&a.imageWidth, cg, "CGImageGetWidth")
		purego.RegisterLibFunc(&a.imageHeight, cg, "CGImageGetHeight")
		purego.RegisterLibFunc(&a.bytesPerRow, cg, "CGImageGetBytesPerRow")
		purego.RegisterLibFunc(&a.bitsPerPixel, cg, "CGImageGetBitsPerPixel")
		purego.RegisterLibFunc(&a.bitsPerComp, cg, "CGImageGetBitsPerComponent")
		purego.RegisterLibFunc(&a.bitmapInfo, cg, "CGImageGetBitmapInfo")
		purego.RegisterLibFunc(&a.dataProvider, cg, "CGImageGetDataProvider")
		purego.RegisterLibFunc(&a.copyData, cg, "CGDataProviderCopyData")
		purego.RegisterLibFunc(&a.dataPtr, cf, "CFDataGetBytePtr")
		purego.RegisterLibFunc(&a.dataLen, cf, "CFDataGetLength")
		purego.RegisterLibFunc(&a.release, cf, "CFRelease")
		purego.RegisterLibFunc(&a.mouseEvent, cg, "CGEventCreateMouseEvent")
		purego.RegisterLibFunc(&a.keyEvent, cg, "CGEventCreateKeyboardEvent")
		purego.RegisterLibFunc(&a.keyUnicode, cg, "CGEventKeyboardSetUnicodeString")
		purego.RegisterLibFunc(&a.setFlags, cg, "CGEventSetFlags")
		purego.RegisterLibFunc(&a.setInteger, cg, "CGEventSetIntegerValueField")
		purego.RegisterLibFunc(&a.postEvent, cg, "CGEventPost")
		if symbol, err := purego.Dlsym(cg, "CGPreflightPostEventAccess"); err == nil {
			purego.RegisterFunc(&a.canPost, symbol)
		}
		if symbol, err := purego.Dlsym(cg, "CGRequestPostEventAccess"); err == nil {
			purego.RegisterFunc(&a.requestPost, symbol)
		}
		if runtime.GOARCH == "arm64" {
			// wheel1 is fixed; wheel2 is variadic. Apple arm64 puts the latter
			// in an eight-byte stack slot after the four unused integer registers.
			var scroll func(uintptr, uint32, uint32, int32, uintptr, uintptr, uintptr, uintptr, int64) uintptr
			purego.RegisterLibFunc(&scroll, cg, "CGEventCreateScrollWheelEvent")
			a.scrollEvent = func(source uintptr, unit, count uint32, dy, dx int32) uintptr {
				return scroll(source, unit, count, dy, 0, 0, 0, 0, int64(dx))
			}
		} else {
			purego.RegisterLibFunc(&a.scrollEvent, cg, "CGEventCreateScrollWheelEvent")
		}
		graphics.api = a
	})
	return graphics.api, graphics.err
}

type nativeCapturer struct {
	api  *coreGraphics
	pool sync.Pool
}

// NewCapturer loads macOS capture APIs without requesting permission or using cgo.
func NewCapturer() (Capturer, error) {
	api, err := loadGraphics()
	if err != nil {
		return nil, err
	}
	return &nativeCapturer{api: api}, nil
}

func (c *nativeCapturer) Displays(ctx context.Context) ([]DisplayInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var count uint32
	if status := c.api.activeDisplays(0, nil, &count); status != 0 {
		return nil, fmt.Errorf("remote desktop: enumerate displays: CoreGraphics %d", status)
	}
	if count == 0 {
		return nil, nil
	}
	if count > 256 {
		return nil, errors.New("remote desktop: too many displays")
	}
	ids := make([]uint32, count)
	if status := c.api.activeDisplays(count, &ids[0], &count); status != 0 || int(count) > len(ids) {
		return nil, fmt.Errorf("remote desktop: enumerate displays: CoreGraphics %d", status)
	}
	main := c.api.mainDisplay()
	displays := make([]DisplayInfo, 0, count)
	for _, id := range ids[:count] {
		bounds := c.api.displayBounds(id)
		scale := 1.0
		width, height := int(c.api.pixelsWide(id)), int(c.api.pixelsHigh(id))
		if bounds.Width > 0 {
			scale = float64(width) / bounds.Width
		}
		displays = append(displays, DisplayInfo{ID: id, Name: fmt.Sprintf("Display %d", id), Width: width, Height: height,
			X: bounds.X, Y: bounds.Y, Main: id == main, Scale: scale})
	}
	return displays, nil
}

func (c *nativeCapturer) getFrame(width, height int) *image.RGBA {
	if value := c.pool.Get(); value != nil {
		frame := value.(*image.RGBA)
		if cap(frame.Pix) >= width*height*4 {
			frame.Pix = frame.Pix[:width*height*4]
			frame.Stride = width * 4
			frame.Rect = image.Rect(0, 0, width, height)
			return frame
		}
	}
	return image.NewRGBA(image.Rect(0, 0, width, height))
}

func (c *nativeCapturer) Release(frame *image.RGBA) {
	if frame != nil {
		c.pool.Put(frame)
	}
}

func (c *nativeCapturer) Capture(ctx context.Context, displayID uint32, scale float64) (*image.RGBA, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scale != 1 && scale != .75 && scale != .5 {
		return nil, errors.New("remote desktop: scale must be 1, 0.75, or 0.5")
	}
	if displayID == 0 {
		displayID = c.api.mainDisplay()
	}
	ref := c.api.createImage(displayID)
	if ref == 0 {
		return nil, errors.New(screenPermissionMessage)
	}
	defer c.api.release(ref)
	width, height := int(c.api.imageWidth(ref)), int(c.api.imageHeight(ref))
	stride := int(c.api.bytesPerRow(ref))
	if width < 1 || height < 1 || width > 16384 || height > 16384 || int64(width)*int64(height) > 32<<20 ||
		stride < width*4 || int64(stride) > math.MaxInt64/int64(height) || c.api.bitsPerPixel(ref) != 32 || c.api.bitsPerComp(ref) != 8 {
		return nil, errors.New("remote desktop: unsupported CoreGraphics pixel layout")
	}
	provider := c.api.dataProvider(ref)
	if provider == 0 {
		return nil, errors.New("remote desktop: image has no data provider")
	}
	data := c.api.copyData(provider)
	if data == 0 {
		return nil, errors.New("remote desktop: cannot copy display pixels")
	}
	defer c.api.release(data)
	length, ptr := c.api.dataLen(data), c.api.dataPtr(data)
	if length < int64(stride)*int64(height) || ptr == nil {
		return nil, errors.New("remote desktop: incomplete display pixels")
	}
	pixels := unsafe.Slice(ptr, stride*height)
	frame := c.getFrame(width, height)
	if err := convertDisplayPixels(frame, pixels, stride, c.api.bitmapInfo(ref)); err != nil {
		c.Release(frame)
		return nil, err
	}
	if scale != 1 {
		scaled := c.getFrame(max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale)))
		xdraw.ApproxBiLinear.Scale(scaled, scaled.Rect, frame, frame.Rect, xdraw.Src, nil)
		c.Release(frame)
		frame = scaled
	}
	return frame, nil
}

func convertDisplayPixels(dst *image.RGBA, src []byte, stride int, bitmap uint32) error {
	alpha, order := bitmap&31, bitmap&0x7000
	first := alpha == 2 || alpha == 4 || alpha == 6
	last := alpha == 1 || alpha == 3 || alpha == 5
	if (!first && !last) || (order != 0 && order != 0x2000 && order != 0x4000) {
		return errors.New("remote desktop: unsupported display channel order")
	}
	red, green, blue := 0, 1, 2
	if first {
		red, green, blue = 1, 2, 3
	}
	if order == 0x2000 {
		red, green, blue = 3-red, 3-green, 3-blue
	}
	for y := 0; y < dst.Rect.Dy(); y++ {
		for x := 0; x < dst.Rect.Dx(); x++ {
			i, j := y*stride+x*4, y*dst.Stride+x*4
			dst.Pix[j], dst.Pix[j+1], dst.Pix[j+2], dst.Pix[j+3] = src[i+red], src[i+green], src[i+blue], 255
		}
	}
	return nil
}
