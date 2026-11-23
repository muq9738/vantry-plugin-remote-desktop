//go:build windows

package rtc

import (
	"context"
	"errors"

	root "github.com/muq9738/vantry-plugin-remote-desktop"
)

func probePlatformPermissions(ctx context.Context, helper string) (PermissionStatus, error) {
	status := PermissionStatus{Platform: "windows", ScreenCapture: PermissionAccess{State: "not-required", Message: "开启共享时选择显示器，无需 macOS 式屏幕录制授权。"}, InputControl: PermissionAccess{State: "not-required", Message: "允许普通窗口操作；管理员窗口和安全桌面受 Windows 限制。"}}
	info, err := ProbeNative(ctx, helper)
	if ctx.Err() != nil {
		return status, ctx.Err()
	}
	if err != nil {
		status.UnavailableReason = "Windows 原生捕获与编解码组件不可用，请安装配套应用。"
		if info.Protocol == 1 && info.Codec == "h264" && !info.Hardware {
			status.UnavailableReason = "此设备尚无可用的硬件 H.264 编解码器，不能开启共享。"
		} else if _, pathErr := helperPath(helper); pathErr == nil {
			status.UnavailableReason = "Windows 视频组件未能启动，请重新检查或更新配套应用。"
		}
		return completePermissions(status), nil
	}
	if info.ScreenCaptureAccess != "granted" {
		status.ScreenCapture.State = "unavailable"
		status.ScreenCapture.Message = "当前 Windows 桌面无法共享，请解锁并切回交互会话。"
	}
	canInput, inputErr := root.ProbeInputAccess(ctx)
	if ctx.Err() != nil {
		return status, ctx.Err()
	}
	if inputErr != nil || !canInput {
		status.InputControl.State = "unavailable"
		status.InputControl.Message = "当前桌面无法接收操作，请解锁并切回交互会话。"
	}
	return completePermissions(status), nil
}
func requestPlatformPermission(context.Context, string, string) error {
	return errors.New("Windows 普通桌面共享与输入无需单独申请此权限")
}
func openPlatformPermissionSettings(context.Context, string) error {
	return errors.New("Windows 普通桌面共享没有 macOS 屏幕录制或辅助功能授权设置")
}
