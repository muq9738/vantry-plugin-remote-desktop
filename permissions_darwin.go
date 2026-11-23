//go:build darwin

package remotedesktop

import (
	"context"
	"errors"
)

// ProbeInputAccess checks this process's ability to post native input. It never
// requests permission or opens System Settings.
func ProbeInputAccess(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	api, err := loadGraphics()
	if err != nil {
		return false, err
	}
	if api.canPost == nil {
		return false, errors.New("native input permission detection is unavailable")
	}
	return api.canPost(), nil
}

// RequestInputAccess must be called only for an explicit user action. The OS
// associates the request with this Core's responsible application.
func RequestInputAccess(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	api, err := loadGraphics()
	if err != nil {
		return false, err
	}
	if api.canPost != nil && api.canPost() {
		return true, nil
	}
	if api.requestPost == nil {
		return false, errors.New("native input permission request is unavailable")
	}
	return api.requestPost(), nil
}
