//go:build !darwin && !windows

package rtc

import (
	"context"
	"errors"
)

func probePlatformPermissions(ctx context.Context, helper string) (PermissionStatus, error) {
	if err := ctx.Err(); err != nil {
		return PermissionStatus{}, err
	}
	return unavailablePermissions("此平台尚未提供原生远程桌面组件"), nil
}
func requestPlatformPermission(context.Context, string, string) error {
	return errors.New("此平台没有该系统授权请求")
}
func openPlatformPermissionSettings(context.Context, string) error {
	return errors.New("此平台不使用 macOS 的屏幕或辅助功能授权设置")
}
