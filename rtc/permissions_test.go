package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPermissionReadinessRequiresComponentAndEveryNeededGrant(t *testing.T) {
	base := PermissionStatus{ScreenCapture: PermissionAccess{State: "granted"}, InputControl: PermissionAccess{State: "granted"}}
	if !completePermissions(base).CanHost {
		t.Fatal("ready host blocked")
	}
	base.InputControl.State = "denied"
	if completePermissions(base).CanHost {
		t.Fatal("missing input permission enabled host")
	}
	base.InputControl.State = "not-required"
	base.UnavailableReason = "missing native media"
	if completePermissions(base).CanHost {
		t.Fatal("permission status hid missing media component")
	}
}
func TestPermissionCallsRejectCancelledContextBeforeOSActions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProbePermissions(ctx, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := RequestPermission(ctx, "unused", PermissionScreenCapture); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := OpenPermissionSettings(ctx, PermissionInputControl); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := RequestPermission(context.Background(), "unused", "invalid"); err == nil {
		t.Fatal("invalid request accepted")
	}
	if err := OpenPermissionSettings(context.Background(), "invalid"); err == nil {
		t.Fatal("invalid settings accepted")
	}
}
func TestPermissionStatusSerializesStableCamelCase(t *testing.T) {
	raw, err := json.Marshal(PermissionStatus{Platform: "darwin", ScreenCapture: PermissionAccess{State: "denied", CanRequest: true, CanOpenSettings: true}, InputControl: PermissionAccess{State: "granted"}})
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	for _, key := range []string{"platform", "screenCapture", "inputControl", "canHost"} {
		if _, ok := value[key]; !ok {
			t.Fatalf("missing permission contract field %s", key)
		}
	}
	item := value["screenCapture"].(map[string]any)
	if item["state"] != "denied" || item["canRequest"] != true || item["canOpenSettings"] != true {
		t.Fatal(string(raw))
	}
}
func TestPermissionProbeMissingHelperIsStructuredAndNeverPrompts(t *testing.T) {
	status, err := ProbePermissions(context.Background(), filepath.Join(t.TempDir(), "missing-native-helper"))
	if err != nil {
		t.Fatal(err)
	}
	if status.CanHost || status.UnavailableReason == "" {
		t.Fatalf("missing component looked ready: %+v", status)
	}
	if status.Platform != runtime.GOOS {
		t.Fatal(status.Platform)
	}
	if runtime.GOOS != "darwin" && (status.ScreenCapture.CanRequest || status.InputControl.CanRequest || status.ScreenCapture.CanOpenSettings) {
		t.Fatal("non-Mac status offered macOS authorization")
	}
}
func TestNativePermissionsProbeIsReadOnly(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS native capability probe")
	}
	helper, _ := filepath.Abs(filepath.Join("..", "native", "bin", "vantry-remote-helper"))
	if _, err := os.Stat(helper); err != nil {
		t.Skip("build native helper")
	}
	status, err := ProbePermissions(context.Background(), helper)
	if err != nil {
		t.Fatal(err)
	}
	if status.ScreenCapture.State != "granted" && status.ScreenCapture.State != "denied" {
		t.Fatalf("bad native screen state %+v", status)
	}
	if status.InputControl.State != "granted" && status.InputControl.State != "denied" {
		t.Fatalf("bad Core input state %+v", status)
	}
}
