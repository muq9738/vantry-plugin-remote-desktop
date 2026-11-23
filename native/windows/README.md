# Windows native media helper 交付

这是供主代理导入 `vantry-plugin-remote-desktop/native/windows` 的独立交付目录。实现仅包含 media helper、构建文件和合成数据验收；没有修改共享插件、Go 平台生命周期、macOS helper 或 SendInput，也没有推送远程。

源码入口为 `main.cpp`；`protocol_core.h` 供 helper 和便携测试共用，`decoder_fixture.h` 是 245 字节合成红色 IDR。编译需要 Windows 10+、MSVC C++17 / Windows SDK 和 CMake 3.24+。运行系统需自带 Media Foundation；Windows N/KN 或未启用媒体组件的 Server 镜像需要镜像自身先提供该组件。amd64 是首要目标；ARM64 配置入口已提供，尚未构建验证。

```powershell
# 在 Windows 上，VS 2022 Desktop development with C++ / Windows SDK 已安装
.\build.ps1 -RunAcceptance
# 有实际 GPU 编解码器的私有 runner，必须执行这一条才能验收硬件路径
.\build.ps1 -RunAcceptance -RequireHardware
```

脚本不安装工具、不提权、不修改系统策略。默认 `/MT` 静态 MSVC CRT；manifest 使用 `asInvoker`、`uiAccess=false` 和 PerMonitorV2 DPI awareness。产物是 `build/Release/vantry-remote-helper.exe`。

## 已实现的路径

- `capture`：按 `DXGI_OUTPUT_DESC.DeviceName` 的 `\\.\DISPLAYn` 数字选择输出，`0` 选主屏；Desktop Duplication → GPU 缩放/旋转 → BGRA → D3D11 video processor NV12 → 硬件 H.264 MFT。保留原显示器像素比例、不放大、输出尺寸取偶数。
- `pattern`：生成移动色条、RGB/白色象限、十进制微秒时间戳和位条；可加确定性噪声。使用相同 GPU NV12 / 硬件 MFT 编码路径，不读取桌面。
- `decode`：先用内嵌合成 IDR 实际验证 Microsoft H.264 MFT 的 D3D11/DXVA 输出，再发送 hello；实际输入的每一张图片也要求同一硬件设备上的 NV12 `IMFDXGIBuffer`、`D3D11_BIND_DECODER`、无 CPU access。GPU 视频处理应用 SPS crop，BGRA staging 按 `RowPitch` 拷贝并转紧密 RGBA。
- `probe`：以8帧连续合成输入分别验证低延迟编码、标准编码，并验证解码，未成功的能力报告 false。普通 Default 桌面且当前 session active/unlocked 时，再创建/释放 duplication 验证可用性；**不调用 AcquireNextFrame，不取得桌面像素**。

硬件 MFT 只以 `MFT_ENUM_FLAG_HARDWARE` 枚举，要求 hardware URL、async 标志、D3D11-aware、接受硬件 DXGI manager，并实际产出 SPS/IDR。BPictureCount=0 的设置及读回是硬要求；不支持该控制的硬件 MFT 会被拒绝。Windows 1703+ 通过 `MFTEnum2` 的 adapter LUID 过滤；较早 Windows 10 动态退到同样仅硬件的 `MFTEnumEx`，仍要求设备握手和实际编码。没有软件 codec / WARP 回退。`lowlatency` 尝试启用可选的 AVLowLatencyMode，只有设置成功并读回为真才报告 `lowLatencyRateControl:true`；驱动不支持时继续使用硬件 CBR/无 B 帧并如实报告 false。`hardwareEvidence` 明确写出已完成的检查；LUID 指提供给 MFT 的 D3D11 设备。

微软说明 `CODECAPI_AVDecVideoAcceleration_H264` 对 MF 无效，所以实现不以该开关作为证据。解码始终保留 D3D11 manager；不接受需要清除 manager / CPU 输出才能成功的回退路径。自检验证一张 320×180 的红色 GPU 图片及裁剪，之后重新创建远端流的 decoder，不会把测试图发给宿主。

## 协议与宿主对齐

固定 framing/type2/type3 与现有 `native/PROTOCOL.md` 一致，详见 `PROTOCOL_WINDOWS.md`。stdout 仅二进制 record；stderr 仅固定操作名、错误码和计数，不记录图像、控制消息正文、票据、用户名或设备私有路径。

`probe.displays[]` 包括 `id`、`deviceName`、主屏标志、point/pixel 宽高和 bounds。`id` 与宿主 GetMonitorInfoW 的 DISPLAY 数字一致，绝不使用 DXGI 或 EnumDisplayMonitors 的遍历序号。无法解析/重复 ID 返回 `display_id_unmapped`，不静默选另一屏。Windows 的 `pointBounds` 和 `pixelBounds` 都采用 **physicalPixels**；宿主 Go 获取显示器边界时也需要使用相同 DPI awareness。

Windows 没有 macOS TCC 请求命令。`screenCaptureAccess=granted` 表示当时 session/Default desktop 预检通过并至少一个 DXGI 输出能创建 duplication；不是永久权限。`captureRestriction` 描述锁屏、session disconnected、secure/other desktop、无显示器或 DXGI 创建失败。宿主仍负责用户主动开启远控和选择显示器；helper 不自动弹窗、提权、切换 desktop，也不跨锁屏持续捕获。

## 队列、取消与错误

所有 MFT 调用在 media 线程上；硬件异步 MFT 先 unlock，仅按 NeedInput / HaveOutput credit 调用输入输出。处理 stream change、drain complete、提供/不提供 sample、事件对象释放和 `IMFShutdown`。初始 decoder placeholder type 与实际 SPS stream change 分别处理。

编码按异步输入事件推进，在途最多32张；不以两帧延迟假设阻断需要第3帧的硬件流水线，达到预算仍不输出会明确超时。MF 时间戳用本地 serial，显式映射回原 UnixMicros，不用 FIFO 猜测。编码后逐个 slice 检查无 B 帧；每个 IDR 重加 SPS/PPS，hello 的 profile 从真实 SPS 得到。capture idle 不报错；请求 keyframe 会重编码最后一张缓存图。格式/分辨率变更 drain 旧 decoder，再等 IDR 重建；可恢复的 MF 解码错误发限频 need_keyframe。

stdout 队列有 record 数量和 128 MiB 总载荷上限。H264 满时清空待发 AU，等待带 SPS/PPS 的 IDR 并请求编码器强制 IDR；RGBA 支持 drop-oldest / block。decode stdin 最多两条排队 record，单条先校验 64 MiB 限制再分配。reader/writer 阻塞 IO 用 CancelSynchronousIo 取消，采集等待最长 20ms。退出 drain MFT、取消 stdin、最多 2s 等 stdout；4s清理watchdog防止驱动/IO锁死；终止路径不调用可能阻塞的stderr日志。异步stream-change消耗本次HaveOutput许可，协商后等待新输出事件再调用ProcessOutput。首次 hello/probe 另有 8.5s 启动 watchdog，避免宿主 10s 启动等待无限挂起。watchdog 退出码 70，不伪造能力成功。

## 验收与 CI

`ci/windows-native-helper.yml` 是模板，主代理导入后再放到 `.github/workflows/`。默认 `windows-2022` 构建和运行便携测试，再执行不访问屏幕的能力/合成测试；没有真实 GPU codec 时 summary 明确是 `HARDWARE_SKIPPED`。手动启用私有 `[self-hosted, Windows, X64, vantry-media-gpu]` job 后必须 `-RequireHardware`，硬件缺失或任何合成验收失败均失败。该模板已复制到插件.github/workflows并在Windows托管runner运行；私有GPU runner尚不存在。

验收脚本验证：wire 长度与 truncation、实际 hello 证据、SPS/PPS 与 IDR 标志、无 B 帧、keyframe 与动态 bitrate 命令、背压与 stop、紧密 RGBA/颜色/alpha、UnixMicros 精确保留、320×180 → 352×198 流变更和 crop、未知 record 忽略。**没有任何验收用例调用 capture**。真实屏幕、混合 DPI、旋转、光标和锁屏行为仍需主代理在授权的 Windows 会话验收。

实现作者的本机证据保留于私有交付目录，未把构建日志/SDK复制进源码仓库。主代理导入后重跑portable/CTest与Python测试，并在Windows CI 44034373302、44037564787完成MSVC构建及无屏幕探测；无GPU时结果准确为HARDWARE_SKIPPED。可在Mac用已有工具重做交叉构建，脚本只读现成SDK，不下载：

```sh
python3 tests/cross_compile.py --sdk "$HOME/Library/Caches/cargo-xwin/xwin"
cmake -S . -B build-portable -DBUILD_TESTING=ON
cmake --build build-portable
ctest --test-dir build-portable --output-on-failure
python3 -m unittest discover -s tests -p 'test_*.py' -v
```

交叉构建时现有 Clang 17 比缓存 MSVC STL 旧，脚本使用 `_ALLOW_COMPILER_AND_STL_VERSION_MISMATCH` 跳过其版本门禁；没有修改 SDK。Windows 正式 CMake/MSVC 构建不使用该覆盖。交叉构建成功不等于 Windows 运行或 GPU 验收通过。

## 明确限制与剩余验收

- 主代理已运行Windows MSVC/CMake/PowerShell/GitHub Actions；helper能力探测及协议/工具测试通过，托管runner真实hardwareEncode/hardwareDecode=false。尚未在真实GPU证明驱动接受这些设置，硬件运行结论留给严格GPU CI；没有把HARDWARE_SKIPPED写成通过。
- 输入为 progressive 8-bit 4:2:0 H.264，拒绝 B 帧、交错、10-bit/HDR 和其它 chroma。硬件设备/分辨率/profile 不支持则 error，没有软件兜底。高分辨率能力由实际 decoder surface 验证，不根据 adapter 枚举猜测。
- 视频处理当前使用 BT.709 limited-range NV12，面向 SDR 远程桌面；没有 HDR 色调映射和 VUI 非 BT.709 色彩管理。
- 支持 DXGI color/masked-color/monochrome 独立光标；`--no-cursor` 仅能去除 DXGI 独立光标，无法移除系统已烧入桌面图像的光标。旋转/光标组合尚无 Windows 运行证据。光标在缩放后的 BGRA 上合成，避免读回整张高分辨率源屏幕；这条路径仍需性能测量。
- AcquireNextFrame 返回 DXGI access loss 时，在普通可用 desktop 上最多重建三次；锁屏、secure desktop、断连立即失败，显示器 geometry 改变、GPU device removal 或不可恢复的 MFT 错误由宿主重启 helper。没有跨设备自动迁移。
- 动态 bitrate 若驱动拒绝 ICodecAPI 更新会明确 `encoder_config_failed`，不会伪称设置成功。吞吐/端到端延迟尚未测量。

导入时复制源码、manifest、构建脚本、tests 和协议补充；`evidence/` 与本地构建产物用于复核，不应作为正式插件二进制发布。
