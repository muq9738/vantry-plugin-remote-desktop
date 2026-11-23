#!/usr/bin/env python3
"""Test-only verifier for the vantry-native-helper record stream (not shipped)."""
import json
import struct
import sys
import time

MAX_PAYLOAD = 64 * 1024 * 1024


def read_records(f):
    while True:
        h = f.read(5)
        if not h:
            return
        if len(h) < 5:
            raise ValueError("truncated header")
        t = h[0]
        n = struct.unpack(">I", h[1:5])[0]
        if n > MAX_PAYLOAD:
            raise ValueError(f"payload too large {n}")
        p = f.read(n)
        if len(p) < n:
            raise ValueError("truncated payload")
        yield t, p


def nals(annexb):
    out, i, n, starts = [], 0, len(annexb), []
    while i + 2 < n:
        if annexb[i] == 0 and annexb[i + 1] == 0 and annexb[i + 2] == 1:
            starts.append((i - 1 if i > 0 and annexb[i - 1] == 0 else i, i + 3))
            i += 3
        else:
            i += 1
    for k, (cs, ns) in enumerate(starts):
        end = starts[k + 1][0] if k + 1 < len(starts) else n
        out.append(annexb[ns:end])
    return out


def h264_summary(path):
    s = {"hello": None, "errors": [], "events": [], "frames": 0, "keyframes": 0, "keyIndices": [],
         "firstFrameKey": None, "firstFrameNalTypes": None, "nalTypes": {}, "nonRefFrames": 0,
         "tsMonotonic": True, "keyFlagMatchesIDR": True, "idrHaveSpsPps": True, "bytes": 0, "ts": [],
         "maxFrameBytes": 0, "prefixNal14or20": 0, "otherRecords": 0}
    with open(path, "rb") as f:
        for t, p in read_records(f):
            if t == 1:
                if s["hello"] is None:
                    s["hello"] = json.loads(p)
                else:
                    s.setdefault("extraHellos", []).append(json.loads(p))
            elif t == 127:
                s["errors"].append(json.loads(p))
            elif t == 4:
                s["events"].append(json.loads(p))
            elif t == 2:
                ts = struct.unpack(">Q", p[:8])[0]
                key = p[8]
                ns = nals(p[9:])
                types = [x[0] & 0x1F for x in ns]
                for ty in types:
                    s["nalTypes"][ty] = s["nalTypes"].get(ty, 0) + 1
                    if ty in (14, 20):
                        s["prefixNal14or20"] += 1
                idr = 5 in types
                if bool(key) != idr:
                    s["keyFlagMatchesIDR"] = False
                if idr and not (7 in types and 8 in types):
                    s["idrHaveSpsPps"] = False
                if s["frames"] == 0:
                    s["firstFrameKey"] = bool(key)
                    s["firstFrameNalTypes"] = types
                    sps = next((x for x in ns if x[0] & 0x1F == 7), None)
                    if sps:
                        s["firstSpsProfileLevelId"] = sps[1:4].hex()
                slices = [x for x in ns if x[0] & 0x1F in (1, 5)]
                if slices and all((x[0] & 0x60) == 0 for x in slices):
                    s["nonRefFrames"] += 1
                if key:
                    s["keyframes"] += 1
                    s["keyIndices"].append(s["frames"])
                if s["ts"] and ts < s["ts"][-1]:
                    s["tsMonotonic"] = False
                s["ts"].append(ts)
                s["frames"] += 1
                s["bytes"] += len(p)
                s["maxFrameBytes"] = max(s["maxFrameBytes"], len(p))
            else:
                s["otherRecords"] += 1
    if len(s["ts"]) > 1:
        dur = (s["ts"][-1] - s["ts"][0]) / 1e6
        s["durationSec"] = round(dur, 3)
        if dur > 0:
            s["measuredFps"] = round((len(s["ts"]) - 1) / dur, 2)
            s["approxBitrate"] = int(s["bytes"] * 8 / dur)
        gaps = [(b - a) / 1000 for a, b in zip(s["ts"], s["ts"][1:])]
        s["maxFrameGapMs"] = round(max(gaps), 2)
    return s


def rgba_summary(path, pattern=False):
    s = {"hello": None, "errors": [], "frames": 0, "sizes": [], "ts": [], "colorChecks": 0, "colorFailures": 0,
         "sizeConsistent": True, "samples": None}
    with open(path, "rb") as f:
        for t, p in read_records(f):
            if t == 1:
                s["hello"] = json.loads(p)
            elif t == 127:
                s["errors"].append(json.loads(p))
            elif t == 3:
                ts, w, h = struct.unpack(">QII", p[:16])
                if len(p) != 16 + w * h * 4:
                    s["sizeConsistent"] = False
                if not s["sizes"] or s["sizes"][-1][0] != [w, h]:
                    s["sizes"].append([[w, h], 0])
                s["sizes"][-1][1] += 1
                s["ts"].append(ts)
                s["frames"] += 1
                if pattern:
                    band = max(8, h // 8)
                    qt = band + (h - band) // 2
                    pts = {"red": (w // 4, band + (qt - band) // 2), "green": (3 * w // 4, band + (qt - band) // 2),
                           "blue": (w // 4, qt + (h - qt) // 2), "white": (3 * w // 4, qt + (h - qt) // 2)}
                    exp = {"red": (255, 0, 0), "green": (0, 255, 0), "blue": (0, 0, 255), "white": (255, 255, 255)}
                    got = {}
                    for name, (x, y) in pts.items():
                        o = 16 + (y * w + x) * 4
                        px = tuple(p[o:o + 4])
                        got[name] = px
                        s["colorChecks"] += 1
                        if any(abs(a - b) > 24 for a, b in zip(px[:3], exp[name])) or px[3] != 255:
                            s["colorFailures"] += 1
                    if s["samples"] is None:
                        s["samples"] = got
    return s


def cmd_h264(argv):
    print(json.dumps({k: v for k, v in h264_summary(argv[0]).items() if k != "ts"}, sort_keys=True))


def cmd_rgba(argv):
    pattern = "--pattern" in argv
    s = rgba_summary(argv[0], pattern)
    if len(argv) > 1 and not argv[1].startswith("--"):
        enc_ts = h264_summary(argv[1])["ts"]
        s["tsSubsetOfEncoded"] = set(s["ts"]).issubset(set(enc_ts))
        s["tsOrderPreserved"] = s["ts"] == sorted(s["ts"])
    s.pop("ts")
    print(json.dumps(s, sort_keys=True))


def cmd_live(argv):
    """Read type3 from stdin as it arrives; latency = arrival unix time - source timestamp."""
    lat, frames, sizes, pattern_fail = [], 0, set(), 0
    for t, p in read_records(sys.stdin.buffer):
        now = time.time_ns() // 1000
        if t == 3:
            ts, w, h = struct.unpack(">QII", p[:16])
            lat.append((now - ts) / 1000)
            sizes.add((w, h))
            frames += 1
    lat.sort()
    out = {"frames": frames, "sizes": sorted(sizes)}
    if lat:
        out.update({"latencyMs": {"min": round(lat[0], 2), "p50": round(lat[len(lat) // 2], 2),
                                  "p95": round(lat[int(len(lat) * 0.95)], 2), "max": round(lat[-1], 2)}})
    print(json.dumps(out, sort_keys=True))


def cmd_slow(argv):
    """Copy records stdin -> file, sleeping per media record for the first `slow_sec` seconds."""
    out, delay, slow_sec = argv[0], float(argv[1]), float(argv[2])
    t0 = time.time()
    with open(out, "wb") as f:
        for t, p in read_records(sys.stdin.buffer):
            f.write(bytes([t]) + struct.pack(">I", len(p)) + p)
            if t in (2, 3) and time.time() - t0 < slow_sec:
                time.sleep(delay)


def cmd_drop_first(argv):
    """Write stream without its first N type2 records (simulate joining mid-GOP)."""
    src, dst, n = argv[0], argv[1], int(argv[2])
    k = 0
    with open(src, "rb") as f, open(dst, "wb") as o:
        for t, p in read_records(f):
            if t == 2:
                k += 1
                if k <= n:
                    continue
            o.write(bytes([t]) + struct.pack(">I", len(p)) + p)


def cmd_records(argv):
    with open(argv[0], "rb") as f:
        recs = [(t, len(p), p[:200].decode("utf-8", "replace") if t in (1, 4, 127) else None) for t, p in read_records(f)]
    print(json.dumps({"count": len(recs), "types": sorted({r[0] for r in recs}),
                      "json": [r[2] for r in recs if r[2] is not None][:8]}))


if __name__ == "__main__":
    {"h264": cmd_h264, "rgba": cmd_rgba, "live": cmd_live, "slow": cmd_slow,
     "drop-first": cmd_drop_first, "records": cmd_records}[sys.argv[1]](sys.argv[2:])
