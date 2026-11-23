# Vantry Remote Desktop

当前修正默认连接方式：不配置中继时只使用局域网；跨网络由用户提供自己的服务器与 SSH 配置，不默认或回退到私人服务器。

Vantry 的独立远程桌面插件，使用 ScreenCaptureKit、VideoToolbox 硬件 H.264、WebRTC 和原生键鼠。Vantry 负责页面、焦点和机器归属，本仓库负责采集、编解码、媒体与输入协议。Windows 仍为实现预览，尚无真实 GPU、真屏与输入性能通过结论。

在插件中心安装或更新 `muq9738/vantry-plugin-remote-desktop`。原生引擎与 helper 随 Vantry 构建；更新脚本插件后，需要使用匹配的 Vantry 版本。macOS 13 或更新版本，主机需要屏幕录制及辅助功能权限；探测不会自动请求或绕过系统授权。

## 连接与配置

默认“仅局域网”，`defaultRelayHost` 留空。被控端自行提供 TLS 信令监听，连接地址为 `vantry-lan://…`。两端直接交换 WebRTC host candidates，不使用外部 STUN/TURN，也不启动 SSH。关闭共享会关闭监听和会话。连接地址内的公开 TLS 证书只用于本次共享，与 App 的代码签名证书无关。

跨网络时，使用者可部署自己的中继服务，见 [自建服务](relayrtc/README.md)。在每台电脑设置 `defaultRelayHost` 为本机 SSH 别名，然后在 UI 主动选择“自建中继”。观看端与被控端应访问同一服务器，本地别名可不同。可选 `sshConfigFile` 使用该电脑自己的配置文件。空配置、错误配置或局域网失败都不会自动选择外部服务器。

- 信令通过 SSH 连接自建中继的私有 Unix socket `/run/vantry-rtc/signaling.sock`。
- 若公网 TURN UDP/TCP 不可达，也可启用回环 TURN/TCP，经插件托管的 `ssh -W 127.0.0.1:3479 <alias>` 隧道转发，统计标记 `transport: ssh-tcp`。
- 媒体全程使用 DTLS-SRTP 与 relay-only ICE。部署与认证合同见 [relayrtc/README.md](relayrtc/README.md)。

目标机器主动开启被控后，分享私有 `vantry-rtc://…` 票据给观看端。票据、角色 token、TURN 凭据和 SDP/ICE 密码只能进入私有 UI 或 0600 文件。关闭目标 Core 的被控会话会撤销会话；关闭观看页会关闭该页自己的观看器及传输。

## 按需授权

安装插件和启动应用不会申请屏幕或输入权限。主机页可调用 `rtc.ProbePermissions(ctx, helperPath)` 做无弹窗只读检查；用户选择开启被控后，由宿主显示各权限状态。仅显式点按允许时调用 `rtc.RequestPermission(ctx, helperPath, kind)`；`kind` 为 `screen-capture` 或 `input-control`。已允许不会重复申请，拒绝后提供 `rtc.OpenPermissionSettings(ctx, kind)`，并在返回时重新检查。

macOS 的屏幕录制请求由包内helper发起，辅助功能请求由目标Core发起，均使用真实责任进程。开关只有在权限及组件均可用、HostStart成功后才显示开启；意图不跨视图或重启持久化。Windows的系统授权语义独立于macOS，未有可用原生组件时返回unavailable原因，不伪造已授权或自动提权。

## Windows 实现预览

`native/windows` 提供 DXGI/D3D11 捕获、Media Foundation 硬件 H.264 编解码及严格合成验收；Go 使用原生 SendInput并处理Mac键码/CGFlags转换、Unicode、DPI与DISPLAYn坐标。helper和SSH在后台运行，普通探测不弹控制台、系统授权或UAC。Windows按当前交互桌面及实际硬件能力判断CanHost；缺硬件不伪装授权成功，不回退软件编码。详见[native/windows/README.md](native/windows/README.md)。

Windows预览宿主使用 `go get github.com/muq9738/vantry-plugin-remote-desktop@v0.2.2` 并打包对应exe；仅发布为prerelease，不替换稳定Mac版本。真实GPU/屏幕/输入仍需Windows机器，现有两台Mac的验收不能替代。

## Go 接入

```sh
go get github.com/muq9738/vantry-plugin-remote-desktop@latest
```

局域网主机调用 `rtc.StartHost(ctx, rtc.HostConfig{HelperPath: helperPath})`，不传 `SSHHost` 或 `Relay`；观看端把 `host.Ticket()` 传给 `rtc.Dial`。原生 helper 和编解码链路不变。

下面是用户明确选择自建中继时的接入示例：

```go
import "github.com/muq9738/vantry-plugin-remote-desktop/rtc"

relay, err := rtc.NewSSHRelay(rtc.SSHConfig{
    Host: "my-relay", // 用户自己配置的 SSH 别名
    ConfigFile: localSSHConfig, // 可选，本机配置路径
})
if err != nil { return err }
defer relay.Close()

host, err := rtc.StartHost(ctx, rtc.HostConfig{
    Relay: relay,
    SSHHost: "my-relay",
    HelperPath: helperPath,
    Quality: "balanced",
})
if err != nil { return err }
defer host.Close()
// 在私有 UI 分享 host.Ticket()。
```

原生 helper 默认打包于 `Vantry.app/Contents/Resources/remote-desktop/vantry-remote-helper`。开发构建可显式指定路径；构建方式见 [native/build.sh](native/build.sh)。

```go
session, err := rtc.Dial(ctx, rtc.ConnectConfig{
    Ticket: privateTicket,
    HelperPath: helperPath,
    Relay: relay,
})
if err != nil { return err }
defer session.Close()
session.OnFrame(func() { invalidateOnUIThread() })
// UI 渲染时调用 session.LatestFrame()。
```

调用方提供的 relay 由调用方关闭；内部创建的 relay 随会话关闭。`ctx` 控制完整会话。`Dial` 等待媒体连接及两个输入 DataChannel 就绪。`OnFrame` 是后台合并通知，UI 应在自己的线程读取帧。

`LatestFrame()` 返回 `(*image.RGBA, seq, Stats)`。图像保持只读，租赁有效至下一次 `LatestFrame()` 或关闭会话；序号不变表示没有新帧，不复制整幅图像。内部有界缓冲保留最新已解码画面，编码参考链丢失时请求 IDR。`Done`、`Err`、`Close` 提供终止原因和回收；断线后由调用方重新 `Dial`。

| 画质 | 最大分辨率 | 帧率上限 | 请求码率 |
| --- | --- | --- | --- |
| ultra | 2560×1440 | 60 fps | 8 Mbps |
| balanced | 1920×1080 | 30 fps | 4 Mbps |
| low | 1280×720 | 30 fps | 1.5 Mbps |

尺寸保持源显示器比例。编码强制硬件、无 B 帧，使用 RTCP 带宽反馈调整码率；不静默改用软件或 JPEG。根目录历史传输实现的原版本说明保留在 `v0.1.0` 标签，0.2 宿主调用 `rtc/`。

## 输入与所有权

鼠标坐标为源显示器的 `[0,1]` 点坐标，按钮 `0/1/2` 表示左/右/中键；事件为 `move/down/up/drag`。可靠有序通道负责按钮及按键，无重传通道负责位移和滚轮。位移携带它依赖的最近按钮序号，只保留一条等待位移，防止跨通道乱序丢失 mouse-up 或提前拖动。原生注入按目标机器系统双击间隔生成 CGClickState。

键盘只接收 `down/up`，`KeyCode` 为 macOS 虚拟键码，`Modifiers` 为 CoreGraphics flags。Unicode 传已提交文本；IME 组合态留在本机。宿主负责 HID/逻辑键转换、IME caret、远程焦点、全局快捷键以及失焦时释放持有输入。会话关闭也释放本会话的按键和按钮。可靠输入发送失败会终止观看会话，使目标释放输入状态。

观看器属于本机窗口；HostStart/Stop/Status 必须发给目标环境所属 Core。远程 IDE 页面不能继承本机 hoster，否则会误开启本机屏幕。

## 验证与测量

```sh
go test -race ./... -timeout=120s
go vet ./...
(cd relayrtc && go test -race ./... -timeout=120s)
./native/build.sh
```

本地已通过主 Go module 35 项 race 测试、relayrtc 6 项 race 测试及 vet。回归覆盖信令空闲与自动 Pong deadline、丢包/静态 marker 的 IDR 恢复、续期失败重试、帧租赁、输入乱序、断开释放、坐标/Unicode/双击和实际硬件编解码桥。

`cmd/rtc-check` 提供显式 synthetic、真屏、host/viewer 与私有指标文件。默认输入使用记录器；viewer 默认不发送输入。真实注入要求 host `--allow-input`，受控测试发送还必须显式 `--test-input`。原生临时测试窗口见 [native/tests/InputWindow.swift](native/tests/InputWindow.swift)，会限时退出，报告点击/文本及只读权限状态。

RTT、首帧耗时、时钟补偿后的估计帧龄分别报告；FPS 为会话平均值，`framesPolled` 与解码 `frames` 分开。协议及边界说明见 [SPEC-RTC.md](SPEC-RTC.md)。

## License

MIT License — see [LICENSE](LICENSE).
