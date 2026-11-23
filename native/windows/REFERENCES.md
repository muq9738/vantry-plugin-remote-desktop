# Microsoft first-party references used

Implementation was written from the shared protocol and these Microsoft contracts; no third-party private implementation was read.

- [MFTEnum2](https://learn.microsoft.com/en-us/windows/win32/api/mfapi/nf-mfapi-mftenum2): hardware-only flags, adapter filtering, activation-array ownership and the distinction between software async and hardware MFT categories.
- [MFT_ENUM_ADAPTER_LUID](https://learn.microsoft.com/en-us/windows/win32/medfound/mft-enum-adapter-luid): adapter identity, Windows 10 1703 availability and blob value.
- [Asynchronous MFTs](https://learn.microsoft.com/en-us/windows/win32/medfound/asynchronous-mfts): unlock, event credits, format changes, drain completion and IMFShutdown. GetEvent(NO_WAIT) consumes IMFMediaEventGenerator events without a callback touching codec state.
- [Hardware MFTs](https://learn.microsoft.com/en-us/windows/win32/medfound/hardware-mfts): hardware proxy/driver contract and D3D handshake.
- [MF_SA_D3D11_AWARE](https://learn.microsoft.com/en-us/windows/win32/medfound/mf-sa-d3d11-aware): read-only awareness capability; client must not manufacture the attribute.
- [MFT_MESSAGE_SET_D3D_MANAGER](https://learn.microsoft.com/en-us/windows/win32/medfound/mft-message-set-d3d-manager): IMFDXGIDeviceManager handshake and teardown.
- [Supporting Direct3D 11 video decoding in MF](https://learn.microsoft.com/en-us/windows/win32/medfound/supporting-direct3d-11-video-decoding-in-media-foundation): GPU decoder profiles, multithread protection, decoder-bound texture arrays, DXGI sample allocation. Software fallback requires clearing the manager and renegotiating; this helper does not perform that fallback.
- [H.264 video encoder](https://learn.microsoft.com/en-us/windows/win32/medfound/h-264-video-encoder): output-before-input negotiation, B count before output type, dynamic mean bitrate and the HCK required/optional controls. LowLatencyMode and BPictureCount are optional for hardware certification; this helper treats low latency as best effort but requires confirmed zero B count.
- [H.264 video decoder](https://learn.microsoft.com/en-us/windows/win32/medfound/h-264-video-decoder): CLSID, Annex-B input, placeholder output / stream changes, crop and DXVA constraints.
- [AVDecVideoAcceleration_H264](https://learn.microsoft.com/en-us/windows/win32/codecapi/avdecvideoacceleration-h264-property): has no effect with IMFTransform/Media Foundation, so it is not used as proof.
- [DuplicateOutput](https://learn.microsoft.com/en-us/windows/win32/api/dxgi1_2/nf-dxgi1_2-idxgioutput1-duplicateoutput): same-adapter device, secure desktop access denial, disconnection and duplication limit.
- [Desktop Duplication API](https://learn.microsoft.com/en-us/windows/win32/direct3ddxgi/desktop-dup-api): BGRA surfaces, rotated surfaces and pointer shapes; hotspot is informational.
- [DXGI_OUTDUPL_POINTER_POSITION](https://learn.microsoft.com/en-us/windows/win32/api/dxgi1_2/ns-dxgi1_2-dxgi_outdupl_pointer_position): pointer position relative to output top-left.

Decoder fixture provenance (generated locally; no screen or external input):

```sh
ffmpeg -hide_banner -loglevel error -f lavfi -i color=c=red:s=320x180:r=30 \
  -frames:v 1 -c:v libx264 -profile:v baseline -level:v 3.0 -pix_fmt yuv420p \
  -threads 1 -x264-params 'bframes=0:keyint=1:repeat-headers=1' -f h264 decoder-selftest.h264
```

SEI was removed, leaving only SPS/PPS/IDR (245 bytes). File and embedded header are byte-compared by tests. FFmpeg/x264 is an offline fixture generator, not linked to the helper and not a codec fallback. All runtime self-tests decode these bytes through the hardware-verified MF/D3D11 path.
