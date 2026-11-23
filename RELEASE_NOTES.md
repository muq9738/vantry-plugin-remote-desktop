## v0.2.3

- 默认采用局域网 WebRTC 直连，不预设任何外部中继；跨网访问需显式配置自建中继。
- 支持兼容 iOS WebKit 的 Constrained High 硬件 H.264 视频编码与协商。

## v0.2.2（Windows 实现预览）

配套Vantry0.1.13，增加DXGI/D3D11/Media Foundation硬件H.264 helper、SendInput与平台权限状态。媒体和输入共享DISPLAYn物理像素坐标；Windows普通共享不制造macOS式授权或自动提权。SDK原生exe检查、进程取消和后台窗口行为按平台实现。

Windows MSVC构建、portable协议及Python工具测试通过，Windows输入编码/取消/回调复用/混合Unicode释放回归通过。独立审查指出的watchdog stderr阻塞、两帧in-flight阻断、异步stream-change越过事件许可均已修正，跨构建与CI重验。源有界输入预算32帧，probe包含8帧连续合成编码。

托管Windows CI实际报告无可用GPU codec：HARDWARE_SKIPPED、未调用capture。尚未证明真实GPU/屏幕/混合DPI/键鼠/UIPI与性能；此版本标记prerelease，Mac稳定版本仍v0.2.1。未把交叉编译或没有硬件的CI当成Windows远控验收完成。

## v0.2.1

为Vantry0.1.12提供按需权限API：只读ProbePermissions、仅用户点按时RequestPermission、按平台打开系统设置；返回稳定camelCase的屏幕/输入状态和组件可用性。普通安装、启动和轮询不申请授权。macOS分别在真实helper/Core责任进程调用系统申请；拒绝保留为待授权。添加取消、未知kind、缺组件和平台状态回归，主module40项race/vet通过。Windows组件可用性和OS授权分别报告，不以权限文案声称Windows引擎已经可用。

## v0.2.0

通过原生 WebRTC 建立低延迟远程桌面：ScreenCaptureKit 采集、VideoToolbox 硬件 H.264 编解码、DTLS-SRTP 视频、可靠按键/按钮与无重传位移通道，支持通过自建中继的 SSH 私有信令和 TURN/TCP 跨网访问。

- Vantry 0.1.11 起使用 `rtc/`，原生窗口与目标 Core 分别持有观看器/被控会话；helper 随 App 打包。Go module 或 helper 升级需匹配宿主重建。
- 最新画面租赁、有界缓冲、参考帧丢失/静态尾包的 PLI 恢复、TURN 凭据续取与 ICE restart。修复 WebSocket 信令空闲及自动 Pong 的 SSH deadline 错误。
- 按钮与位移通过依赖序号保持状态，断开释放输入；原生 CG click-state 支持双击。键盘接口为 macOS 虚拟键码/CGFlags 和已提交 Unicode down/up。
- 新增本机 `sshConfigFile` 设置，复用已有认证，不修改全局 SSH/Git 配置。票据和媒体协商凭据仅进入私有 UI/文件。
- 主 Go module 与 `relayrtc` 竞态测试及 vet 通过。

版本需要 macOS 屏幕录制和辅助功能权限，拒绝时明确报错，不自动请求或绕过。完整测量边界见 README。

## v0.1.0

从 Vantry `0260956` 提取旧 JPEG/SSH/HTTP 远程桌面为独立插件及 Go module，保留既有公开输入类型、历史传输接口与测试。此版本未实现 WebRTC；0.2 宿主使用新的 `rtc/` 接线。
