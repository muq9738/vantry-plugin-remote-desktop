#!/usr/bin/env python3
"""End-to-end tests for vantry-remote-helper. Usage: python3 tests/run_tests.py [--skip-capture] [--skip-universal]"""
import datetime
import json
import os
import re
import signal
import struct
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
BIN = os.path.join(ROOT, "bin", "vantry-remote-helper")
VERIFY = [sys.executable, os.path.join(HERE, "verify.py")]
RUN_DIR = os.path.join(ROOT, "test-results", datetime.datetime.now().strftime("run-%Y%m%d-%H%M%S"))
os.makedirs(RUN_DIR, exist_ok=True)
RESULTS = []
SKIP_CAPTURE = "--skip-capture" in sys.argv
SKIP_UNIVERSAL = "--skip-universal" in sys.argv


def path(name):
    return os.path.join(RUN_DIR, name)


def verify(*args):
    out = subprocess.run(VERIFY + list(args), capture_output=True, text=True, check=True).stdout
    return json.loads(out)


def run(cmd, stdin_path=None, stdout_path=None, stderr_path=None, timeout=60, timed=False):
    """Run helper; returns (exit code, wall seconds, max RSS bytes or None)."""
    full = (["/usr/bin/time", "-l"] if timed else []) + cmd
    sin = open(stdin_path, "rb") if stdin_path else subprocess.DEVNULL
    sout = open(stdout_path, "wb") if stdout_path else subprocess.DEVNULL
    serr = open(stderr_path, "wb") if stderr_path else subprocess.DEVNULL
    t0 = time.time()
    try:
        p = subprocess.run(full, stdin=sin, stdout=sout, stderr=serr, timeout=timeout)
    finally:
        for f in (sin, sout, serr):
            if hasattr(f, "close"):
                f.close()
    rss = None
    if timed and stderr_path:
        m = re.search(r"(\d+)\s+maximum resident set size", open(stderr_path, errors="replace").read())
        rss = int(m.group(1)) if m else None
    return p.returncode, time.time() - t0, rss


def final_stats(stderr_path):
    txt = open(stderr_path, errors="replace").read()
    m = re.findall(r"final-stats (\{.*\})", txt)
    w = re.findall(r"writer-final (\{.*\})", txt)
    return (json.loads(m[-1]) if m else None), (json.loads(w[-1]) if w else None), txt


def record(name, status, checks, details):
    RESULTS.append({"test": name, "status": status, "checks": checks, "details": details})
    mark = {"pass": "PASS", "fail": "FAIL", "skip": "SKIP"}[status]
    print(f"[{mark}] {name}")
    for k, v in checks.items():
        if not v:
            print(f"        failed check: {k}")


def test(name):
    def deco(fn):
        def wrapper():
            try:
                res = fn()
                if res is None:
                    return
                checks, details = res
                record(name, "pass" if all(checks.values()) else "fail", checks, details)
            except Exception as e:  # noqa
                record(name, "fail", {"exception": False}, {"error": repr(e)})
        return wrapper
    return deco


def mb(x):
    return round(x / 1024 / 1024, 1) if x else None


# ---------------------------------------------------------------------------

@test("probe")
def t_probe():
    code, wall, _ = run([BIN, "probe"], stdout_path=path("probe.bin"), stderr_path=path("probe.err"))
    recs = verify("records", path("probe.bin"))
    hello = json.loads(open(path("probe.bin"), "rb").read()[5:])
    checks = {
        "exit0": code == 0,
        "singleHelloRecord": recs["count"] == 1 and recs["types"] == [1],
        "protocol1": hello.get("protocol") == 1 and hello.get("mode") == "probe" and hello.get("codec") == "h264",
        "hardwareEncode": hello.get("hardwareEncode") is True,
        "hardwareDecode": hello.get("hardwareDecode") is True,
        "noPrompt": hello.get("permissionPromptIssued") is False,
    }
    hello.pop("displays", None)
    return checks, {"hello": hello, "wallSec": round(wall, 2)}


ENC = path("pattern-1080p.h264rec")


@test("pattern_1080p60_120_frames_hw_encode")
def t_pattern():
    code, wall, rss = run([BIN, "pattern", "--width", "1920", "--height", "1080", "--fps", "60", "--frames", "120"],
                          stdout_path=ENC, stderr_path=path("pattern-1080p.err"), timed=True)
    s = verify("h264", ENC)
    st, wf, _ = final_stats(path("pattern-1080p.err"))
    h = s["hello"] or {}
    checks = {
        "exit0": code == 0,
        "frames==120": s["frames"] == 120,
        "helloFirstAndValid": h.get("protocol") == 1 and h.get("codec") == "h264" and h.get("hardware") is True,
        "helloDims": h.get("width") == 1920 and h.get("height") == 1080 and h.get("fps") == 60,
        "profileConstrainedHigh": (h.get("profileLevelId") or "").startswith("640c") and h.get("profileLevelId") == s.get("firstSpsProfileLevelId"),
        "firstFrameIsIDRwithSPSPPS": s["firstFrameKey"] and s["firstFrameNalTypes"][:3] == [7, 8, 5],
        "keyFlagMatchesIDR": s["keyFlagMatchesIDR"],
        "everyIDRhasSPSPPS": s["idrHaveSpsPps"],
        "noBframesReorder(tsMonotonic)": s["tsMonotonic"],
        "noRawDrops": st and st["encoder"]["droppedRawBackpressure"] == 0,
        "noEncodeErrors": st and st["encoder"]["encodeErrors"] == 0,
        "writerQueueBounded": wf and wf["maxQueueDepth"] <= wf["queueLimit"],
    }
    s.pop("hello")
    return checks, {"summary": s, "hello": h, "encoderStats": st and st["encoder"], "writer": wf,
                    "maxRSSMiB": mb(rss), "wallSec": round(wall, 2)}


@test("decode_1080p_120_frames_hw_rgba_lossless")
def t_decode():
    out = path("decode-1080p.rgbarec")
    code, wall, rss = run([BIN, "decode", "--output-policy", "block"], stdin_path=ENC, stdout_path=out,
                          stderr_path=path("decode-1080p.err"), timed=True)
    s = verify("rgba", out, ENC, "--pattern")
    st, wf, _ = final_stats(path("decode-1080p.err"))
    checks = {
        "exit0OnEOF": code == 0,
        "frames==120": s["frames"] == 120,
        "size1920x1080": s["sizes"] == [[[1920, 1080], 120]],
        "compactRGBA(len==16+w*h*4)": s["sizeConsistent"],
        "colors(RGBA quadrants ±24)": s["colorChecks"] == 480 and s["colorFailures"] == 0,
        "tsPassThrough": s.get("tsSubsetOfEncoded") and s.get("tsOrderPreserved"),
        "helloDecodeHardware": (s["hello"] or {}).get("hardware") is True and (s["hello"] or {}).get("mode") == "decode",
        "decoderHardware": st and st["decoder"]["hardware"] is True,
        "noDecodeErrors": st and st["decoder"]["decodeErrors"] == 0,
        "writerQueueBounded": wf and wf["maxQueueDepth"] <= 2,
    }
    return checks, {"summary": s, "decoderStats": st and st["decoder"], "writer": wf, "maxRSSMiB": mb(rss),
                    "wallSec": round(wall, 2)}


@test("decode_default_drop_oldest_from_file")
def t_decode_drop():
    out = path("decode-1080p-drop.rgbarec")
    code, wall, _ = run([BIN, "decode"], stdin_path=ENC, stdout_path=out, stderr_path=path("decode-1080p-drop.err"))
    s = verify("rgba", out, ENC, "--pattern")
    st, wf, _ = final_stats(path("decode-1080p-drop.err"))
    checks = {
        "exit0": code == 0,
        "decodedAll120": st and st["decoder"]["decoded"] == 120,
        "outputWithinQueueLimit": wf and wf["maxQueueDepth"] <= 2,
        "droppedOnlyStaleRGBA": wf and wf["droppedMedia"] + s["frames"] == 120,
        "lastFrameKept(order)": s.get("tsOrderPreserved"),
        "colors": s["colorFailures"] == 0,
    }
    return checks, {"framesWritten": s["frames"], "writer": wf}


def live_pipeline(tag, rc, frames=300):
    enc_err, dec_err = path(f"live-{tag}-enc.err"), path(f"live-{tag}-dec.err")
    with open(enc_err, "wb") as e1, open(dec_err, "wb") as e2:
        p1 = subprocess.Popen([BIN, "pattern", "--width", "1920", "--height", "1080", "--fps", "60", "--frames", str(frames),
                               "--rate-control", rc], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=e1)
        p2 = subprocess.Popen([BIN, "decode"], stdin=p1.stdout, stdout=subprocess.PIPE, stderr=e2)
        p1.stdout.close()
        p3 = subprocess.run(VERIFY + ["live"], stdin=p2.stdout, capture_output=True, text=True, timeout=60)
        p2.stdout.close()
        c1, c2 = p1.wait(10), p2.wait(10)
    live = json.loads(p3.stdout)
    st1, _, _ = final_stats(enc_err)
    st2, wf2, _ = final_stats(dec_err)
    return c1, c2, live, st1, st2, wf2


@test("live_pipe_pattern1080p60_to_rgba_latency_lowlatency")
def t_live_ll():
    c1, c2, live, st1, st2, wf2 = live_pipeline("lowlatency", "lowlatency")
    checks = {"exit0": c1 == 0 and c2 == 0, "frames>=295": live["frames"] >= 295,
              "p95<50ms": live.get("latencyMs", {}).get("p95", 1e9) < 50}
    return checks, {"live(sourceTs->RGBA arrival in consumer)": live, "encodeLatency": st1["encoder"]["encodeLatency"],
                    "decodeLatency": st2["decoder"]["decodeLatency"], "bgraToRgba": st2["decoder"]["bgraToRgbaLatency"],
                    "decodeWriter": wf2}


@test("live_pipe_pattern1080p60_to_rgba_latency_standard_rc")
def t_live_std():
    c1, c2, live, st1, st2, wf2 = live_pipeline("standard", "standard")
    checks = {"exit0": c1 == 0 and c2 == 0, "frames>=295": live["frames"] >= 295,
              "p95<50ms": live.get("latencyMs", {}).get("p95", 1e9) < 50}
    return checks, {"live": live, "encodeLatency": st1["encoder"]["encodeLatency"],
                    "decodeLatency": st2["decoder"]["decodeLatency"]}


@test("standard_rate_control_hw_property_verified")
def t_std_hw():
    out = path("pattern-standard.h264rec")
    code, _, _ = run([BIN, "pattern", "--width", "1280", "--height", "720", "--frames", "60", "--rate-control", "standard"],
                     stdout_path=out, stderr_path=path("pattern-standard.err"))
    s = verify("h264", out)
    h = s["hello"] or {}
    checks = {"exit0": code == 0, "frames==60": s["frames"] == 60,
              "aveEncoder": h.get("encoderID") == "com.apple.videotoolbox.videoencoder.ave.avc",
              "UsingHardwareAcceleratedVideoEncoder==true": h.get("hardwareEvidence") == "UsingHardwareAcceleratedVideoEncoder" and h.get("hardware") is True,
              "lowLatencyFalse": h.get("lowLatencyRateControl") is False,
              "firstIDR(SPS,PPS[,SEI],IDR)": s["firstFrameNalTypes"][:2] == [7, 8] and 5 in s["firstFrameNalTypes"]}
    return checks, {"hello": h}


@test("dynamic_resolution_reconfigure")
def t_dynamic():
    combo = path("dynamic.h264rec")
    sizes = [(1280, 720), (640, 360), (1366, 768)]
    with open(combo, "wb") as f:
        for w, h in sizes:
            part = path(f"dyn-{w}x{h}.h264rec")
            run([BIN, "pattern", "--width", str(w), "--height", str(h), "--frames", "30", "--fps", "60"],
                stdout_path=part, stderr_path=part + ".err")
            f.write(open(part, "rb").read())
    out = path("dynamic.rgbarec")
    code, _, _ = run([BIN, "decode", "--output-policy", "block"], stdin_path=combo, stdout_path=out, stderr_path=path("dynamic-dec.err"))
    s = verify("rgba", out, combo, "--pattern")
    st, _, _ = final_stats(path("dynamic-dec.err"))
    checks = {"exit0": code == 0, "sizesSequence": s["sizes"] == [[[w, h], 30] for w, h in sizes],
              "threeSessions": st and st["decoder"]["sessionsCreated"] == 3,
              "noDecodeErrors": st and st["decoder"]["decodeErrors"] == 0,
              "colors": s["colorFailures"] == 0, "compact": s["sizeConsistent"]}
    return checks, {"sizes": s["sizes"], "decoder": st and st["decoder"]}


@test("stdin_control_keyframe_and_bitrate")
def t_control():
    out, err = path("control.h264rec"), path("control.err")
    with open(out, "wb") as o, open(err, "wb") as e:
        p = subprocess.Popen([BIN, "pattern", "--width", "1280", "--height", "720", "--fps", "60", "--frames", "240",
                              "--keyframe-interval", "100", "--events"], stdin=subprocess.PIPE, stdout=o, stderr=e)
        time.sleep(1.0)
        p.stdin.write(b'{"action":"keyframe"}\n'); p.stdin.flush()
        time.sleep(1.0)
        p.stdin.write(b'{"action":"bitrate","bitrate":2000000}\n'); p.stdin.flush()
        time.sleep(0.2)
        p.stdin.write(b'not json\n'); p.stdin.flush()
        code = p.wait(15)
    s = verify("h264", out)
    txt = open(err, errors="replace").read()
    ki = s["keyIndices"]
    checks = {"exit0": code == 0, "frames==240": s["frames"] == 240,
              "exactlyInitial+RequestedIDR": len(ki) == 2 and ki[0] == 0 and 40 <= ki[1] <= 90,
              "bitrateApplied": "bitrate -> 2000000 (AverageBitRate=0, DataRateLimits=0)" in txt,
              "malformedLineIgnored": "ignoring malformed line" in txt,
              "eventRecordEmitted(--events)": any(ev.get("event") == "keyframe_requested" for ev in s["events"])}
    return checks, {"keyIndices": ki, "events": s["events"]}


@test("sigterm_endless_pattern_clean_exit")
def t_sigterm_pattern():
    out, err = path("sigterm-pattern.h264rec"), path("sigterm-pattern.err")
    with open(out, "wb") as o, open(err, "wb") as e:
        p = subprocess.Popen([BIN, "pattern", "--frames", "0"], stdin=subprocess.PIPE, stdout=o, stderr=e)
        time.sleep(1.5)
        t0 = time.time()
        p.send_signal(signal.SIGTERM)
        code = p.wait(6)
        dt = time.time() - t0
    s = verify("h264", out)
    st, wf, _ = final_stats(err)
    checks = {"exit0": code == 0, "exitWithin1s": dt < 1.0, "streamNotTruncated": s["frames"] > 50,
              "sessionFlushed(final-stats)": st is not None and st["encoder"]["inFlight"] == 0}
    return checks, {"exitSec": round(dt, 3), "frames": s["frames"]}


@test("sigterm_decode_with_open_stdin")
def t_sigterm_decode():
    out, err = path("sigterm-decode.rgbarec"), path("sigterm-decode.err")
    data = open(ENC, "rb").read()
    with open(out, "wb") as o, open(err, "wb") as e:
        p = subprocess.Popen([BIN, "decode"], stdin=subprocess.PIPE, stdout=o, stderr=e)
        p.stdin.write(data[: len(data) // 2]); p.stdin.flush()  # stdin stays open
        time.sleep(1.0)
        t0 = time.time()
        p.send_signal(signal.SIGTERM)
        code = p.wait(6)
        dt = time.time() - t0
    s = verify("rgba", out)
    checks = {"exit0": code == 0, "exitWithin1s": dt < 1.0, "outputParses": s["sizeConsistent"] and s["frames"] >= 1}
    return checks, {"exitSec": round(dt, 3), "framesOut": s["frames"]}


@test("sigterm_decode_block_policy_stalled_consumer")
def t_sigterm_stalled():
    import threading
    err = path("sigterm-stalled.err")
    data = open(ENC, "rb").read()
    with open(err, "wb") as e:
        # stdout is a pipe nobody reads -> writer and (block policy) decoder stall
        p = subprocess.Popen([BIN, "decode", "--output-policy", "block"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=e)

        def feed():
            try:
                p.stdin.write(data)
                p.stdin.flush()
            except Exception:
                pass
        threading.Thread(target=feed, daemon=True).start()
        time.sleep(1.5)
        t0 = time.time()
        p.send_signal(signal.SIGTERM)
        code = p.wait(8)
        dt = time.time() - t0
    txt = open(err, errors="replace").read()
    return {"exit0": code == 0, "exitWithin3s(no watchdog)": dt < 3.0, "noWatchdog": "watchdog" not in txt}, {"exitSec": round(dt, 3)}


@test("join_mid_gop_waits_for_idr")
def t_mid_gop():
    src = path("gop.h264rec")
    run([BIN, "pattern", "--width", "1280", "--height", "720", "--frames", "120", "--keyframe-interval", "0.5"],
        stdout_path=src, stderr_path=src + ".err")
    full = verify("h264", src)
    cut = path("gop-cut.h264rec")
    subprocess.run(VERIFY + ["drop-first", src, cut, "5"], check=True)
    out = path("gop-cut.rgbarec")
    code, _, _ = run([BIN, "decode", "--output-policy", "block", "--events"], stdin_path=cut, stdout_path=out, stderr_path=path("gop-cut.err"))
    st, _, _ = final_stats(path("gop-cut.err"))
    recs = verify("records", out)
    second_idr = full["keyIndices"][1] if len(full["keyIndices"]) > 1 else None
    d = st["decoder"]
    checks = {"exit0": code == 0, "hadLaterIDR": second_idr is not None,
              "skippedUntilIDR": second_idr is not None and d["skippedWaitingKey"] == second_idr - 5,
              "decodedFromIDR": second_idr is not None and d["decoded"] == 120 - second_idr,
              "noDecodeErrors": d["decodeErrors"] == 0,
              "needKeyframeEvent": any("need_keyframe" in (j or "") for j in recs["json"])}
    return checks, {"keyIndices": full["keyIndices"], "decoder": d}


@test("protocol_errors_and_garbage")
def t_proto():
    res = {}
    big = path("oversize.bin")
    open(big, "wb").write(bytes([2]) + struct.pack(">I", 64 * 1024 * 1024 + 1))
    c1, _, _ = run([BIN, "decode"], stdin_path=big, stdout_path=path("oversize.out"), stderr_path=path("oversize.err"))
    r1 = verify("records", path("oversize.out"))
    trunc = path("trunc.bin")
    open(trunc, "wb").write(bytes([2]) + struct.pack(">I", 100) + b"\x00" * 10)
    c2, _, _ = run([BIN, "decode"], stdin_path=trunc, stdout_path=path("trunc.out"), stderr_path=path("trunc.err"))
    r2 = verify("records", path("trunc.out"))
    garbage = path("garbage.bin")
    with open(garbage, "wb") as f:
        rnd = os.urandom(4000)
        for payload in [struct.pack(">QB", 1, 1) + b"\x00\x00\x00\x01\x67" + rnd[:50],
                        struct.pack(">QB", 2, 0) + b"\x00\x00\x01\x41" + rnd[50:2000], b"\x01\x02", b""]:
            f.write(bytes([2]) + struct.pack(">I", len(payload)) + payload)
        f.write(bytes([9]) + struct.pack(">I", 3) + b"abc")
        f.write(open(ENC, "rb").read())  # valid stream afterwards must still decode
    c3, _, _ = run([BIN, "decode", "--output-policy", "block"], stdin_path=garbage, stdout_path=path("garbage.rgbarec"), stderr_path=path("garbage.err"))
    s3 = verify("rgba", path("garbage.rgbarec"))
    c4, _, _ = run([BIN, "pattern", "--bogus", "1"], stdout_path=path("usage.out"), stderr_path=path("usage.err"))
    r4 = verify("records", path("usage.out"))
    checks = {
        "oversize->type127+exit6": c1 == 6 and r1["types"][-1] == 127 and "payload_too_large" in r1["json"][-1],
        "truncated->type127+exit6": c2 == 6 and "input_truncated" in r2["json"][-1],
        "garbageSurvived+validStreamDecoded": c3 == 0 and s3["frames"] == 120,
        "usage->type127+exit64": c4 == 64 and r4["types"] == [127],
    }
    res.update({"oversize": r1, "trunc": r2, "garbageFramesOut": s3["frames"], "usage": r4})
    return checks, res


@test("backpressure_encoded_flush_and_idr_recovery")
def t_bp_enc():
    out, err = path("bp-enc.h264rec"), path("bp-enc.err")
    with open(err, "wb") as e:
        p1 = subprocess.Popen([BIN, "pattern", "--width", "1280", "--height", "720", "--fps", "60", "--frames", "600",
                               "--no-pace", "--noise", "--bitrate", "20000000", "--queue", "4", "--keyframe-interval", "100"], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=e)
        subprocess.run(VERIFY + ["slow", out, "0.03", "1.5"], stdin=p1.stdout, check=True, timeout=120)
        p1.stdout.close()
        c1 = p1.wait(20)
    st, wf, _ = final_stats(err)
    s = verify("h264", out)
    dout = path("bp-enc.rgbarec")
    c2, _, _ = run([BIN, "decode", "--output-policy", "block"], stdin_path=out, stdout_path=dout, stderr_path=path("bp-enc-dec.err"))
    dst, _, _ = final_stats(path("bp-enc-dec.err"))
    checks = {"exit0": c1 == 0 and c2 == 0,
              "overflowHappened": wf and wf["overflowFlushes"] > 0 and wf["droppedMedia"] > 0,
              "recoveryIDRsForced": s["keyframes"] >= 2,
              "queueNeverAboveLimit": wf and wf["maxQueueDepth"] <= 4,
              "receivedStreamFullyDecodable": dst["decoder"]["decodeErrors"] == 0 and dst["decoder"]["skippedWaitingKey"] == 0
              and dst["decoder"]["decoded"] == s["frames"]}
    return checks, {"writer": wf, "framesReceived": s["frames"], "keyIndices": s["keyIndices"][:20], "decoder": dst["decoder"]}


@test("backpressure_rgba_drop_oldest_slow_consumer")
def t_bp_rgba():
    out = path("bp-rgba.rgbarec")
    with open(path("bp-rgba-enc.err"), "wb") as e1, open(path("bp-rgba-dec.err"), "wb") as e2:
        p1 = subprocess.Popen([BIN, "pattern", "--width", "1920", "--height", "1080", "--fps", "60", "--frames", "180"],
                              stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=e1)
        p2 = subprocess.Popen([BIN, "decode"], stdin=p1.stdout, stdout=subprocess.PIPE, stderr=e2)
        p1.stdout.close()
        subprocess.run(VERIFY + ["slow", out, "0.05", "100"], stdin=p2.stdout, check=True, timeout=120)
        p2.stdout.close()
        c1, c2 = p1.wait(10), p2.wait(10)
    st, wf, _ = final_stats(path("bp-rgba-dec.err"))
    s = verify("rgba", out)
    checks = {"exit0": c1 == 0 and c2 == 0, "decodedAll": st["decoder"]["decoded"] == 180,
              "staleDropped": wf["droppedMedia"] > 0, "queue<=2": wf["maxQueueDepth"] <= 2,
              "orderPreserved": s["frames"] + wf["droppedMedia"] == 180}
    return checks, {"writer": wf, "framesOut": s["frames"]}


def screen_access():
    try:
        hello = json.loads(open(path("probe.bin"), "rb").read()[5:])
        return hello.get("screenCaptureAccess")
    except Exception:
        return None


@test("capture_real_display_sck_to_decode")
def t_capture():
    if SKIP_CAPTURE:
        record("capture_real_display_sck_to_decode", "skip", {}, {"reason": "--skip-capture"})
        return None
    out, err = path("capture.h264rec"), path("capture.err")
    if screen_access() != "granted":
        code, _, _ = run([BIN, "capture"], stdout_path=out, stderr_path=err, timeout=10)
        r = verify("records", out)
        checks = {"permissionDeniedReported": code == 3 and "screen_permission_denied" in r["json"][-1]}
        return checks, {"note": "Screen Recording not granted; capture correctly refused without prompting", "records": r}
    with open(out, "wb") as o, open(err, "wb") as e:
        p = subprocess.Popen([BIN, "capture", "--display", "0", "--width", "1920", "--height", "1080", "--fps", "60",
                              "--bitrate", "6000000"], stdin=subprocess.PIPE, stdout=o, stderr=e)
        time.sleep(2.0)
        p.stdin.write(b'{"action":"keyframe"}\n'); p.stdin.flush()
        time.sleep(1.0)
        p.stdin.write(b'{"action":"stop"}\n'); p.stdin.flush()
        code = p.wait(8)
    s = verify("h264", out)
    st, wf, _ = final_stats(err)
    dout = path("capture.rgbarec")
    c2, _, _ = run([BIN, "decode", "--output-policy", "block"], stdin_path=out, stdout_path=dout, stderr_path=path("capture-dec.err"))
    d = verify("rgba", dout, out)
    dst, _, _ = final_stats(path("capture-dec.err"))
    h = s["hello"] or {}
    checks = {"exit0": code == 0 and c2 == 0, "framesCaptured": s["frames"] >= 2,
              "helloCapture": h.get("mode") == "capture" and h.get("hardware") is True,
              "firstIDRwithSPSPPS": s["firstFrameNalTypes"][:3] == [7, 8, 5],
              "keyframeRequestHonoured": s["keyframes"] >= 2,
              "decodedAll": d["frames"] == s["frames"] and dst["decoder"]["decodeErrors"] == 0,
              "decodedSizeMatchesHello": d["sizes"][0][0] == [h.get("width"), h.get("height")],
              "tsPassThrough": d.get("tsSubsetOfEncoded")}
    return checks, {"hello": h, "summary": {k: s[k] for k in ("frames", "keyIndices", "measuredFps", "approxBitrate", "maxFrameGapMs")},
                    "encoder": st["encoder"], "capture": st["capture"], "decodedSizes": d["sizes"]}


@test("capture_live_pipe_latency")
def t_capture_live():
    if SKIP_CAPTURE or screen_access() != "granted":
        record("capture_live_pipe_latency", "skip", {}, {"reason": "capture skipped or not permitted"})
        return None
    e1p, e2p = path("capture-live-enc.err"), path("capture-live-dec.err")
    with open(e1p, "wb") as e1, open(e2p, "wb") as e2:
        p1 = subprocess.Popen([BIN, "capture", "--fps", "60"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=e1)
        p2 = subprocess.Popen([BIN, "decode"], stdin=p1.stdout, stdout=subprocess.PIPE, stderr=e2)
        p1.stdout.close()

        def stopper():
            time.sleep(4)
            p1.stdin.write(b'{"action":"stop"}\n'); p1.stdin.flush()
        import threading
        threading.Thread(target=stopper, daemon=True).start()
        p3 = subprocess.run(VERIFY + ["live"], stdin=p2.stdout, capture_output=True, text=True, timeout=30)
        c1, c2 = p1.wait(10), p2.wait(10)
    live = json.loads(p3.stdout)
    st1, _, _ = final_stats(e1p)
    checks = {"exit0": c1 == 0 and c2 == 0, "framesReceived": live["frames"] >= 2}
    return checks, {"live(captureTs->RGBA in consumer, same host)": live, "encoder": st1["encoder"], "capture": st1["capture"]}


@test("capture_stops_on_stdin_eof")
def t_capture_eof():
    if SKIP_CAPTURE or screen_access() != "granted":
        record("capture_stops_on_stdin_eof", "skip", {}, {"reason": "capture skipped or not permitted"})
        return None
    t0 = time.time()
    code, _, _ = run([BIN, "capture"], stdout_path=path("capture-eof.out"), stderr_path=path("capture-eof.err"), timeout=10)
    dt = time.time() - t0
    txt = open(path("capture-eof.err"), errors="replace").read()
    return {"exit0": code == 0, "stoppedOnEOF": "stdin EOF" in txt, "within3s": dt < 3}, {"sec": round(dt, 2)}


@test("no_leftover_helper_processes")
def t_leftover():
    time.sleep(0.5)
    r = subprocess.run(["pgrep", "-fl", BIN], capture_output=True, text=True)
    return {"none": r.stdout.strip() == ""}, {"pgrep": r.stdout.strip()}


@test("universal_build_arm64_x86_64")
def t_universal():
    if SKIP_UNIVERSAL:
        record("universal_build_arm64_x86_64", "skip", {}, {"reason": "--skip-universal"})
        return None
    outdir = path("universal-bin")
    r = subprocess.run(["bash", os.path.join(ROOT, "build.sh")], env=dict(os.environ, ARCHS="arm64 x86_64", OUT_DIR=outdir),
                       capture_output=True, text=True, timeout=600)
    archs = subprocess.run(["lipo", "-archs", os.path.join(outdir, "vantry-remote-helper")], capture_output=True, text=True).stdout.split()
    return {"buildOK": r.returncode == 0, "archs": sorted(archs) == ["arm64", "x86_64"]}, {"archs": archs, "x86_64Runtime": "not tested (no Intel host)"}


if __name__ == "__main__":
    if not os.path.exists(BIN):
        sys.exit(f"missing {BIN}; run build.sh first")
    for fn in [t_probe, t_pattern, t_decode, t_decode_drop, t_live_ll, t_live_std, t_std_hw, t_dynamic, t_control,
               t_sigterm_pattern, t_sigterm_decode, t_sigterm_stalled, t_mid_gop, t_proto, t_bp_enc, t_bp_rgba, t_capture, t_capture_live,
               t_capture_eof, t_leftover, t_universal]:
        fn()
    # big intermediate RGBA dumps are not useful artifacts; keep small ones
    for f in os.listdir(RUN_DIR):
        fp = os.path.join(RUN_DIR, f)
        if f.endswith(".rgbarec") and os.path.getsize(fp) > 20 * 1024 * 1024:
            os.remove(fp)
    summary = {"runDir": RUN_DIR, "binary": BIN, "host": os.uname().version,
               "passed": sum(r["status"] == "pass" for r in RESULTS), "failed": sum(r["status"] == "fail" for r in RESULTS),
               "skipped": sum(r["status"] == "skip" for r in RESULTS), "results": RESULTS}
    with open(path("summary.json"), "w") as f:
        json.dump(summary, f, indent=2, sort_keys=True)
    print(f"\n{summary['passed']} passed, {summary['failed']} failed, {summary['skipped']} skipped -> {path('summary.json')}")
    sys.exit(1 if summary["failed"] else 0)
