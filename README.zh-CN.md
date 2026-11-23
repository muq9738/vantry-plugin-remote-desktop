# Vantry 远程桌面插件

<p>
  <a href="README.md">English</a> · <strong>简体中文</strong> · <a href="README.zh-TW.md">繁體中文</a>
</p>

[Vantry](https://github.com/muq9738/Vantry) 的独立原生远程桌面引擎与插件，使用 **ScreenCaptureKit + VideoToolbox** 硬件 H.264（macOS）、**DXGI/D3D11 + Media Foundation**（Windows 预览）、**Pion WebRTC** 与原生键鼠注入。

Vantry 主应用负责页面、焦点与机器归属；本仓库负责屏幕采集、硬件编解码、WebRTC 媒体与输入协议，以及可选的自建 SSH/TURN 中继服务。

> **默认仅局域网**：不配置中继时仅在局域网内直连（`vantry-lan://`），不连接任何外部服务器；跨网络访问由用户显式配置自己的中继，不会自动回退到第三方基础设施。

## 连接与配置

- **局域网模式（默认）**：`defaultRelayHost` 留空。被控端自行启动临时 TLS 信令监听（`vantry-lan://…`），两端直接交换 WebRTC host candidates，不使用外部 STUN/TURN 或 SSH。关闭共享会立即关闭监听并撤销链接。
- **自建中继模式（可选）**：在自己的服务器上部署中继服务（见 [relayrtc/README.md](relayrtc/README.md)），在每台电脑将 `defaultRelayHost` 设为本机 OpenSSH 主机别名，并在界面中主动选择自建中继。
  - 信令经 SSH 连接中继主机的私有 Unix socket（`/run/vantry-rtc/signaling.sock`）。
  - 公网 TURN UDP/TCP 不可达时，支持经 `ssh -W 127.0.0.1:3479 <alias>` 隧道访问回环 TURN/TCP（统计标记 `transport: ssh-tcp`）。
  - 中继模式下媒体全程使用 DTLS-SRTP 与 relay-only ICE。

会话票据（`vantry-lan://…` / `vantry-rtc://…`）、角色 token、TURN 凭据和 SDP/ICE 密码仅限私有 UI 或 `0600` 文件保存。

## 按需授权

安装插件和启动应用不会申请屏幕录制或输入控制权限：

- `rtc.ProbePermissions(ctx, helperPath)` 执行无弹窗只读状态探测。
- `rtc.RequestPermission(ctx, helperPath, kind)`（`screen-capture` 或 `input-control`）仅在用户显式点击授权时调用。
- `rtc.OpenPermissionSettings(ctx, kind)` 在先前拒绝时跳转系统设置页面。

macOS 13+ 上，屏幕录制由随包原生 helper 申请，辅助功能由所属 Core 进程申请；共享开启状态不会跨重启持久化。

## Windows 实现预览

`native/windows` 提供 DXGI/D3D11 采集、Media Foundation 硬件 H.264 编解码及原生 `SendInput` 键鼠转换（兼容 macOS 键码/CGFlags、Unicode、DPI 与 `DISPLAYn` 坐标）。按真实硬件能力判断可用性，不静默回退软件编码。详见 [native/windows/README.md](native/windows/README.md)。

## Go 接入

```sh
go get github.com/muq9738/vantry-plugin-remote-desktop@latest
```

局域网主机调用 `rtc.StartHost(ctx, rtc.HostConfig{HelperPath: helperPath})`（不传 `SSHHost` 或 `Relay`），观看端将 `host.Ticket()` 传给 `rtc.Dial`。

显式使用自建 SSH 中继示例：

```go
import "github.com/muq9738/vantry-plugin-remote-desktop/rtc"

relay, err := rtc.NewSSHRelay(rtc.SSHConfig{
    Host:       "my-relay",     // 本机 OpenSSH 别名
    ConfigFile: localSSHConfig, // 可选，本机 SSH 配置路径
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

观看端示例：

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
// UI 渲染时调用 session.LatestFrame()。
```

### 画质档位

| 画质 | 最大分辨率 | 帧率上限 | 请求码率 |
| --- | --- | --- | --- |
| `ultra` | 2560×1440 | 60 fps | 8 Mbps |
| `balanced` | 1920×1080 | 30 fps | 4 Mbps |
| `low` | 1280×720 | 30 fps | 1.5 Mbps |

尺寸保持源显示器比例。编码强制硬件 H.264、无 B 帧，依据 RTCP 带宽反馈调整码率，参考链丢失时自动通过 PLI 请求 IDR。

## 输入与所有权

- 鼠标坐标映射为源显示器的 `[0, 1]` 归一化坐标（`0/1/2` 分别对应左/右/中键）。
- 按键与鼠标点击走可靠有序 DataChannel；鼠标位移与滚轮走无重传通道并携带最近可靠按钮序号，避免跨通道乱序。
- 会话关闭或窗口失焦时自动释放所有按住的按键与鼠标按钮。

## 构建与验证

```sh
go test -race ./... -timeout=120s
go vet ./...
(cd relayrtc && go test -race ./... -timeout=120s)
./native/build.sh
```

完整协议与原生 helper ABI 见 [SPEC-RTC.md](SPEC-RTC.md)。

## 开源许可

MIT License — 详见 [LICENSE](LICENSE)。
