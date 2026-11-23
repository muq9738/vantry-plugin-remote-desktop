# Windows media helper additions — protocol 1

The shared `native/PROTOCOL.md` framing, H264 and RGBA layouts are unchanged. This file describes the Windows implementation; it does not replace the macOS contract.

```text
stdout (all modes): [type:u8][length:u32 BE][payload]
decode stdin:      [type:u8][length:u32 BE][payload]
type 2: [UnixMicros:u64 BE][keyFlag:u8][Annex-B access unit]
type 3: [UnixMicros:u64 BE][width:u32 BE][height:u32 BE][tight RGBA]
```

`length <= 67108864`, including the timestamp/dimension prefix. Tight RGBA dimensions must also fit that limit and be at most 8192 on each axis. Unknown decode records are consumed/skipped. H264 records require keyFlag 0/1 matching actual IDR NAL presence; no B slices are accepted.

## Commands

```text
probe
capture --display 0 --width 1920 --height 1080 --fps 30 --bitrate 6000000
        [--keyframe-interval 2] [--queue 6] [--rate-control lowlatency|standard]
        [--no-cursor] [--events] [--allow-prompt]
pattern --width 1280 --height 720 --fps 30 --frames 120 --bitrate 6000000
        [--keyframe-interval 2] [--queue 6] [--rate-control lowlatency|standard]
        [--no-pace] [--noise] [--events]
decode [--queue 2] [--output-policy drop-oldest|block] [--events]
```

- No audio or SendInput. No Windows request-permission command. `--allow-prompt` is accepted for host compatibility and has no effect. There is no global permission prompt, desktop switching or automatic elevation.
- Display ID is the positive numeric suffix of `DXGI_OUTPUT_DESC.DeviceName` (`\\.\DISPLAY2` → 2). Zero means the attached main output. Invalid/unmappable/ambiguous identities never select an arbitrary output.
- `--width` / `--height` are an even bounding box for capture: preserve aspect, no upscale, round down to even dimensions. Pattern dimensions are exact and even. Minimum requested/encoded dimension is 64. FPS 1..120, bitrate 100000..200000000, queue 1..32, key interval 0.1..3600s. Frames 0 means endless.
- Both rate modes use CBR. `lowlatency` attempts the optional AVLowLatencyMode and only reports lowLatencyRateControl=true after successful set and true readback; an unsupported switch leaves hardware CBR/no-B encoding active with that flag false. `standard` does not enable it. Both require BPictureCount=0 readback and check every emitted slice. No software codec fallback. Dynamic bitrate sets AVEncCommonMeanBitRate; no macOS DataRateLimits claim is made.

## Hello and capability proof

Required keys remain `protocol:1`, `mode`, `codec:"h264"`, `hardware` boolean. Encoding hello is first and immediately precedes the first type2, after an actual SPS/IDR has been produced. Width/height/profileLevelId come from the SPS. Other shared encode fields are present. Added: `platform:"windows"`.

Decode sends hello only after a synthetic red 320x180 IDR was decoded to an actual hardware-device NV12 decoder-bound texture, cropped, converted on the GPU and read back with its color validated. It includes `output:"rgba"`, `decoderID:"CLSID_CMSH264DecoderMFT"` and specific `hardwareEvidence`. It then opens a separate session for the caller's stream. Every later decoded frame must also have GPU decoder-bound output on the supplied device.

Probe emits exactly one type1 on success, including hardwareEncode, hardwareDecode, lowLatencyRateControl, standardRateControlHardware, encoderID, hardwareEvidence, decoderHardwareEvidence, screenCaptureAccess, captureRestriction, captureAuthorization, permissionPromptIssued=false and displays[]. Each true codec capability requires an actual synthetic encode/decode. `hardware` is hardwareEncode && hardwareDecode. Failed capabilities are false and carry an error code; enumeration alone never sets true. No desktop pixels are read by probe.

`screenCaptureAccess` is `granted` only if the current user's session is active/unlocked, the accessible input desktop is the caller's Default desktop, and an attached DXGI output can create a duplication object. Otherwise it is `denied`; captureRestriction supplies the transient reason. This is a current-session capability, not macOS TCC. The host owns user authorization and display selection.

Display object fields:

```json
{
  "id": 2, "deviceName": "\\\\.\\DISPLAY2", "main": false,
  "pixelWidth": 1920, "pixelHeight": 1080,
  "pointWidth": 1920, "pointHeight": 1080,
  "pixelBounds": {"x":1920, "y":0, "width":1920, "height":1080},
  "pointBounds": {"x":1920, "y":0, "width":1920, "height":1080},
  "coordinateSpace": "physicalPixels", "refreshRate": 60
}
```

Negative virtual-desktop origins are supported. Point bounds are Win32 physical coordinates in this DPI-aware process, not a second DIP coordinate system. GetMonitorInfo/SendInput integration must use the same physical space.

## Controls and events

Capture / pattern stdin is bounded ASCII NDJSON:

```json
{"action":"keyframe"}
{"action":"bitrate","bitrate":3000000}
{"action":"stop"}
```

Each line <=4096 bytes; malformed/unknown/duplicate fields are ignored with a fixed diagnostic. Never logs the input line. Capture EOF stops; pattern ignores EOF so finite fixtures can finish. Decode stdin stays binary; EOF drains. Console Ctrl-C / Ctrl-Break requests stop. Host termination via TerminateProcess cannot be made graceful.

With `--events`, type4 `need_keyframe` is emitted at most once every 200ms while awaiting an IDR or after recoverable MF decode errors. Reasons: waiting_for_idr, decode_error. Encode keyframe_requested is best effort, reason control_or_backpressure. No events are emitted before encoding hello.

## Timing, queues and stream changes

Capture maps Desktop Duplication LastPresentTime/LastMouseUpdateTime QPC through a sampled precise FILETIME/QPC anchor to Unix microseconds. Pattern samples the same anchor at generation. Repeated idle snapshots requested by keyframe control use current QPC. Encoded timestamps are clamped to last+1us for decode-order monotonicity. MF itself gets local serial times mapped explicitly back to the original uint64 UnixMicros; decoder copies the uint64 unchanged, including across parameter changes.

At most 32 encode inputs are in flight, governed by asynchronous input credits; the host does not assume that every valid hardware MFT emits output after only two inputs. Capture coalesces to the latest DXGI desktop image rather than queueing raw pictures. Pattern waits for input capacity. Async MFT input/output use event credits; one HaveOutput permits one ProcessOutput call, including a stream-change response. Stream changes renegotiate output types and asynchronous transforms wait for the next output event. H264 output is normalized from Annex-B or AVC lengths; avcC/Annex-B sequence headers provide SPS/PPS. Every IDR carries SPS+PPS and keyFlag derives from NAL 5.

Encoded stdout limit: --queue plus 128 MiB. Overflow discards all pending AUs, gates output until IDR and requests a fresh IDR. It never emits a P-only continuation after discarding a reference chain. Control records are retained; opt-in events are capped. RGBA uses drop-oldest or lossless block with stdin backpressure. Decode reader adds at most two queued records, each <=64 MiB. Driver reference surfaces are owned by MF.

SPS dimensions/crop are parsed for progressive 8-bit 4:2:0 only. New SPS/PPS drain the old session before rebuilding on an IDR; decoded stream changes renegotiate NV12. Readback does not treat NV12 as tight rows: GPU views handle texture layout and array subresources, then BGRA staging rows are copied using RowPitch and swizzled to tight RGBA.

## Error and exit behavior

Type127 is `{code,message}` then a nonzero exit, except that broken stdout cannot receive an error. Diagnostics use stable operation names and HRESULT numbers, without user data.

| Code | Exit | Meaning |
|---|---:|---|
| usage | 64 | Invalid CLI |
| hw_encoder_unavailable / hw_decoder_unavailable / hw_codec_unavailable / video_processor_unavailable | 2 | Required actual hardware path unavailable |
| display_not_found / display_id_unmapped | 4 | Selected identity absent, invalid, unmappable or ambiguous |
| capture_unavailable / capture_start_failed / capture_geometry_changed / capture_protected_content | 4 | Session/desktop restrictions or DXGI capture failure; host may restart after resolving |
| payload_too_large / input_truncated / input_failed / frame_too_large | 6 | Wire limit or input IO violation |
| decoder_format | 1 or 6 | Invalid H264 syntax / invalid type2 prefix or key flag |
| encoder_config_failed / encoder_format / encoder_timestamp / decoder_timestamp / encoder_incomplete / decoder_incomplete | 1 | Required codec setting, actual output or timestamp contract failed |
| mft_* / internal | 1 | Unrecoverable transform / internal failure |

Exit 0: finished finite pattern, stop, EOF or requested console stop. Exit 5: stdout closed/failed or cancelled after its 2s shutdown drain deadline. Exit 70: 4s cleanup or 8.5s startup watchdog. Secure/locked/noninteractive desktops fail explicitly; helper does not capture or request elevation there. DXGI access loss retries only on an ordinary still-accessible desktop with unchanged output geometry.

The default video color path is BT.709 limited-range SDR. HDR, interlace, B frames and non-4:2:0/8-bit streams are rejected or unsupported rather than converted through a software codec.
