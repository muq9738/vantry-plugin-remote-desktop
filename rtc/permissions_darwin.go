//go:build darwin

package rtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"time"

	root "github.com/muq9738/vantry-plugin-remote-desktop"
)

func probePlatformPermissions(ctx context.Context, helper string) (PermissionStatus, error) {
	info, err := ProbeNative(ctx, helper)
	if ctx.Err() != nil {
		return PermissionStatus{}, ctx.Err()
	}
	if err != nil {
		message := "屏幕捕获组件不可用，请安装配套的远程桌面插件和应用"
		if info.Protocol == 1 && info.Codec == "h264" && !info.Hardware {
			message = "此设备尚无可用的硬件 H.264 编解码器，不能开启共享。"
		} else if _, pathErr := helperPath(helper); pathErr == nil {
			message = "视频组件未能启动，请重新检查或更新配套应用。"
		}
		return unavailablePermissions(message), nil
	}
	status := PermissionStatus{Platform: "darwin"}
	status.ScreenCapture = PermissionAccess{State: info.ScreenCaptureAccess, CanRequest: true, CanOpenSettings: true}
	if status.ScreenCapture.State != "granted" {
		status.ScreenCapture.State = "denied"
		status.ScreenCapture.Message = "允许 Vantry 录制屏幕，用于向观看端显示此电脑。"
	}
	allowed, inputErr := root.ProbeInputAccess(ctx)
	if ctx.Err() != nil {
		return PermissionStatus{}, ctx.Err()
	}
	status.InputControl = PermissionAccess{State: "denied", CanRequest: inputErr == nil, CanOpenSettings: true, Message: "允许 Vantry 使用辅助功能，以接收键盘和鼠标操作。"}
	if inputErr != nil {
		status.InputControl.State = "unavailable"
		status.InputControl.Message = "无法检查此 Core 的辅助功能权限"
		status.UnavailableReason = status.InputControl.Message
	} else if allowed {
		status.InputControl.State = "granted"
		status.InputControl.Message = ""
	}
	return completePermissions(status), nil
}
func requestPlatformPermission(ctx context.Context, helper, kind string) error {
	if kind == PermissionInputControl {
		_, err := root.RequestInputAccess(ctx)
		return err
	}
	path, err := helperPath(helper)
	if err != nil {
		return err
	}
	request, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(request, path, "request-permission", "--kind", PermissionScreenCapture)
	cmd.Stderr = io.Discard
	output, err := cmd.Output()
	if request.Err() != nil {
		return request.Err()
	}
	if err != nil {
		return errors.New("系统授权请求未能完成，请打开系统设置")
	}
	record, data, err := readNative(bytes.NewReader(output))
	if err != nil {
		return err
	}
	if record == nativeError {
		return nativeFailure(data)
	}
	var result struct {
		Protocol int    `json:"protocol"`
		Mode     string `json:"mode"`
	}
	if record != nativeHello || json.Unmarshal(data, &result) != nil || result.Protocol != 1 || result.Mode != "request-permission" {
		return errors.New("授权组件版本不匹配，请更新配套应用")
	}
	return nil
}
func openPlatformPermissionSettings(ctx context.Context, kind string) error {
	page := "Privacy_ScreenCapture"
	if kind == PermissionInputControl {
		page = "Privacy_Accessibility"
	}
	return exec.CommandContext(ctx, "/usr/bin/open", "x-apple.systempreferences:com.apple.preference.security?"+page).Run()
}
