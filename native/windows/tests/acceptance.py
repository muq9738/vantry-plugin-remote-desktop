#!/usr/bin/env python3
"""Windows helper acceptance. Uses ONLY probe and synthetic pattern/decode, never capture.
A runner without real GPU codecs reports an explicit hardware SKIP. --require-hardware
turns that into a failure; use it on a private GPU runner for release acceptance.
"""
from __future__ import annotations
import argparse
import hashlib
import json
from pathlib import Path
import struct
import subprocess
import sys
import time

MAX_PAYLOAD = 64 * 1024 * 1024
ROOT = Path(__file__).resolve().parents[1]


def records(data: bytes) -> list[tuple[int, bytes]]:
    result = []
    position = 0
    while position < len(data):
        if len(data) - position < 5:
            raise AssertionError('truncated record header')
        kind, size = struct.unpack_from('>BI', data, position)
        position += 5
        if size > MAX_PAYLOAD:
            raise AssertionError('record exceeds 64 MiB')
        if size > len(data) - position:
            raise AssertionError('truncated record payload')
        result.append((kind, data[position:position + size]))
        position += size
    return result


def pack(kind: int, payload: bytes) -> bytes:
    assert len(payload) <= MAX_PAYLOAD
    return struct.pack('>BI', kind, len(payload)) + payload


def nals(data: bytes) -> list[bytes]:
    # No private/reference implementation is used. This is the wire start-code grammar.
    starts = []
    position = 0
    while position + 2 < len(data):
        if data[position:position + 4] == b'\0\0\0\1':
            starts.append((position, position + 4))
            position += 4
        elif data[position:position + 3] == b'\0\0\1':
            starts.append((position, position + 3))
            position += 3
        else:
            position += 1
    assert starts and not any(data[:starts[0][0]]), 'missing Annex-B start code'
    result = []
    for index, (_, begin) in enumerate(starts):
        end = starts[index + 1][0] if index + 1 < len(starts) else len(data)
        nal = data[begin:end].rstrip(b'\0')
        assert nal and not (nal[0] & 0x80), 'invalid NAL header'
        result.append(nal)
    return result


def slice_type(nal: bytes) -> int:
    rbsp = bytearray()
    zeros = 0
    for value in nal[1:]:
        if zeros >= 2 and value == 3:
            zeros = 0
            continue
        rbsp.append(value)
        zeros = zeros + 1 if value == 0 else 0
    offset = 0

    def bit() -> int:
        nonlocal offset
        assert offset < len(rbsp) * 8, 'truncated slice header'
        value = (rbsp[offset // 8] >> (7 - offset % 8)) & 1
        offset += 1
        return value

    def ue() -> int:
        count = 0
        while not bit():
            count += 1
            assert count <= 30, 'oversized Exp-Golomb integer'
        value = 0
        for _ in range(count):
            value = value * 2 + bit()
        return (1 << count) - 1 + value

    ue()  # first_mb_in_slice
    return ue() % 5


def check_h264(data: bytes, expected_frames: int | None = None) -> tuple[dict, list[tuple[int, bool, bytes]]]:
    stream = records(data)
    assert stream and stream[0][0] == 1, 'hello must be first'
    hello = json.loads(stream[0][1])
    assert hello['protocol'] == 1 and hello['codec'] == 'h264' and hello['hardware'] is True
    evidence = hello.get('hardwareEvidence', '')
    assert 'MFT_ENUM_FLAG_HARDWARE' in evidence and 'actual GPU NV12 input' in evidence
    assert 'BPictureCount readback=0' in evidence
    frames = []
    for kind, payload in stream[1:]:
        assert kind != 127, f'helper error: {payload!r}'
        if kind != 2:
            continue
        assert len(payload) > 9 and payload[8] in (0, 1)
        stamp = struct.unpack_from('>Q', payload)[0]
        units = nals(payload[9:])
        types = [n[0] & 31 for n in units]
        key = 5 in types
        assert key == bool(payload[8]), 'key flag must derive from an IDR NAL'
        for unit in units:
            if unit[0] & 31 in (1, 5):
                assert slice_type(unit) != 1, 'B frame was emitted'
        if key:
            assert types[:2] == [7, 8], 'every IDR must start with SPS and PPS'
            assert hello['profileLevelId'] == units[0][1:4].hex()
        if frames:
            assert stamp > frames[-1][0], 'timestamps must increase without B frames'
        frames.append((stamp, key, payload[9:]))
    assert frames and frames[0][1], 'first access unit must be IDR'
    if expected_frames is not None:
        assert len(frames) == expected_frames, f'expected {expected_frames} H264 frames; got {len(frames)}'
    return hello, frames


def check_rgba(data: bytes, stamps: list[int], sizes: list[tuple[int, int]], check_colors: bool = True) -> list[bytes]:
    stream = records(data)
    assert stream and stream[0][0] == 1
    hello = json.loads(stream[0][1])
    assert hello['mode'] == 'decode' and hello['hardware'] is True and hello['output'] == 'rgba'
    assert 'D3D11_BIND_DECODER' in hello.get('hardwareEvidence', '')
    pictures = []
    for kind, payload in stream[1:]:
        assert kind != 127, f'decode error: {payload!r}'
        if kind != 3:
            continue
        assert len(payload) >= 16
        stamp, width, height = struct.unpack_from('>QII', payload)
        index = len(pictures)
        assert index < len(stamps), 'unexpected decoded frame'
        assert stamp == stamps[index], 'Unix microsecond timestamp was not preserved exactly'
        assert (width, height) == sizes[index], f'crop/stream size mismatch: {(width, height)} != {sizes[index]}'
        pixels = payload[16:]
        assert len(pixels) == width * height * 4, 'RGBA must be tightly packed'
        assert all(pixels[i] == 255 for i in range(3, len(pixels), 4)), 'RGBA alpha'
        if check_colors:
            band = max(32, height // 6)
            top_y = band + (height - band) // 4
            bottom_y = band + (height - band) * 3 // 4
            for x, y, expected in [(width // 4, top_y, (255, 0, 0)), (3 * width // 4, top_y, (0, 255, 0)),
                                   (width // 4, bottom_y, (0, 0, 255)), (3 * width // 4, bottom_y, (255, 255, 255))]:
                at = (y * width + x) * 4
                assert all(abs(pixels[at + k] - expected[k]) <= 45 for k in range(3)), 'quadrant color/channel order'
        pictures.append(pixels)
    assert len(pictures) == len(stamps), f'lossless block decode expected {len(stamps)} frames, got {len(pictures)}'
    return pictures


class Acceptance:
    def __init__(self, helper: Path, output: Path):
        self.helper, self.output = helper.resolve(), output.resolve()
        self.output.mkdir(parents=True, exist_ok=True)
        self.results = []
        self.sequence = 0

    def call(self, args: list[str], data: bytes = b'', expected: int = 0, timeout: int = 35) -> bytes:
        self.sequence += 1
        label = f'{self.sequence:02d}-{args[0]}'
        started = time.monotonic()
        result = subprocess.run([str(self.helper), *args], input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        (self.output / (label + '.stdout.bin')).write_bytes(result.stdout)
        (self.output / (label + '.stderr.txt')).write_bytes(result.stderr)
        self.results.append({'mode': args[0], 'args': args, 'exitCode': result.returncode,
                             'seconds': round(time.monotonic() - started, 3), 'stdoutBytes': len(result.stdout)})
        assert result.returncode == expected, f'{args[0]} exited {result.returncode}, expected {expected}; see {label}.stderr.txt'
        records(result.stdout)
        return result.stdout

    def pattern(self, width=320, height=180, count=12) -> tuple[bytes, dict, list]:
        raw = self.call(['pattern', '--width', str(width), '--height', str(height), '--fps', '30', '--frames', str(count),
                         '--bitrate', '1500000', '--queue', '32', '--keyframe-interval', '30', '--no-pace'])
        info, frames = check_h264(raw, count)
        assert (info['width'], info['height']) == (width, height)
        return raw, info, frames

    def control_and_backpressure(self):
        # Hold stdout briefly, then resume reading while stop drains. This is synthetic noise, never a screen.
        process = subprocess.Popen([str(self.helper), 'pattern', '--width', '640', '--height', '360', '--frames', '0',
                                    '--fps', '120', '--no-pace', '--noise', '--queue', '1'],
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            # First prove that encoding has started, then stop consuming the pipe.
            import threading
            prefix, failures = [], []
            def first_frame():
                try:
                    while True:
                        header = process.stdout.read(5)
                        assert len(header) == 5, 'early EOF before synthetic backpressure test'
                        kind, size = struct.unpack('>BI', header)
                        assert size <= MAX_PAYLOAD
                        payload = bytearray()
                        while len(payload) < size:
                            chunk = process.stdout.read(size - len(payload))
                            assert chunk, 'truncated initial backpressure frame'
                            payload.extend(chunk)
                        prefix.append(header + bytes(payload))
                        if kind == 2:
                            return
                except Exception as exc:
                    failures.append(str(exc))
            first = threading.Thread(target=first_frame, daemon=True)
            first.start()
            first.join(timeout=12)
            assert not first.is_alive() and not failures, f'backpressure startup failed: {failures}'
            time.sleep(0.5)
            rest, err = process.communicate(b'{"action":"keyframe"}\n{"action":"bitrate","bitrate":2000000}\n{"action":"stop"}\n', timeout=8)
            raw = b''.join(prefix) + rest
            (self.output / 'backpressure.stdout.bin').write_bytes(raw)
            (self.output / 'backpressure.stderr.txt').write_bytes(err)
            assert process.returncode == 0, 'stop/backpressure must terminate cleanly'
            check_h264(raw)
            import re
            drops = [int(value) for value in re.findall(rb'droppedOutput=(\d+)', err)]
            assert drops and max(drops) > 0, 'synthetic stall did not exercise the bounded output queue'
            self.results.append({'case': 'synthetic-backpressure-stop', 'exitCode': process.returncode, 'stdoutBytes': len(raw), 'droppedOutput': max(drops)})
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()

    def explicit_control(self):
        # Read four complete AUs before requesting a new IDR, so the request is not coalesced into the initial IDR.
        process = subprocess.Popen([str(self.helper), 'pattern', '--width', '320', '--height', '180', '--frames', '24',
                                    '--fps', '15', '--queue', '32', '--keyframe-interval', '30'],
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        import threading
        packets = []
        failure = []
        errors = []
        def collect_errors():
            errors.append(process.stderr.read())
        stderr_thread = threading.Thread(target=collect_errors, daemon=True)
        stderr_thread.start()
        def read_exact(size):
            data = b''
            while len(data) < size:
                part = process.stdout.read(size - len(data))
                if not part:
                    raise AssertionError('early EOF during control test')
                data += part
            return data
        def read():
            try:
                count = 0
                while True:
                    header = process.stdout.read(5)
                    if not header:
                        break
                    assert len(header) == 5
                    kind, size = struct.unpack('>BI', header)
                    assert size <= MAX_PAYLOAD
                    payload = read_exact(size)
                    packets.append(header + payload)
                    if kind == 2:
                        count += 1
                        if count == 4:
                            process.stdin.write(b'{"action":"keyframe"}\n{"action":"bitrate","bitrate":1200000}\n')
                            process.stdin.flush()
            except Exception as exc:
                failure.append(str(exc))
        thread = threading.Thread(target=read, daemon=True)
        thread.start()
        try:
            process.wait(timeout=15)
            thread.join(timeout=2)
            stderr_thread.join(timeout=2)
            assert not thread.is_alive() and not stderr_thread.is_alive()
            assert process.returncode == 0 and not failure, f'control test: {failure}'
            raw = b''.join(packets)
            (self.output / 'control.stdout.bin').write_bytes(raw)
            (self.output / 'control.stderr.txt').write_bytes(b''.join(errors))
            _, frames = check_h264(raw, 24)
            assert any(key for _, key, _ in frames[4:10]), 'keyframe control did not force a prompt IDR'
            self.results.append({'case': 'keyframe-and-dynamic-bitrate', 'exitCode': process.returncode, 'frames': len(frames)})
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
            for stream in (process.stdin, process.stdout, process.stderr):
                stream.close()

    def run(self, require_hardware: bool) -> dict:
        invalid = self.call(['pattern', '--width', '3'], expected=64)
        assert records(invalid)[0][0] == 127 and json.loads(records(invalid)[0][1])['code'] == 'usage'
        probe_raw = self.call(['probe'])
        probe_packets = records(probe_raw)
        assert len(probe_packets) == 1 and probe_packets[0][0] == 1, 'probe must emit exactly one hello'
        info = json.loads(probe_packets[0][1])
        (self.output / 'probe.json').write_text(json.dumps(info, indent=2) + '\n')
        enc, dec = info['hardwareEncode'], info['hardwareDecode']
        assert info['hardware'] == bool(enc and dec)
        assert info['screenCaptureAccess'] in ('granted', 'denied') and info['permissionPromptIssued'] is False
        ids = set()
        for display in info['displays']:
            name = display['deviceName']
            prefix = r'\\.\DISPLAY'
            assert name.startswith(prefix) and name[len(prefix):].isdigit()
            assert display['id'] == int(name[len(prefix):]) and display['id'] not in ids
            ids.add(display['id'])
            assert display['pixelBounds'] == display['pointBounds']
            assert display['pixelBounds']['width'] == display['pixelWidth']
        if enc:
            assert 'actual GPU NV12 input' in info['hardwareEvidence']
        if dec:
            assert 'D3D11_BIND_DECODER' in info['decoderHardwareEvidence']
            fixture = (ROOT / 'tests/decoder-selftest.h264').read_bytes()
            stamps = [0, 1700000000123456, (1 << 64) - 1]
            encoded = b''.join(pack(2, struct.pack('>QB', stamp, 1) + fixture) for stamp in stamps)
            raw = self.call(['decode', '--output-policy', 'block'], encoded)
            pictures = check_rgba(raw, stamps, [(320, 180)] * len(stamps), check_colors=False)
            center = (90 * 320 + 160) * 4
            assert pictures[0][center] > 160 and pictures[0][center + 1] < 100
            for data, code in [(b'\x02\x04\x00\x00\x01', 'payload_too_large'), (b'\x02\x00\x00', 'input_truncated'),
                               (b'\x02\x00\x00\x00\x14x', 'input_truncated')]:
                out = self.call(['decode'], data, expected=6)
                errors = [json.loads(p) for k, p in records(out) if k == 127]
                assert errors and errors[-1]['code'] == code
            mismatch = self.call(['decode'], pack(2, struct.pack('>QB', 1, 0) + fixture), expected=6)
            assert any(k == 127 and json.loads(p)['code'] == 'decoder_format' for k, p in records(mismatch))
            unknown = self.call(['decode'], pack(90, b'ignored') + pack(1, b'{}'))
            assert not any(k == 3 for k, _ in records(unknown))
        if enc:
            first, _, frames = self.pattern()
            self.explicit_control()
            self.control_and_backpressure()
            if dec:
                second, _, frames2 = self.pattern(352, 198, 4)
                combined = first + second
                out = self.call(['decode', '--output-policy', 'block', '--queue', '2', '--events'], combined)
                pictures = check_rgba(out, [t for t, _, _ in frames + frames2], [(320, 180)] * len(frames) + [(352, 198)] * len(frames2))
                # Moving bar in the top band must survive encode/decode.
                def bar_x(pixels):
                    return [x for x in range(320) if min(pixels[(24 * 320 + x) * 4:(24 * 320 + x) * 4 + 3]) > 170]
                assert bar_x(pictures[0]) != bar_x(pictures[len(frames) - 1]), 'synthetic moving bar is static'
        status = 'PASS' if enc and dec else 'HARDWARE_SKIPPED'
        summary = {'status': status, 'hardwareEncode': enc, 'hardwareDecode': dec, 'captureInvoked': False,
                   'requireHardware': require_hardware, 'helperSHA256': hashlib.sha256(self.helper.read_bytes()).hexdigest(),
                   'cases': self.results, 'hardwareSkipReason': '' if enc and dec else 'Runner did not pass real hardware encoder and decoder self-tests'}
        if require_hardware and not (enc and dec):
            summary['status'] = 'FAIL_REQUIRED_HARDWARE'
        return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--helper', required=True, type=Path)
    parser.add_argument('--artifacts', type=Path, default=ROOT / 'build/acceptance')
    parser.add_argument('--require-hardware', action='store_true')
    args = parser.parse_args()
    test = Acceptance(args.helper, args.artifacts)
    try:
        summary = test.run(args.require_hardware)
    except Exception as exc:
        summary = {'status': 'FAIL', 'error': str(exc), 'cases': test.results, 'captureInvoked': False}
    (test.output / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print(json.dumps(summary, indent=2))
    return 1 if summary['status'].startswith('FAIL') else 0


if __name__ == '__main__':
    sys.exit(main())
