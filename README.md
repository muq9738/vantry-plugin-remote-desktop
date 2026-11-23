# Vantry Remote Desktop

<p>
  <strong>English</strong> · <a href="README.zh-CN.md">简体中文</a> · <a href="README.zh-TW.md">繁體中文</a>
</p>

The standalone native remote-desktop engine and plugin for [Vantry](https://github.com/muq9738/Vantry), built with **ScreenCaptureKit + VideoToolbox** hardware H.264 (macOS), **DXGI/D3D11 + Media Foundation** (Windows preview), **Pion WebRTC**, and native keyboard/mouse injection.

Vantry owns the desktop UI, focus, and machine identity; this repository provides screen capture, hardware encoding/decoding, WebRTC transport, input injection, and an optional self-hosted SSH/TURN relay.

> **LAN-first by default:** When no relay is configured, sharing works strictly over LAN (`vantry-lan://`) with zero external servers. Cross-network access uses your own self-hosted relay and never falls back to third-party infrastructure.

## Connection & Configuration

- **LAN mode (default):** `defaultRelayHost` is empty. The host exposes an ephemeral TLS signaling listener (`vantry-lan://…`) and exchanges WebRTC host candidates directly over the local network without STUN, TURN, or SSH. Stopping sharing closes the listener and invalidates the link immediately.
- **Self-hosted relay (optional):** Deploy your own relay (see [relayrtc/README.md](relayrtc/README.md)), configure `defaultRelayHost` to your local OpenSSH host alias, and explicitly select the relay mode in the UI.
  - Signaling runs over SSH to the relay's Unix socket (`/run/vantry-rtc/signaling.sock`).
  - When public TURN UDP/TCP is unreachable, loopback TURN/TCP can be tunneled over `ssh -W 127.0.0.1:3479 <alias>` (`transport: ssh-tcp`).
  - Media always uses DTLS-SRTP and relay-only ICE in relay mode.

Session tickets (`vantry-lan://…` / `vantry-rtc://…`), role tokens, TURN credentials, and SDP/ICE secrets belong strictly in private UI or `0600` files.

## On-Demand Permissions

Installing the plugin or launching Vantry never requests screen capture or input permissions.

- `rtc.ProbePermissions(ctx, helperPath)` performs a read-only check without prompting.
- `rtc.RequestPermission(ctx, helperPath, kind)` (`screen-capture` or `input-control`) is only invoked on explicit user action.
- `rtc.OpenPermissionSettings(ctx, kind)` opens the OS settings pane when previously denied.

On macOS 13+, screen recording is requested by the bundled native helper and accessibility/input control by the owning Core process. Sharing state is never persisted across restarts.

## Windows Preview

`native/windows` provides DXGI/D3D11 capture, Media Foundation hardware H.264 encoding/decoding, and native `SendInput` translation (macOS keycodes/CGFlags, Unicode, DPI, and `DISPLAYn` coordinates). Hardware availability is probed honestly without silent software fallback. See [native/windows/README.md](native/windows/README.md).

## Go Usage

```sh
go get github.com/muq9738/vantry-plugin-remote-desktop@latest
```

For LAN-only hosting, call `rtc.StartHost(ctx, rtc.HostConfig{HelperPath: helperPath})` without `SSHHost` or `Relay`, and pass `host.Ticket()` to `rtc.Dial`.

For an explicit self-hosted SSH relay:

```go
import "github.com/muq9738/vantry-plugin-remote-desktop/rtc"

relay, err := rtc.NewSSHRelay(rtc.SSHConfig{
    Host:       "my-relay",     // Your local OpenSSH alias
    ConfigFile: localSSHConfig, // Optional local SSH config path
})
if err != nil {
    return err
}
defer relay.Close()

host, err := rtc.StartHost(ctx, rtc.HostConfig{
    Relay:      relay,
    SSHHost:    "my-relay",
    HelperPath: helperPath,
    Quality:    "balanced",
})
if err != nil {
    return err
}
defer host.Close()
```

Viewer side:

```go
session, err := rtc.Dial(ctx, rtc.ConnectConfig{
    Ticket:     privateTicket,
    HelperPath: helperPath,
    Relay:      relay,
})
if err != nil {
    return err
}
defer session.Close()
session.OnFrame(func() { invalidateOnUIThread() })
// Read frames on the UI thread via session.LatestFrame().
```

### Quality Presets

| Preset | Max Resolution | FPS Cap | Target Bitrate |
| --- | --- | --- | --- |
| `ultra` | 2560×1440 | 60 fps | 8 Mbps |
| `balanced` | 1920×1080 | 30 fps | 4 Mbps |
| `low` | 1280×720 | 30 fps | 1.5 Mbps |

Aspect ratio matches the source display. Hardware H.264 encoding runs without B-frames and adapts bitrate via RTCP feedback, with automatic PLI/IDR recovery on reference loss.

## Input & Ownership

- Pointer coordinates map to normalized `[0, 1]` coordinates on the source display (`0/1/2` for left/right/middle buttons).
- Button clicks and key events travel on an ordered reliable DataChannel; mouse motion and scroll travel on an unordered/unreliable DataChannel with latest-button sequence dependency so clicks are never reordered behind motion.
- Disconnecting or losing focus releases all held keys and mouse buttons.

## Build & Test

```sh
go test -race ./... -timeout=120s
go vet ./...
(cd relayrtc && go test -race ./... -timeout=120s)
./native/build.sh
```

See [SPEC-RTC.md](SPEC-RTC.md) for the full protocol and native helper ABI specification.

## License

MIT License — see [LICENSE](LICENSE).
