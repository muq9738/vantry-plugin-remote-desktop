# 原生远程桌面连接合同

当前产品目标：默认局域网直连；使用者可自行部署中继用于跨网络访问。未配置中继时只允许局域网，不推断任何外部服务器。插件拥有视频、输入和连接协议；Vantry 保留原生页面及生命周期适配。

## 传输架构与范围

本实现使用 ScreenCaptureKit + VideoToolbox（macOS）或 DXGI/Media Foundation（Windows 预览）硬件 H.264、Pion WebRTC/DTLS-SRTP 和 DataChannel。局域网模式由被控端提供 TLS 信令，访问凭据仅用于本次共享；host candidates 仅来自本地私有接口，不使用外部 STUN/TURN。自建中继模式由用户明确配置 SSH 主机，其信令入口限服务端 Unix socket，经使用者自己的 SSH 认证访问。两个模式都不回退到裸 TCP/JPEG。

## 组件与所有权

| 组件 | 维护位置 | 职责 |
| --- | --- | --- |
| Native helper | 本插件 `native/` | 平台采集、硬编/硬解、权限探测与受控退出 |
| RTC host/viewer | 本插件 `rtc/` | WebRTC/TURN 传输、视频与键鼠、帧龄/RTT、资源清理 |
| Self-hosted relay | 本插件 `relayrtc/` 子模块 | SSH 私有信令与短期认证 TURN 凭据签发 |
| Vantry 宿主集成 | Vantry 主仓库 | 原生页面、权限引导、helper 打包与会话生命周期管理 |

插件主 `go.mod`/`go.sum` 与 `relayrtc/` 子模块分别独立管理依赖。

## 自建中继信令与认证

服务专用 Unix socket 为 `/run/vantry-rtc/signaling.sock`，0700 目录、0600 socket。客户端通过已有 SSH 调用中继主机上的 `/opt/vantry-rtc/bin/relayrtc proxy --socket /run/vantry-rtc/signaling.sock`，以标准输入输出连接此 socket；不复制 SSH 私钥，不关闭主机认证。

- `POST /v1/sessions`：仅通过 SSH 私有 socket 创建。响应 `sessionId, hostToken, viewerToken, iceServers, iceTransportPolicy:"relay", expiresAt, video`。
- `GET /v1/sessions/{id}`：Bearer hostToken 或 viewerToken，返回 sessionId、iceServers、expiresAt 和 video，不返回对方 token。
- `GET /v1/sessions/{id}/signal?role=host|viewer`：WebSocket，Bearer 对应角色 token。
- `DELETE /v1/sessions/{id}`：仅 hostToken 删除会话。
- `iceServers` 使用标准 WebRTC `{urls:[...],username,credential}`；expiresAt 为本次 TURN 凭据到期的 RFC3339 时间，默认 10 分钟，客户端提前续取并由 host 执行 ICE restart。活跃 session 不能因这次 TURN TTL 到期被直接删除；会话若有独立 TTL 使用 `sessionExpiresAt`。标准 TURN REST HMAC，禁止匿名分配。
- `video` 可携带 `{width,height,fps,bitrate,profileLevelId,displayId}`，供观看端适配。创建时允许请求该对象，不作为授权依据。
- 信令消息 `{type:"offer"|"answer"|"ice"|"bye",sdp?,candidate?}`。双方 signal 都在线后，服务通知 `{type:"peer",connected:true}`，host 创建 offer，viewer 回 answer。remote description 未就绪时缓存有限的 trickle ICE。

若公网 TURN UDP/TCP 3478 不可达，服务端支持启用专用回环 TURN/TCP（如 3479），经客户端托管的 SSH stdio 隧道连接，响应 `transport:"ssh-tcp"` 和 `turnTcpPort`。两端使用 `ICETransportPolicyRelay`。注册 token、viewer token、TURN credential、SDP/ICE 密码不写入普通日志。

票据采用 `vantry-rtc://<ssh别名>/<sessionId>?token=<viewerToken>`；SSH 别名属于各端自己的配置；没有默认别名，观看端可以明确指定自己的同服务器别名。局域网采用独立的 `vantry-lan://<LAN地址>/<sessionId>?token=…&cert=…` 连接地址；公开 TLS 证书只用于建立本次信令连接，不是源码或构建内容锁。票据仅通过 UI/受信文件交付，不参与模型上下文。

## Native helper ABI

stdout 固定二进制帧：`[type:u8][payload length:u32 BE][payload]`，最大 64MiB；日志只写 stderr。

- 1：hello JSON，`protocol:1, mode, codec:"h264", hardware, width?,height?,fps?,profileLevelId?`。probe 使用 `screenCaptureAccess:"granted"|"denied"` 及显示器信息，普通探测不弹权限框。
- 2：H.264，`Unix微秒:u64 BE + keyframe:u8 + Annex-B bytes`；IDR 含 SPS/PPS。
- 3：RGBA，`Unix微秒:u64 BE + width:u32 BE + height:u32 BE + width*height*4 bytes`。
- 127：error JSON `{code,message}`。

命令：`probe`（无弹窗只读）、`capture --display ID --width W --height H --fps F --bitrate B`、`pattern ... --frames N`、`decode`。capture/pattern stdin 为 NDJSON 控制：keyframe、bitrate、stop；decode stdin 为 type2 帧。require 硬编而非静默软件替代；真实屏幕许可不足明确报错。仅视频，不在本任务扩展音频。

## 媒体与输入

H.264 High 优先、实时编码、无 B 帧、定期 IDR。采集与显示最新帧优先，有限缓冲；不随机丢编码 P 帧破坏参考链，丢失时 RTCP PLI 请求 IDR。保持解码参考链，允许丢已解码的过期显示帧。接收端复用帧缓冲，不能每次 UI 轮询复制未更新的整幅画面。

视频 RTP 携带绝对采集时间扩展；输入通道做 ping/pong 时钟偏移估计。区分网络 RTT、解码耗时与时钟补偿后的估计帧龄，不把 RTT 当端到端延迟。

可靠有序 DataChannel 负责按键、点击/抬起和控制；无重传通道负责可丢的移动/滚动。移动携带最近可靠按钮序号，等待依赖的按钮状态且只保留一条最新移动；可靠释放事件不因较新移动而丢弃。输入验证及串行注入；断开清理本会话按下状态，失焦由宿主先释放持有输入。坐标始终按源显示器的全局点坐标映射，不混用缩放后的观看分辨率。键盘为 macOS 虚拟键码/CGFlags 和已提交 Unicode down/up；本机 IME 组合态不外传。

## 完成标准

1. Native 真硬编/硬解与 synthetic pattern 通过，真实屏幕权限不足不能标真屏采集通过。
2. 局域网验证两个真实 PeerConnection、H.264 与输入通道，且没有外部 TURN/STUN 或 SSH；自建中继另验真实 TURN 与 relay candidate，不能互相代替证据。
3. UI 实际显示视频并控制正确机器；远端 IDE view 的被控操作不落到观看端本机。
4. 首帧、实际帧率、RTT、估计帧龄 P50/P95、丢帧/PLI、断线与退出记录可复核。
5. 两端 App/Core 与插件/helper 正式包匹配；缺 macOS 屏幕录制/辅助功能权限时按需引导用户授权。
