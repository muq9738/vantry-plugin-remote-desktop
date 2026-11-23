//go:build !darwin && !windows

package remotedesktop

import "context"

func ProbeInputAccess(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, ErrPlatform
}
func RequestInputAccess(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, ErrPlatform
}
