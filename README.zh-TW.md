# Vantry 遠端桌面外掛

<p>
  <a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a> · <strong>繁體中文</strong>
</p>

[Vantry](https://github.com/muq9738/Vantry) 的獨立原生遠端桌面引擎與外掛，採用 **ScreenCaptureKit + VideoToolbox** 硬體 H.264（macOS）、**DXGI/D3D11 + Media Foundation**（Windows 預覽）、**Pion WebRTC** 與原生鍵鼠注入。

Vantry 主應用程式負責頁面、焦點與機器歸屬；本倉庫負責螢幕擷取、硬體編解碼、WebRTC 媒體與輸入協定，以及可選的自建 SSH/TURN 中繼服務。

> **預設僅區域網路**：未設定中繼時僅在區域網路內直連（`vantry-lan://`），不連線任何外部伺服器；跨網路存取須由使用者顯式設定自建中繼，不會自動回退至第三方基礎設施。

## 連線與設定

- **區域網路模式（預設）**：`defaultRelayHost` 留空。被控端自行啟動臨時 TLS 信令監聽（`vantry-lan://…`），兩端直接交換 WebRTC host candidates，不使用外部 STUN/TURN 或 SSH。關閉共用後會立即停止監聽並撤銷連結。
- **自建中繼模式（可選）**：於自有伺服器部署中繼服務（見 [relayrtc/README.md](relayrtc/README.md)），在每台電腦將 `defaultRelayHost` 設為本機 OpenSSH 主機別名，並於介面中主動選擇自建中繼。
  - 信令透過 SSH 連線至中繼主機的私有 Unix socket（`/run/vantry-rtc/signaling.sock`）。
  - 若公網 TURN UDP/TCP 無法連線，支援經由 `ssh -W 127.0.0.1:3479 <alias>` 通道轉送回環 TURN/TCP（統計標記為 `transport: ssh-tcp`）。
  - 中繼模式下媒體全程採用 DTLS-SRTP 與 relay-only ICE。

工作階段票據（`vantry-lan://…` / `vantry-rtc://…`）、角色 token、TURN 憑證與 SDP/ICE 密碼僅限私有 UI 或 `0600` 檔案保存。

## 按需授權

安裝外掛與啟動應用程式不會主動要求螢幕錄製或輸入控制權限：

- `rtc.ProbePermissions(ctx, helperPath)` 執行無彈窗唯讀狀態偵測。
- `rtc.RequestPermission(ctx, helperPath, kind)`（`screen-capture` 或 `input-control`）僅在使用者明確點擊授權時呼叫。
- `rtc.OpenPermissionSettings(ctx, kind)` 於先前遭拒時開啟系統設定頁面。

在 macOS 13+ 上，螢幕錄製由隨附原生 helper 發起申請，輔助使用則由所屬 Core 處理程序發起；共用開啟狀態不會跨重啟持久化。

## Windows 實作預覽

`native/windows` 提供 DXGI/D3D11 擷取、Media Foundation 硬體 H.264 編解碼及原生 `SendInput` 鍵鼠轉換（相容 macOS 鍵碼/CGFlags、Unicode、DPI 與 `DISPLAYn` 座標）。依實際硬體能力判斷可用性，不靜默回退至軟體編碼。詳見 [native/windows/README.md](native/windows/README.md)。

## Go 整合

```sh
go get github.com/muq9738/vantry-plugin-remote-desktop@latest
```

區域網路主機端呼叫 `rtc.StartHost(ctx, rtc.HostConfig{HelperPath: helperPath})`（不傳入 `SSHHost` 或 `Relay`），觀看端將 `host.Ticket()` 傳給 `rtc.Dial`。

顯式使用自建 SSH 中繼範例：

```go
import "github.com/muq9738/vantry-plugin-remote-desktop/rtc"

relay, err := rtc.NewSSHRelay(rtc.SSHConfig{
    Host:       "my-relay",     // 本機 OpenSSH 別名
    ConfigFile: localSSHConfig, // 可選，本機 SSH 設定路徑
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

觀看端範例：

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
// 於 UI 繪製時呼叫 session.LatestFrame()。
```

### 畫質預設

| 畫質 | 最大解析度 | 影格率上限 | 目標位元率 |
| --- | --- | --- | --- |
| `ultra` | 2560×1440 | 60 fps | 8 Mbps |
| `balanced` | 1920×1080 | 30 fps | 4 Mbps |
| `low` | 1280×720 | 30 fps | 1.5 Mbps |

畫面比例維持來源顯示器原比例。硬體 H.264 編碼不啟用 B 影格，依 RTCP 頻寬回饋動態調整位元率，參考鏈遺失時自動透過 PLI 請求 IDR。

## 輸入與所有權

- 滑鼠座標對應至來源顯示器的 `[0, 1]` 正規化座標（`0/1/2` 分別代表左/右/中鍵）。
- 鍵盤與滑鼠點擊經由可靠有序 DataChannel 傳送；滑鼠移動與滾輪經由無重傳通道並攜帶最近可靠按鈕序號，避免跨通道亂序。
- 工作階段關閉或視窗失焦時自動釋放所有按住的按鍵與滑鼠按鈕。

## 建置與驗證

```sh
go test -race ./... -timeout=120s
go vet ./...
(cd relayrtc && go test -race ./... -timeout=120s)
./native/build.sh
```

完整協定與原生 helper ABI 規格請見 [SPEC-RTC.md](SPEC-RTC.md)。

## 開源授權

MIT License — 詳見 [LICENSE](LICENSE)。
