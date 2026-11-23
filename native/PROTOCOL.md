# vantry-native-helper protocol (protocol = 1)

Contract between the Go/MyGo host and the macOS Swift helper. The fixed parts (record framing, types 1/2/3/127, payload layouts) **must not change silently**; additions below are marked *(addition)* and are backward compatible.

## Framing (stdout of every mode, stdin of `decode`)

```
[type:u8][length:u32 big-endian][payload: length bytes]
```

- `length` <= 64 MiB (67108864). The decoder rejects larger records with type127 `payload_too_large` and exits with code 6.
- stdout carries only records. All logs and diagnostics go to stderr, as lines prefixed with `[vantry-native <pid>]`.
- A consumer must ignore record types it does not know. The `decode` reader ignores types 1, 4 and 127, and logs and skips any other unknown type.

| type | name | payload |
|---|---|---|
| 1 | hello | UTF-8 JSON object |
| 2 | H.264 access unit | `unixMicros:u64be` · `keyFrame:u8` · Annex-B bytes |
| 3 | RGBA frame | `unixMicros:u64be` · `width:u32be` · `height:u32be` · RGBA bytes, tightly packed, `width*height*4` |
| 4 | event *(addition, opt-in `--events`)* | UTF-8 JSON `{event, reason}` |
| 127 | error | UTF-8 JSON `{code, message}` |

### type 1 hello

- **request-permission** *(addition)*: invoked only on an explicit user action as `request-permission --kind screen-capture`. It calls `CGRequestScreenCaptureAccess` only when preflight is denied, then emits `{protocol:1,mode:"request-permission",screenCaptureAccess,permissionPromptIssued,helperVersion}`. It does not start capture or media probing. Core input authorization is requested in the Core process with `CGRequestPostEventAccess`; it must not borrow the helper's permission. Ordinary probe/install/startup never invoke this mode.

Media modes and probe always include `protocol` (1), `mode`, `codec` (`"h264"`) and `hardware` (bool). The explicit permission mode below is a diagnostic reply and does not claim media/hardware availability.

- **capture / pattern**: the hello is emitted **once, immediately before the first type2 record**, after the first frame has been encoded, so `profileLevelId` comes from the real SPS. Keys: `width`, `height` (the actual encoded size), `fps`, `profileLevelId` (hex such as `"640c1f"` for Constrained High, with the level chosen automatically by VideoToolbox), `bitrate`, `keyframeIntervalSec`, `lowLatencyRateControl`, `encoderID` and `hardwareEvidence`. Capture adds `displayID`, `sourcePixelWidth` and `sourcePixelHeight`. Pattern adds `frames`.
- **decode**: emitted at start with `{protocol:1, mode:"decode", codec:"h264", hardware:true, output:"rgba"}`.
- **probe**: the only record. It adds `hardwareEncode`, `hardwareDecode`, `lowLatencyRateControl`, `standardRateControlHardware`, `encoderID`, `hardwareEvidence`, `screenCaptureAccess` (`"granted"`/`"denied"` from `CGPreflightScreenCaptureAccess`, which never prompts), `permissionPromptIssued:false`, and `displays[]` (`id`, `main`, `pixelWidth`, `pixelHeight`, `pointWidth`, `pointHeight`, `refreshRate`).

`hardware:true` is only reported when it is verified. Either the session reports `UsingHardwareAcceleratedVideoEncoder == true` (the standard AVE encoder), or the low-latency RTVC encoder was created under `RequireHardwareAcceleratedVideoEncoder = true` while a hardware AVC encoder is listed. In that second case the evidence string says so explicitly. If no hardware encoder is available, the helper emits type127 `hw_encoder_unavailable` and exits with code 2. There is no software fallback.

### type 2 H.264

- `unixMicros` is the Unix epoch in microseconds (`CLOCK_REALTIME`) of the capture instant. In capture mode it comes from the SCK sample PTS (host clock) mapped to wall-clock time. In pattern mode it is the generation time. `decode` copies it unchanged into type3, which lets cross-machine latency be computed with clock-offset compensation.
- `keyFrame` is 1 exactly when the access unit contains an IDR slice (NAL 5). Each IDR AU is prefixed with SPS and PPS (`00 00 00 01` start codes). The standard encoder can also put an SEI (NAL 6) between the PPS and the IDR.
- There are no B-frames (`AllowFrameReordering = false`), so `unixMicros` is monotonic in decode order.
- One record is one access unit.

### type 3 RGBA

`width` and `height` are the display size of the decoded picture with cropping applied: for example a 1920x1080 stream decodes to 1920x1080, not 1088. The size can change between records whenever the stream sends new SPS/PPS.

### type 127 error

`code` is a stable identifier. After emitting a type127 record the helper shuts down with a non-zero exit code. Exception: during an already-started shutdown, errors are only logged.

| code | exit | meaning |
|---|---|---|
| `usage` | 64 | bad CLI arguments |
| `hw_encoder_unavailable` / `hw_decoder_unavailable` | 2 | no hardware H.264 encoder or decoder |
| `screen_permission_denied` | 3 | Screen Recording not granted; the helper did **not** prompt unless `--allow-prompt` was given |
| `display_not_found`, `capture_unavailable`, `capture_start_failed`, `capture_stopped` | 4 | ScreenCaptureKit failures |
| `payload_too_large`, `input_truncated`, `frame_too_large` | 6 | protocol violations |
| `encoder_config_failed`, `encoder_invalidated`, `decoder_format`, `pattern_*`, `internal` | 1 | internal |

Other exit codes: 0 = clean stop (EOF, SIGTERM/SIGINT/SIGHUP, `{action:stop}`, pattern finished). 5 = stdout closed (EPIPE). 70 = the shutdown watchdog fired (cleanup took more than 4 s).

### type 4 event *(addition)*

Only emitted when `--events` is passed, so a strict consumer is unaffected.
- capture/pattern: `{"event":"keyframe_requested","reason":"control"|"backpressure"}`
- decode: `{"event":"need_keyframe","reason":"waiting_for_idr"|"no_session"|"decode_error"|"decode_failed"|"bad_parameter_sets"}`, rate-limited to one every 200 ms. The Go side can map this to an RTCP PLI.

## CLI

```
probe
capture --display ID(0=main) --width 1920 --height 1080 --fps 60 --bitrate 6000000
        [--keyframe-interval 2] [--queue 6] [--rate-control lowlatency|standard]
        [--no-cursor] [--allow-prompt] [--events]
pattern --width 1280 --height 720 --fps 60 --frames 120(0=endless) [--bitrate ..] [--keyframe-interval 2]
        [--queue 6] [--rate-control ..] [--no-pace] [--noise] [--events]
decode  [--queue 2] [--output-policy drop-oldest|block] [--events]
```

- capture: `--width`/`--height` define a bounding box. The output keeps the display's pixel aspect ratio, never upscales, and has even dimensions (a 3024x1964 display with a 1920x1080 box gives 1662x1080). Video only, no audio. The cursor is shown unless `--no-cursor`.
- pattern: synthetic BGRA IOSurface `CVPixelBuffer`s with a moving bar in a gray top band and red/green/blue/white quadrants, sent through the same VT encoder. `--noise` puts random noise in the left half of the top band, which produces large P-frames for stress tests. The quadrants stay unchanged.

### stdin control (capture, pattern): one JSON object per line

```
{"action":"keyframe"}                  next encoded frame is an IDR (use for RTCP PLI/FIR)
{"action":"bitrate","bitrate":3000000} AverageBitRate + DataRateLimits(1.5x/1s); 100k..200M
{"action":"stop"}                      graceful stop, exit 0
```

Malformed lines are logged to stderr and ignored. **On stdin EOF, capture stops** (the parent is gone). Pattern ignores stdin EOF because it is a finite test source.

On a static screen, ScreenCaptureKit delivers no new frames. A keyframe request then re-encodes the last captured frame as an IDR, so a new or recovering viewer gets a picture right away.

## Latency and queue policy

- **Raw frames (capture/pattern)**: at most 2 frames are in flight inside VideoToolbox. If a new frame arrives while 2 are pending, that raw frame is dropped (`droppedRawBackpressure`). SCK `queueDepth = 5`. One of those surfaces may be held for the keyframe re-encode.
- **Encoded frames → stdout**: a writer thread with at most `--queue` (default 6) pending type2 records. When that limit is hit:
  - a frame whose slices all have `nal_ref_idc == 0` (not referenced by any other frame) is dropped by itself;
  - otherwise **all pending type2 records are flushed** (they are stale anyway), output pauses until the next IDR, and the encoder is asked for an IDR. The consumer therefore never sees a broken reference chain: it sees a gap, and the gap always ends with an IDR that carries SPS and PPS.
  - Control records (hello/error/event) are never dropped.
- **Decoded RGBA → stdout**: `--queue` (default 2), `drop-oldest` (keeps the newest pictures). With `--output-policy block` it is lossless instead: the decoder blocks, which back-pressures stdin. That is useful for tests and for consumers that want every frame.
- **Decoder**: VT decodes synchronously, so there is no hidden decoder queue. Parameter sets are parsed from the Annex-B stream. When the SPS/PPS change (new resolution or profile), the session is drained, invalidated and recreated, and an IDR is required again. P-frames received before the first IDR, or after a decode error, are skipped until the next IDR. `kVTInvalidSessionErr` (for example after sleep/wake) leads to a rebuild on the next IDR.

## Shutdown

SIGTERM/SIGINT/SIGHUP, stdin EOF (capture/decode) and `{action:stop}` all take the same path: stop the SCStream, drain the capture queue, run `VTCompressionSessionCompleteFrames` and `Invalidate` (or `WaitForAsynchronousFrames` and `Invalidate` for decode), then drain stdout for up to 2 s and `exit`. A 4 s watchdog forces `_exit` if anything hangs. SIGPIPE is ignored, and a stdout write error gives exit code 5. The helper starts no child processes.

## Stats (stderr)

The helper logs `stats {json}` every 5 s, `final-stats {json}` during shutdown and `writer-final {json}` after the stdout drain. These lines include encode and decode latency (avg/max), capture→encoded latency, source timestamp→RGBA latency, queue depth and limit, and drop counters. They are informational and their format is not part of the ABI.
