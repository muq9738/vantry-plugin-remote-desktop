package rtc

import (
	"context"
	"errors"
	"runtime"
)

type PermissionAccess struct {
	State           string `json:"state"`
	CanRequest      bool   `json:"canRequest"`
	CanOpenSettings bool   `json:"canOpenSettings"`
	Message         string `json:"message,omitempty"`
}
type PermissionStatus struct {
	Platform          string           `json:"platform"`
	ScreenCapture     PermissionAccess `json:"screenCapture"`
	InputControl      PermissionAccess `json:"inputControl"`
	CanHost           bool             `json:"canHost"`
	UnavailableReason string           `json:"unavailableReason,omitempty"`
}

const PermissionScreenCapture = "screen-capture"
const PermissionInputControl = "input-control"

func permissionKind(kind string) error {
	if kind != PermissionScreenCapture && kind != PermissionInputControl {
		return errors.New("请选择屏幕或键鼠控制权限")
	}
	return nil
}

// ProbePermissions is read-only: no OS dialog, settings launch, capture or input.
// Call on the installed plugin's host page, rather than application startup.
func ProbePermissions(ctx context.Context, helper string) (PermissionStatus, error) {
	if ctx == nil {
		return PermissionStatus{}, errors.New("permission context is required")
	}
	if err := ctx.Err(); err != nil {
		return PermissionStatus{}, err
	}
	return probePlatformPermissions(ctx, helper)
}

// RequestPermission is an explicit user action on the owning Core. A caller
// must not invoke it from probe, install, startup or an automatic polling loop.
func RequestPermission(ctx context.Context, helper, kind string) (PermissionStatus, error) {
	if err := permissionKind(kind); err != nil {
		return PermissionStatus{}, err
	}
	status, err := ProbePermissions(ctx, helper)
	if err != nil {
		return status, err
	}
	access := status.ScreenCapture
	if kind == PermissionInputControl {
		access = status.InputControl
	}
	if access.State == "granted" || access.State == "not-required" {
		return status, nil
	}
	if !access.CanRequest {
		return status, errors.New("当前组件不能请求该权限")
	}
	if err = requestPlatformPermission(ctx, helper, kind); err != nil {
		return status, err
	}
	return ProbePermissions(ctx, helper)
}

// OpenPermissionSettings is also user-triggered, never a probe side effect.
func OpenPermissionSettings(ctx context.Context, kind string) error {
	if ctx == nil {
		return errors.New("permission context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := permissionKind(kind); err != nil {
		return err
	}
	return openPlatformPermissionSettings(ctx, kind)
}
func unavailablePermissions(reason string) PermissionStatus {
	status := PermissionStatus{Platform: runtime.GOOS, UnavailableReason: reason}
	status.ScreenCapture = PermissionAccess{State: "unavailable", Message: reason}
	status.InputControl = PermissionAccess{State: "unavailable", Message: reason}
	return status
}
func completePermissions(status PermissionStatus) PermissionStatus {
	allowed := func(p PermissionAccess) bool { return p.State == "granted" || p.State == "not-required" }
	status.CanHost = status.UnavailableReason == "" && allowed(status.ScreenCapture) && allowed(status.InputControl)
	return status
}
