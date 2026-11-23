// vantry-native-helper
//
// macOS 13+ native media helper for the Vantry remote desktop plugin.
//   probe    : read-only capability check (no permission prompt, no capture)
//   capture  : ScreenCaptureKit display capture -> VideoToolbox HW H.264 -> type2 records
//   pattern  : synthetic CVPixelBuffer frames   -> VideoToolbox HW H.264 -> type2 records
//   decode   : type2 records on stdin -> VideoToolbox HW H.264 decode -> type3 RGBA records
//
// stdout carries ONLY binary records: [type:u8][length:u32 BE][payload]. Logs go to stderr.
// See PROTOCOL.md next to this file for the full ABI.

import Accelerate
import CoreGraphics
import CoreMedia
import CoreVideo
import Foundation
import ScreenCaptureKit
import VideoToolbox

// MARK: - Constants

enum RecordType: UInt8 {
    case hello = 1
    case h264 = 2
    case rgba = 3
    case event = 4 // opt-in (--events), see PROTOCOL.md
    case error = 127
}

let kProtocolVersion = 1
let kMaxPayload = 64 * 1024 * 1024
let kHelperVersion = "0.1.1"

enum ExitCode: Int32 {
    case ok = 0
    case usage = 64
    case internalError = 1
    case hardwareUnavailable = 2
    case permissionDenied = 3
    case captureFailed = 4
    case ioError = 5
    case protocolError = 6
    case watchdog = 70
}

// MARK: - Utilities

let stderrLock = NSLock()
func log(_ message: String) {
    let line = "[vantry-native \(getpid())] \(message)\n"
    stderrLock.lock()
    line.withCString { ptr in _ = Darwin.write(2, ptr, strlen(ptr)) }
    stderrLock.unlock()
}

@inline(__always) func unixMicros() -> UInt64 {
    clock_gettime_nsec_np(CLOCK_REALTIME) / 1000
}

@inline(__always) func hostSeconds() -> Double {
    CMClockGetTime(CMClockGetHostTimeClock()).seconds
}

@inline(__always) func hostTimeNow() -> CMTime {
    CMClockGetTime(CMClockGetHostTimeClock())
}

func jsonData(_ object: [String: Any]) -> Data {
    (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])) ?? Data("{}".utf8)
}

func jsonString(_ object: [String: Any]) -> String {
    String(decoding: jsonData(object), as: UTF8.self)
}

extension Data {
    mutating func appendBE64(_ v: UInt64) {
        var be = v.bigEndian
        Swift.withUnsafeBytes(of: &be) { append(contentsOf: $0) }
    }

    mutating func appendBE32(_ v: UInt32) {
        var be = v.bigEndian
        Swift.withUnsafeBytes(of: &be) { append(contentsOf: $0) }
    }
}

func storeBE64(_ v: UInt64, _ p: UnsafeMutableRawPointer) {
    p.storeBytes(of: v.bigEndian, as: UInt64.self)
}

func storeBE32(_ v: UInt32, _ p: UnsafeMutableRawPointer) {
    p.storeBytes(of: v.bigEndian, as: UInt32.self)
}

func loadBE64(_ d: Data, _ offset: Int) -> UInt64 {
    var v: UInt64 = 0
    for i in 0..<8 { v = (v << 8) | UInt64(d[d.startIndex + offset + i]) }
    return v
}

/// Blocking write of the whole buffer to a fd. Returns false on EPIPE/other errors.
func writeAll(_ fd: Int32, _ base: UnsafeRawPointer, _ count: Int) -> Bool {
    var off = 0
    while off < count {
        let n = Darwin.write(fd, base + off, count - off)
        if n < 0 {
            if errno == EINTR { continue }
            return false
        }
        off += n
    }
    return true
}

/// Reads exactly `count` bytes. Returns nil on EOF before any byte; throws on short read.
enum ReadResult { case ok, eof, truncated, failed }
func readExact(_ fd: Int32, _ base: UnsafeMutableRawPointer, _ count: Int) -> ReadResult {
    var off = 0
    while off < count {
        let n = Darwin.read(fd, base + off, count - off)
        if n == 0 { return off == 0 ? .eof : .truncated }
        if n < 0 {
            if errno == EINTR { continue }
            return .failed
        }
        off += n
    }
    return .ok
}

final class Counter {
    private var lock = os_unfair_lock()
    private var value = 0
    func add(_ d: Int) -> Int { os_unfair_lock_lock(&lock); value += d; let v = value; os_unfair_lock_unlock(&lock); return v }
    var current: Int { os_unfair_lock_lock(&lock); let v = value; os_unfair_lock_unlock(&lock); return v }
}

final class Flag {
    private var lock = os_unfair_lock()
    private var value = false
    func set() { os_unfair_lock_lock(&lock); value = true; os_unfair_lock_unlock(&lock) }
    /// Returns previous value and clears it.
    func take() -> Bool { os_unfair_lock_lock(&lock); let v = value; value = false; os_unfair_lock_unlock(&lock); return v }
    var isSet: Bool { os_unfair_lock_lock(&lock); let v = value; os_unfair_lock_unlock(&lock); return v }
}

/// Running min/avg/max in milliseconds.
final class LatencyStat {
    private var lock = os_unfair_lock()
    private var n = 0
    private var sum = 0.0
    private var maxV = 0.0
    func add(_ ms: Double) { os_unfair_lock_lock(&lock); n += 1; sum += ms; maxV = max(maxV, ms); os_unfair_lock_unlock(&lock) }
    var json: [String: Any] {
        os_unfair_lock_lock(&lock); defer { os_unfair_lock_unlock(&lock) }
        return ["count": n, "avgMs": n > 0 ? (sum / Double(n) * 1000).rounded() / 1000 : 0, "maxMs": (maxV * 1000).rounded() / 1000]
    }
}

/// VTSessionCopyProperty (+1 value released correctly).
func copySessionValue(_ session: VTSession, _ key: CFString) -> (OSStatus, CFTypeRef?) {
    let out = UnsafeMutablePointer<Unmanaged<CFTypeRef>?>.allocate(capacity: 1)
    out.initialize(to: nil)
    defer { out.deinitialize(count: 1); out.deallocate() }
    let st = VTSessionCopyProperty(session, key: key, allocator: nil, valueOut: UnsafeMutableRawPointer(out))
    guard st == noErr, let v = out.pointee?.takeRetainedValue() else { return (st, nil) }
    return (st, v)
}

func copySessionBool(_ session: VTSession, _ key: CFString) -> (OSStatus, Bool?) {
    let (st, v) = copySessionValue(session, key)
    return (st, v as? Bool)
}

func hardwareH264EncoderListed() -> Bool {
    var list: CFArray?
    guard VTCopyVideoEncoderList(nil, &list) == noErr, let arr = list as? [[String: Any]] else { return false }
    return arr.contains { ($0[kVTVideoEncoderList_CodecType as String] as? NSNumber)?.uint32Value == kCMVideoCodecType_H264 && ($0[kVTVideoEncoderList_IsHardwareAccelerated as String] as? Bool) == true }
}

/// Looks up kVTVideoEncoderList_IsHardwareAccelerated for an encoder ID.
func encoderListIsHardware(_ encoderID: String) -> Bool? {
    var list: CFArray?
    guard VTCopyVideoEncoderList(nil, &list) == noErr, let arr = list as? [[String: Any]] else { return nil }
    guard let entry = arr.first(where: { ($0[kVTVideoEncoderList_EncoderID as String] as? String) == encoderID }) else { return nil }
    return entry[kVTVideoEncoderList_IsHardwareAccelerated as String] as? Bool
}

// MARK: - H.264 bitstream helpers

/// Returns ranges of NAL unit payloads (without start codes) in an Annex-B buffer.
func annexBNALRanges(_ buf: UnsafeRawBufferPointer) -> [Range<Int>] {
    let p = buf.bindMemory(to: UInt8.self)
    let n = p.count
    var starts: [(codeStart: Int, nalStart: Int)] = []
    var i = 0
    while i + 2 < n {
        if p[i] == 0 && p[i + 1] == 0 {
            if p[i + 2] == 1 {
                let codeStart = (i > 0 && p[i - 1] == 0) ? i - 1 : i
                starts.append((codeStart, i + 3))
                i += 3
                continue
            } else if p[i + 2] == 0 {
                i += 1
                continue
            }
            i += 3
            continue
        }
        i += 1
    }
    var ranges: [Range<Int>] = []
    for (idx, s) in starts.enumerated() {
        var end = idx + 1 < starts.count ? starts[idx + 1].codeStart : n
        // trailing zero bytes belong to the next start code / trailing padding
        while end > s.nalStart && p[end - 1] == 0 { end -= 1 }
        if end > s.nalStart { ranges.append(s.nalStart..<end) }
    }
    return ranges
}

func profileLevelId(fromSPS sps: Data) -> String? {
    guard sps.count >= 4 else { return nil }
    let b = [UInt8](sps.prefix(4))
    return String(format: "%02x%02x%02x", b[1], b[2], b[3])
}

// MARK: - Record writer (stdout)

/// Single writer thread for stdout. Media records go through a small bounded queue with
/// a codec-aware drop policy, control records (hello/error/event) are never dropped.
final class RecordWriter {
    enum Policy {
        /// Encoded H.264: never silently drop a reference frame. On overflow the whole pending
        /// queue is flushed and output waits for the next IDR (encoder is asked for one).
        case encoded
        /// Decoded RGBA: keep only the newest frames.
        case dropOldest
        /// Decoded RGBA: block the producer (decoder, hence stdin) while the queue is full. Lossless.
        case block
    }

    private struct Item {
        let type: UInt8
        let payload: Data
        let isMedia: Bool
    }

    private let cond = NSCondition()
    private var queue: [Item] = []
    private var mediaCount = 0
    private var closed = false
    private(set) var failed = false
    private var done = false
    private var waitingForIDR = false
    private var blockReleased = false
    private let policy: Policy
    private let maxMedia: Int
    private let fd: Int32 = 1

    var onNeedKeyframe: (() -> Void)?
    var onWriteFailure: (() -> Void)?

    // stats (guarded by cond)
    private var writtenMedia = 0
    private var writtenBytes = 0
    private var droppedMedia = 0
    private var droppedDiscardable = 0
    private var overflowFlushes = 0
    private var maxQueueSeen = 0
    let writeLatency = LatencyStat()

    init(policy: Policy, maxMedia: Int) {
        self.policy = policy
        self.maxMedia = maxMedia
        let t = Thread { [weak self] in self?.run() }
        t.name = "stdout-writer"
        t.qualityOfService = .userInteractive
        t.start()
    }

    func enqueueControl(_ type: RecordType, _ payload: Data) {
        cond.lock()
        if !closed { queue.append(Item(type: type.rawValue, payload: payload, isMedia: false)) }
        cond.signal()
        cond.unlock()
    }

    /// Returns true if accepted.
    @discardableResult
    func enqueueEncoded(_ payload: Data, isKey: Bool, discardable: Bool) -> Bool {
        var needKey = false
        cond.lock()
        defer {
            cond.unlock()
            if needKey { onNeedKeyframe?() }
        }
        if closed { return false }
        if waitingForIDR && !isKey {
            droppedMedia += 1
            return false
        }
        if isKey { waitingForIDR = false }
        if mediaCount >= maxMedia {
            if discardable && !isKey {
                // nal_ref_idc == 0: no other frame references it, safe to drop alone.
                droppedMedia += 1
                droppedDiscardable += 1
                return false
            }
            // Flush every pending media record: the consumer is too slow and anything queued
            // is stale. The stream restarts cleanly from an IDR.
            let before = queue.count
            queue.removeAll { $0.isMedia }
            droppedMedia += before - queue.count
            mediaCount = 0
            overflowFlushes += 1
            if !isKey {
                droppedMedia += 1
                waitingForIDR = true
                needKey = true
                return false
            }
        }
        queue.append(Item(type: RecordType.h264.rawValue, payload: payload, isMedia: true))
        mediaCount += 1
        maxQueueSeen = max(maxQueueSeen, mediaCount)
        cond.signal()
        return true
    }

    func enqueueRGBA(_ payload: Data) {
        cond.lock()
        defer { cond.unlock() }
        if closed { return }
        if policy == .block {
            while mediaCount >= maxMedia && !closed && !failed && !blockReleased { cond.wait() }
            if closed || failed { return }
        }
        while mediaCount >= maxMedia, let idx = queue.firstIndex(where: { $0.isMedia }) {
            queue.remove(at: idx)
            mediaCount -= 1
            droppedMedia += 1
        }
        queue.append(Item(type: RecordType.rgba.rawValue, payload: payload, isMedia: true))
        mediaCount += 1
        maxQueueSeen = max(maxQueueSeen, mediaCount)
        cond.signal()
    }

    /// Shutdown helper: producers blocked by the .block policy fall back to drop-oldest.
    func releaseBlockedProducers() {
        cond.lock(); blockReleased = true; cond.broadcast(); cond.unlock()
    }

    var queuedMedia: Int { cond.lock(); defer { cond.unlock() }; return mediaCount }

    /// Stop accepting, drain what is queued, wait for the thread (bounded).
    func finish(timeout: TimeInterval = 2.0) {
        cond.lock()
        closed = true
        cond.broadcast()
        let deadline = Date().addingTimeInterval(timeout)
        while !done {
            if !cond.wait(until: deadline) { break }
        }
        cond.unlock()
    }

    var statsJSON: [String: Any] {
        cond.lock(); defer { cond.unlock() }
        return [
            "writtenMedia": writtenMedia, "writtenBytes": writtenBytes, "droppedMedia": droppedMedia,
            "droppedDiscardable": droppedDiscardable, "overflowFlushes": overflowFlushes,
            "maxQueueDepth": maxQueueSeen, "queueLimit": maxMedia, "writeLatency": writeLatency.json,
        ]
    }

    private func run() {
        var header = [UInt8](repeating: 0, count: 5)
        while true {
            cond.lock()
            while queue.isEmpty && !closed { cond.wait() }
            if queue.isEmpty && closed {
                done = true
                cond.broadcast()
                cond.unlock()
                return
            }
            let item = queue.removeFirst()
            if item.isMedia { mediaCount -= 1 }
            cond.broadcast() // wake a blocked producer (.block policy)
            cond.unlock()

            if failed { continue }
            header[0] = item.type
            let len = UInt32(item.payload.count)
            header[1] = UInt8(len >> 24); header[2] = UInt8((len >> 16) & 0xff)
            header[3] = UInt8((len >> 8) & 0xff); header[4] = UInt8(len & 0xff)
            let t0 = hostSeconds()
            var ok = header.withUnsafeBytes { writeAll(fd, $0.baseAddress!, 5) }
            if ok && !item.payload.isEmpty {
                ok = item.payload.withUnsafeBytes { writeAll(fd, $0.baseAddress!, $0.count) }
            }
            if !ok {
                let err = errno
                cond.lock(); failed = true; cond.unlock()
                log("stdout write failed: errno=\(err) (\(String(cString: strerror(err))))")
                onWriteFailure?()
                continue
            }
            writeLatency.add((hostSeconds() - t0) * 1000)
            cond.lock()
            if item.isMedia { writtenMedia += 1 }
            writtenBytes += 5 + item.payload.count
            cond.unlock()
        }
    }
}

/// Synchronous record write, used before a writer exists (usage errors, probe).
func writeRecordSync(_ type: RecordType, _ payload: Data) {
    var rec = Data([type.rawValue])
    rec.appendBE32(UInt32(payload.count))
    rec.append(payload)
    _ = rec.withUnsafeBytes { writeAll(1, $0.baseAddress!, $0.count) }
}

func errorPayload(_ code: String, _ message: String) -> Data {
    jsonData(["code": code, "message": message])
}

// MARK: - Lifecycle

final class Lifecycle {
    static let shared = Lifecycle()
    private let lock = NSLock()
    private var shuttingDown = false
    /// Async cleanup; must call the completion when done.
    var cleanup: ((@escaping () -> Void) -> Void)?
    var writer: RecordWriter?
    private var signalSources: [DispatchSourceSignal] = []

    var isShuttingDown: Bool { lock.lock(); defer { lock.unlock() }; return shuttingDown }

    func installSignalHandlers() {
        signal(SIGPIPE, SIG_IGN)
        for sig in [SIGTERM, SIGINT, SIGHUP] {
            signal(sig, SIG_IGN)
            let src = DispatchSource.makeSignalSource(signal: sig, queue: .global(qos: .userInteractive))
            src.setEventHandler { [weak self] in self?.shutdown(.ok, reason: "signal \(sig)") }
            src.resume()
            signalSources.append(src)
        }
    }

    func fail(_ code: String, _ message: String, exit: ExitCode) {
        if isShuttingDown {
            log("error during shutdown (not reported) \(code): \(message)")
            return
        }
        log("error \(code): \(message)")
        if let w = writer {
            w.enqueueControl(.error, errorPayload(code, message))
        } else {
            writeRecordSync(.error, errorPayload(code, message))
        }
        shutdown(exit, reason: code)
    }

    func shutdown(_ code: ExitCode, reason: String) {
        lock.lock()
        if shuttingDown { lock.unlock(); return }
        shuttingDown = true
        lock.unlock()
        log("shutdown (\(reason))")
        DispatchQueue.global().asyncAfter(deadline: .now() + 4) {
            log("shutdown watchdog fired, forcing exit")
            _exit(code == .ok ? ExitCode.watchdog.rawValue : code.rawValue)
        }
        let finishAll = { [writer] in
            writer?.finish()
            if let w = writer { log("writer-final \(jsonString(w.statsJSON))") }
            log("exit \(code.rawValue)")
            exit(code.rawValue)
        }
        writer?.releaseBlockedProducers()
        if let c = cleanup {
            DispatchQueue.global(qos: .userInteractive).async { c(finishAll) }
        } else {
            DispatchQueue.global(qos: .userInteractive).async(execute: finishAll)
        }
    }
}

// MARK: - Arguments

struct Args {
    let mode: String
    private var values: [String: String] = [:]
    private var flags: Set<String> = []

    init(_ argv: [String]) throws {
        guard argv.count >= 2 else { throw UsageError("missing mode") }
        mode = argv[1]
        var i = 2
        while i < argv.count {
            let a = argv[i]
            guard a.hasPrefix("--") else { throw UsageError("unexpected argument \(a)") }
            let key = String(a.dropFirst(2))
            if Args.booleanFlags.contains(key) {
                flags.insert(key)
                i += 1
                continue
            }
            guard i + 1 < argv.count else { throw UsageError("missing value for \(a)") }
            values[key] = argv[i + 1]
            i += 2
        }
    }

    static let booleanFlags: Set<String> = ["events", "allow-prompt", "no-cursor", "no-pace", "noise"]

    func flag(_ k: String) -> Bool { flags.contains(k) }
    func string(_ k: String) -> String? { values[k] }

    func int(_ k: String, _ def: Int, min lo: Int, max hi: Int) throws -> Int {
        guard let s = values[k] else { return def }
        guard let v = Int(s), v >= lo, v <= hi else { throw UsageError("--\(k) must be an integer in [\(lo), \(hi)]") }
        return v
    }

    func double(_ k: String, _ def: Double, min lo: Double, max hi: Double) throws -> Double {
        guard let s = values[k] else { return def }
        guard let v = Double(s), v >= lo, v <= hi else { throw UsageError("--\(k) must be a number in [\(lo), \(hi)]") }
        return v
    }

    func unknownKeys(allowed: Set<String>) -> [String] {
        (Array(values.keys) + Array(flags)).filter { !allowed.contains($0) }
    }
}

func rateControl(_ args: Args) throws -> Bool {
    switch args.string("rate-control") ?? "lowlatency" {
    case "lowlatency": return true
    case "standard": return false
    default: throw UsageError("--rate-control must be lowlatency or standard")
    }
}

struct UsageError: Error { let message: String; init(_ m: String) { message = m } }

let usageText = """
usage:
  vantry-native-helper probe
  vantry-native-helper request-permission --kind screen-capture
  vantry-native-helper capture [--display ID(0=main)] [--width 1920] [--height 1080] [--fps 60]
                               [--bitrate 6000000] [--keyframe-interval 2] [--queue 6]
                               [--rate-control lowlatency|standard] [--no-cursor] [--allow-prompt] [--events]
  vantry-native-helper pattern [--width 1280] [--height 720] [--fps 60] [--frames 120 (0=endless)]
                               [--bitrate 6000000] [--keyframe-interval 2] [--queue 6] [--no-pace] [--noise] [--events]
                               [--rate-control lowlatency|standard]
  vantry-native-helper decode  [--queue 2] [--output-policy drop-oldest|block] [--events]
stdout: binary records [type:u8][len:u32be][payload]; logs on stderr. See PROTOCOL.md.
"""

// MARK: - Encoder

struct EncoderConfig {
    var width: Int
    var height: Int
    var fps: Int
    var bitrate: Int
    var keyframeIntervalSec: Double
    var maxInFlight = 2
    var lowLatencyRateControl = true
}

enum HelperError: Error {
    case hardwareUnavailable(String)
    case failed(String, String)
}

final class H264Encoder {
    let config: EncoderConfig
    private var session: VTCompressionSession?
    /// Serialises EncodeFrame / property changes against finish(). Never taken by output handlers.
    private let sessionLock = NSLock()
    private(set) var lowLatencyRateControl = false
    private(set) var usingHardware = false
    private(set) var encoderID: String?
    private(set) var hardwareEvidence = ""
    private let writer: RecordWriter?
    private let inFlight = Counter()
    private let forceKey = Flag()
    private var stateLock = os_unfair_lock()
    private var lastKeySubmitUs: UInt64 = 0
    private var submittedAny = false
    private var helloSent = false
    private var bitrate: Int
    var helloExtra: [String: Any] = [:]
    var emitEvents = false

    // stats
    let submitted = Counter()
    let encoded = Counter()
    let keyframes = Counter()
    let droppedRaw = Counter()
    let droppedByEncoder = Counter()
    let encodeErrors = Counter()
    let discardableFrames = Counter()
    let encodeLatency = LatencyStat()
    let captureToEncodedLatency = LatencyStat()
    private(set) var firstSPS: Data?

    init(_ cfg: EncoderConfig, writer: RecordWriter?) throws {
        config = cfg
        bitrate = cfg.bitrate
        self.writer = writer
        try createSession()
    }

    private func createSession() throws {
        func attempt(lowLatency: Bool) -> (OSStatus, VTCompressionSession?) {
            var spec: [CFString: Any] = [kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder: kCFBooleanTrue!]
            if lowLatency { spec[kVTVideoEncoderSpecification_EnableLowLatencyRateControl] = kCFBooleanTrue! }
            var s: VTCompressionSession?
            let st = VTCompressionSessionCreate(
                allocator: nil, width: Int32(config.width), height: Int32(config.height),
                codecType: kCMVideoCodecType_H264, encoderSpecification: spec as CFDictionary,
                imageBufferAttributes: nil, compressedDataAllocator: nil, outputCallback: nil, refcon: nil,
                compressionSessionOut: &s)
            return (st, s)
        }
        var (st, s) = config.lowLatencyRateControl ? attempt(lowLatency: true) : (OSStatus(-1), nil)
        if st == noErr, s != nil {
            lowLatencyRateControl = true
        } else if config.lowLatencyRateControl {
            log("low-latency hardware H.264 encoder unavailable (OSStatus \(st)), retrying hardware encoder without low-latency rate control")
            (st, s) = attempt(lowLatency: false)
        } else {
            (st, s) = attempt(lowLatency: false)
        }
        guard st == noErr, let session = s else {
            throw HelperError.hardwareUnavailable("VTCompressionSessionCreate(H.264, RequireHardwareAcceleratedVideoEncoder) failed: OSStatus \(st)")
        }

        // Hardware is required by the encoder specification (creation fails otherwise). Verify it
        // explicitly: the low-latency encoder does not implement UsingHardwareAcceleratedVideoEncoder
        // (kVTPropertyNotSupportedErr), so fall back to the encoder list entry of the selected EncoderID.
        let (hwSt, hw) = copySessionBool(session, kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder)
        encoderID = copySessionValue(session, kVTCompressionPropertyKey_EncoderID).1 as? String
        if hwSt == noErr, let hw = hw {
            usingHardware = hw
            hardwareEvidence = "UsingHardwareAcceleratedVideoEncoder"
        } else if let id = encoderID, let listHW = encoderListIsHardware(id) {
            usingHardware = listHW
            hardwareEvidence = "VTCopyVideoEncoderList.IsHardwareAccelerated(\(id))"
        } else if lowLatencyRateControl, encoderID?.hasSuffix(".rtvc") == true, hardwareH264EncoderListed() {
            // Low-latency (RTVC) wrapper: created under RequireHardwareAcceleratedVideoEncoder=true,
            // which makes VTCompressionSessionCreate fail when no hardware encoder can be used.
            usingHardware = true
            hardwareEvidence = "RequireHardwareAcceleratedVideoEncoder+\(encoderID!) (HW AVC encoder listed; wrapper does not expose UsingHardwareAcceleratedVideoEncoder)"
        } else {
            usingHardware = false
            hardwareEvidence = "unverifiable (property status \(hwSt), encoderID \(encoderID ?? "nil"))"
        }
        guard usingHardware else {
            VTCompressionSessionInvalidate(session)
            throw HelperError.hardwareUnavailable("encoder session is not hardware accelerated: \(hardwareEvidence)")
        }

        func set(_ key: CFString, _ value: CFTypeRef, required: Bool = false) throws {
            let r = VTSessionSetProperty(session, key: key, value: value)
            if r != noErr {
                if required {
                    VTCompressionSessionInvalidate(session)
                    throw HelperError.failed("encoder_config_failed", "VTSessionSetProperty(\(key)) failed: \(r)")
                }
                log("note: optional encoder property \(key) not applied (OSStatus \(r))")
            }
        }
        try set(kVTCompressionPropertyKey_RealTime, kCFBooleanTrue, required: true)
        try set(kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse, required: true)
        try set(kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_ConstrainedHigh_AutoLevel, required: true)
        try set(kVTCompressionPropertyKey_H264EntropyMode, kVTH264EntropyMode_CABAC)
        try set(kVTCompressionPropertyKey_ExpectedFrameRate, config.fps as CFNumber)
        try set(kVTCompressionPropertyKey_AverageBitRate, config.bitrate as CFNumber, required: true)
        try set(kVTCompressionPropertyKey_DataRateLimits, dataRateLimits(config.bitrate))
        try set(kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, config.keyframeIntervalSec as CFNumber)
        try set(kVTCompressionPropertyKey_MaxKeyFrameInterval, max(1, Int(Double(config.fps) * config.keyframeIntervalSec)) as CFNumber)
        try set(kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality, kCFBooleanTrue)
        try set(kVTCompressionPropertyKey_MaxFrameDelayCount, 0 as CFNumber)
        try set(kVTCompressionPropertyKey_ColorPrimaries, kCVImageBufferColorPrimaries_ITU_R_709_2)
        try set(kVTCompressionPropertyKey_TransferFunction, kCVImageBufferTransferFunction_ITU_R_709_2)
        try set(kVTCompressionPropertyKey_YCbCrMatrix, kCVImageBufferYCbCrMatrix_ITU_R_709_2)
        let prep = VTCompressionSessionPrepareToEncodeFrames(session)
        if prep != noErr { log("note: VTCompressionSessionPrepareToEncodeFrames returned \(prep)") }
        self.session = session
        log("encoder ready: \(config.width)x\(config.height)@\(config.fps) \(config.bitrate)bps hardware=\(usingHardware) via \(hardwareEvidence) encoderID=\(encoderID ?? "?") lowLatencyRC=\(lowLatencyRateControl)")
    }

    private func dataRateLimits(_ bps: Int) -> CFArray {
        // Hard cap: 1.5x average bitrate over any 1s window.
        [Double(bps) * 1.5 / 8.0, 1.0] as CFArray
    }

    func requestKeyframe(reason: String) {
        forceKey.set()
        if emitEvents { writer?.enqueueControl(.event, jsonData(["event": "keyframe_requested", "reason": reason])) }
    }

    func setBitrate(_ bps: Int) {
        sessionLock.lock(); defer { sessionLock.unlock() }
        guard let s = session else { return }
        let a = VTSessionSetProperty(s, key: kVTCompressionPropertyKey_AverageBitRate, value: bps as CFNumber)
        let b = VTSessionSetProperty(s, key: kVTCompressionPropertyKey_DataRateLimits, value: dataRateLimits(bps))
        os_unfair_lock_lock(&stateLock); bitrate = bps; os_unfair_lock_unlock(&stateLock)
        log("bitrate -> \(bps) (AverageBitRate=\(a), DataRateLimits=\(b))")
    }

    var inFlightCount: Int { inFlight.current }

    /// Submits one raw frame. Drops it (returns false) when the encoder already has
    /// `maxInFlight` frames pending: raw frames are disposable, encoded frames are not.
    @discardableResult
    func encode(_ pixelBuffer: CVPixelBuffer, unixUs: UInt64, pts: CMTime, forceKeyframe: Bool = false) -> Bool {
        sessionLock.lock(); defer { sessionLock.unlock() }
        guard let session = session else { return false }
        if inFlight.current >= config.maxInFlight {
            _ = droppedRaw.add(1)
            return false
        }
        var force = forceKey.take() || forceKeyframe
        os_unfair_lock_lock(&stateLock)
        if !submittedAny || unixUs &- lastKeySubmitUs >= UInt64(config.keyframeIntervalSec * 1_000_000) { force = true }
        if force { lastKeySubmitUs = unixUs }
        submittedAny = true
        os_unfair_lock_unlock(&stateLock)

        let props: CFDictionary? = force ? [kVTEncodeFrameOptionKey_ForceKeyFrame: kCFBooleanTrue!] as CFDictionary : nil
        _ = inFlight.add(1)
        _ = submitted.add(1)
        let submitHost = hostSeconds()
        let st = VTCompressionSessionEncodeFrame(
            session, imageBuffer: pixelBuffer, presentationTimeStamp: pts, duration: .invalid,
            frameProperties: props, infoFlagsOut: nil
        ) { [weak self] status, flags, sample in
            self?.handleOutput(status: status, flags: flags, sample: sample, unixUs: unixUs, submitHost: submitHost)
        }
        if st != noErr {
            _ = inFlight.add(-1)
            _ = encodeErrors.add(1)
            if force { forceKey.set() }
            log("VTCompressionSessionEncodeFrame failed: \(st)")
            if st == kVTInvalidSessionErr {
                Lifecycle.shared.fail("encoder_invalidated", "VideoToolbox encoder session became invalid (\(st))", exit: .internalError)
            }
            return false
        }
        return true
    }

    private func handleOutput(status: OSStatus, flags: VTEncodeInfoFlags, sample: CMSampleBuffer?, unixUs: UInt64, submitHost: Double) {
        defer { _ = inFlight.add(-1) }
        if flags.contains(.frameDropped) {
            _ = droppedByEncoder.add(1)
            return
        }
        guard status == noErr, let sample = sample, CMSampleBufferDataIsReady(sample) else {
            _ = encodeErrors.add(1)
            log("encode callback error: status=\(status)")
            forceKey.set()
            return
        }
        encodeLatency.add((hostSeconds() - submitHost) * 1000)
        guard let payload = makeAnnexBPayload(sample, unixUs: unixUs) else {
            _ = encodeErrors.add(1)
            forceKey.set()
            return
        }
        _ = encoded.add(1)
        if payload.isKey { _ = keyframes.add(1) }
        if payload.discardable { _ = discardableFrames.add(1) }

        var hello: [String: Any]?
        os_unfair_lock_lock(&stateLock)
        if !helloSent {
            helloSent = true
            var h: [String: Any] = [
                "protocol": kProtocolVersion, "codec": "h264", "hardware": usingHardware,
                "width": config.width, "height": config.height, "fps": config.fps,
                "bitrate": bitrate, "lowLatencyRateControl": lowLatencyRateControl,
                "keyframeIntervalSec": config.keyframeIntervalSec, "helperVersion": kHelperVersion,
                "encoderID": encoderID ?? "", "hardwareEvidence": hardwareEvidence,
            ]
            if let sps = firstSPS, let pl = profileLevelId(fromSPS: sps) { h["profileLevelId"] = pl }
            for (k, v) in helloExtra { h[k] = v }
            hello = h
        }
        os_unfair_lock_unlock(&stateLock)
        if let hello = hello { writer?.enqueueControl(.hello, jsonData(hello)) }

        captureToEncodedLatency.add(Double(Int64(bitPattern: unixMicros() &- unixUs)) / 1000)
        writer?.enqueueEncoded(payload.data, isKey: payload.isKey, discardable: payload.discardable)
    }

    private struct EncodedPayload { let data: Data; let isKey: Bool; let discardable: Bool }

    private func makeAnnexBPayload(_ sample: CMSampleBuffer, unixUs: UInt64) -> EncodedPayload? {
        guard let block = CMSampleBufferGetDataBuffer(sample), let fmt = CMSampleBufferGetFormatDescription(sample) else { return nil }
        let total = CMBlockBufferGetDataLength(block)
        var nalHeaderLen: Int32 = 4
        var psCount = 0
        CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, parameterSetIndex: 0, parameterSetPointerOut: nil, parameterSetSizeOut: nil, parameterSetCountOut: &psCount, nalUnitHeaderLengthOut: &nalHeaderLen)
        guard (1...4).contains(nalHeaderLen) else { return nil }

        var src = Data(count: total)
        let copySt = src.withUnsafeMutableBytes { CMBlockBufferCopyDataBytes(block, atOffset: 0, dataLength: total, destination: $0.baseAddress!) }
        guard copySt == noErr else { return nil }

        // Collect slice NALs first to learn if this is an IDR.
        var nals: [Range<Int>] = []
        var hasIDR = false
        var anyRef = false
        src.withUnsafeBytes { raw in
            let p = raw.bindMemory(to: UInt8.self)
            var off = 0
            while off + Int(nalHeaderLen) <= total {
                var len = 0
                for i in 0..<Int(nalHeaderLen) { len = (len << 8) | Int(p[off + i]) }
                off += Int(nalHeaderLen)
                guard len > 0, off + len <= total else { break }
                let hdr = p[off]
                let type = hdr & 0x1f
                if type == 5 { hasIDR = true }
                if (type == 1 || type == 5) && (hdr & 0x60) != 0 { anyRef = true }
                nals.append(off..<(off + len))
                off += len
            }
        }
        guard !nals.isEmpty else { return nil }

        var out = Data()
        out.reserveCapacity(9 + total + 256)
        out.appendBE64(unixUs)
        out.append(hasIDR ? 1 : 0)
        let startCode: [UInt8] = [0, 0, 0, 1]
        if hasIDR {
            for i in 0..<psCount {
                var ptr: UnsafePointer<UInt8>?
                var size = 0
                if CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, parameterSetIndex: i, parameterSetPointerOut: &ptr, parameterSetSizeOut: &size, parameterSetCountOut: nil, nalUnitHeaderLengthOut: nil) == noErr, let ptr = ptr {
                    out.append(contentsOf: startCode)
                    out.append(ptr, count: size)
                    if i == 0 && firstSPS == nil { firstSPS = Data(bytes: ptr, count: size) }
                }
            }
        }
        for r in nals {
            out.append(contentsOf: startCode)
            out.append(src[r])
        }
        return EncodedPayload(data: out, isKey: hasIDR, discardable: !anyRef && !hasIDR)
    }

    func finish() {
        sessionLock.lock(); defer { sessionLock.unlock() }
        guard let s = session else { return }
        let st = VTCompressionSessionCompleteFrames(s, untilPresentationTimeStamp: .invalid)
        if st != noErr { log("VTCompressionSessionCompleteFrames: \(st)") }
        VTCompressionSessionInvalidate(s)
        session = nil
    }

    var statsJSON: [String: Any] {
        [
            "submitted": submitted.current, "encoded": encoded.current, "keyframes": keyframes.current,
            "discardableFrames": discardableFrames.current, "droppedRawBackpressure": droppedRaw.current,
            "droppedByEncoder": droppedByEncoder.current, "encodeErrors": encodeErrors.current,
            "inFlight": inFlight.current, "maxInFlight": config.maxInFlight,
            "encodeLatency": encodeLatency.json, "captureToEncodedLatency": captureToEncodedLatency.json,
            "hardware": usingHardware, "lowLatencyRateControl": lowLatencyRateControl,
        ]
    }
}

// MARK: - stdin control (capture / pattern)

func startControlReader(encoder: H264Encoder, stopOnEOF: Bool, onKeyframe: (() -> Void)? = nil) {
    let t = Thread {
        while let line = readLine(strippingNewline: true) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty { continue }
            guard let obj = (try? JSONSerialization.jsonObject(with: Data(trimmed.utf8))) as? [String: Any],
                  let action = obj["action"] as? String else {
                log("control: ignoring malformed line: \(trimmed.prefix(200))")
                continue
            }
            switch action {
            case "keyframe":
                encoder.requestKeyframe(reason: "control")
                onKeyframe?()
            case "bitrate":
                if let n = (obj["bitrate"] as? NSNumber)?.intValue, n >= 100_000, n <= 200_000_000 {
                    encoder.setBitrate(n)
                } else {
                    log("control: invalid bitrate \(String(describing: obj["bitrate"]))")
                }
            case "stop":
                Lifecycle.shared.shutdown(.ok, reason: "control stop")
                return
            default:
                log("control: unknown action \(action)")
            }
        }
        if stopOnEOF {
            Lifecycle.shared.shutdown(.ok, reason: "stdin EOF")
        } else {
            log("control: stdin EOF (ignored in this mode)")
        }
    }
    t.name = "stdin-control"
    t.start()
}

func startStatsTimer(_ every: Double, _ produce: @escaping () -> [String: Any]) -> DispatchSourceTimer {
    let t = DispatchSource.makeTimerSource(queue: .global(qos: .utility))
    t.schedule(deadline: .now() + every, repeating: every)
    t.setEventHandler { log("stats \(jsonString(produce()))") }
    t.resume()
    return t
}

// MARK: - Explicit permission request

// This command is separate from probe/capture. Only a user-triggered action
// may invoke it; normal startup, installation and checks never request access.
func runPermissionRequest(_ args: Args) throws -> Never {
    guard args.string("kind") == "screen-capture", args.unknownKeys(allowed: ["kind"]).isEmpty else {
        throw UsageError("request-permission requires --kind screen-capture")
    }
    let before = CGPreflightScreenCaptureAccess()
    let granted = before || CGRequestScreenCaptureAccess()
    writeRecordSync(.hello, jsonData(["protocol": kProtocolVersion, "mode": "request-permission", "screenCaptureAccess": granted ? "granted" : "denied", "permissionPromptIssued": !before, "helperVersion": kHelperVersion]))
    exit(ExitCode.ok.rawValue)
}

// MARK: - Probe

func runProbe() -> Never {
    let screenAccess = CGPreflightScreenCaptureAccess() // never prompts
    var encOK = false
    var lowLatency = false
    var encErr: String?
    var encEvidence = ""
    var encID: String?
    do {
        let enc = try H264Encoder(EncoderConfig(width: 640, height: 360, fps: 30, bitrate: 1_000_000, keyframeIntervalSec: 2), writer: nil)
        encOK = enc.usingHardware
        lowLatency = enc.lowLatencyRateControl
        encEvidence = enc.hardwareEvidence
        encID = enc.encoderID
        enc.finish()
    } catch HelperError.hardwareUnavailable(let m) {
        encErr = m
    } catch {
        encErr = "\(error)"
    }
    var standardHW: Bool?
    if let std = try? H264Encoder(EncoderConfig(width: 640, height: 360, fps: 30, bitrate: 1_000_000, keyframeIntervalSec: 2, lowLatencyRateControl: false), writer: nil) {
        standardHW = std.usingHardware
        std.finish()
    }
    let decOK = VTIsHardwareDecodeSupported(kCMVideoCodecType_H264)

    var displayIDs = [CGDirectDisplayID](repeating: 0, count: 16)
    var count: UInt32 = 0
    CGGetActiveDisplayList(16, &displayIDs, &count)
    let mainID = CGMainDisplayID()
    let displays: [[String: Any]] = displayIDs.prefix(Int(count)).map { id in
        let b = CGDisplayBounds(id)
        let mode = CGDisplayCopyDisplayMode(id)
        return [
            "id": Int(id), "main": id == mainID,
            "pointWidth": Int(b.width), "pointHeight": Int(b.height),
            "pixelWidth": mode?.pixelWidth ?? 0, "pixelHeight": mode?.pixelHeight ?? 0,
            "refreshRate": mode?.refreshRate ?? 0,
        ]
    }
    var hello: [String: Any] = [
        "protocol": kProtocolVersion, "mode": "probe", "codec": "h264",
        "hardware": encOK && decOK, "hardwareEncode": encOK, "hardwareDecode": decOK,
        "lowLatencyRateControl": lowLatency,
        "screenCaptureAccess": screenAccess ? "granted" : "denied",
        "permissionPromptIssued": false, "displays": displays,
        "helperVersion": kHelperVersion,
        "os": ProcessInfo.processInfo.operatingSystemVersionString,
    ]
    if let e = encErr { hello["hardwareEncodeError"] = e }
    hello["hardwareEvidence"] = encEvidence
    hello["encoderID"] = encID ?? ""
    hello["standardRateControlHardware"] = standardHW ?? false
    writeRecordSync(.hello, jsonData(hello))
    exit(ExitCode.ok.rawValue)
}

// MARK: - Pattern source

final class PatternSource {
    let width: Int
    let height: Int
    private let pool: CVPixelBufferPool
    private let band: Int
    private let rowTopLeft: [UInt8]   // red | green
    private let rowBottom: [UInt8]    // blue | white
    private let noise: Bool

    init(width: Int, height: Int, noise: Bool = false) throws {
        self.width = width
        self.height = height
        self.noise = noise
        let attrs: [CFString: Any] = [
            kCVPixelBufferPixelFormatTypeKey: kCVPixelFormatType_32BGRA,
            kCVPixelBufferWidthKey: width, kCVPixelBufferHeightKey: height,
            kCVPixelBufferIOSurfacePropertiesKey: [:] as CFDictionary,
        ]
        var p: CVPixelBufferPool?
        let st = CVPixelBufferPoolCreate(nil, [kCVPixelBufferPoolMinimumBufferCountKey: 4] as CFDictionary, attrs as CFDictionary, &p)
        guard st == kCVReturnSuccess, let pool = p else { throw HelperError.failed("pattern_pool_failed", "CVPixelBufferPoolCreate: \(st)") }
        self.pool = pool
        band = max(8, height / 8)
        func row(_ left: [UInt8], _ right: [UInt8]) -> [UInt8] {
            var r = [UInt8](repeating: 0, count: width * 4)
            for x in 0..<width {
                let c = x < width / 2 ? left : right
                r[x * 4] = c[0]; r[x * 4 + 1] = c[1]; r[x * 4 + 2] = c[2]; r[x * 4 + 3] = c[3]
            }
            return r
        }
        // BGRA byte order
        rowTopLeft = row([0, 0, 255, 255], [0, 255, 0, 255])     // red | green
        rowBottom = row([255, 0, 0, 255], [255, 255, 255, 255]) // blue | white
    }

    /// Top band: gray with a moving black bar; below: red/green over blue/white quadrants.
    func frame(_ index: Int) -> CVPixelBuffer? {
        var pbOut: CVPixelBuffer?
        guard CVPixelBufferPoolCreatePixelBuffer(nil, pool, &pbOut) == kCVReturnSuccess, let pb = pbOut else { return nil }
        CVPixelBufferLockBaseAddress(pb, [])
        defer { CVPixelBufferUnlockBaseAddress(pb, []) }
        guard let base = CVPixelBufferGetBaseAddress(pb) else { return nil }
        let stride = CVPixelBufferGetBytesPerRow(pb)
        let barW = max(8, width / 16)
        let barX = (index * max(4, width / 120)) % max(1, width - barW)
        var bandRow = [UInt8](repeating: 128, count: width * 4)
        for x in barX..<(barX + barW) {
            bandRow[x * 4] = 0; bandRow[x * 4 + 1] = 0; bandRow[x * 4 + 2] = 0
        }
        for x in 0..<width { bandRow[x * 4 + 3] = 255 }
        let quadTop = band + (height - band) / 2
        bandRow.withUnsafeBytes { b in
            rowTopLeft.withUnsafeBytes { t in
                rowBottom.withUnsafeBytes { bo in
                    for y in 0..<height {
                        let src = y < band ? b : (y < quadTop ? t : bo)
                        memcpy(base + y * stride, src.baseAddress!, width * 4)
                    }
                }
            }
        }
        if noise {
            // High-entropy band (left half of the top band) -> large P frames, stresses rate control/backpressure.
            for y in 0..<band { arc4random_buf(base + y * stride, (width / 2) * 4) }
        }
        return pb
    }
}

func runPattern(_ args: Args) throws -> Never {
    let allowed: Set<String> = ["width", "height", "fps", "frames", "bitrate", "keyframe-interval", "queue", "no-pace", "events", "rate-control", "noise"]
    if let bad = args.unknownKeys(allowed: allowed).first { throw UsageError("unknown option --\(bad) for pattern") }
    let width = try args.int("width", 1280, min: 16, max: 8192) & ~1
    let height = try args.int("height", 720, min: 16, max: 8192) & ~1
    let fps = try args.int("fps", 60, min: 1, max: 240)
    let frames = try args.int("frames", 120, min: 0, max: Int.max)
    let bitrate = try args.int("bitrate", 6_000_000, min: 100_000, max: 200_000_000)
    let kfi = try args.double("keyframe-interval", 2, min: 0.1, max: 3600)
    let queue = try args.int("queue", 6, min: 1, max: 120)
    let lowLatencyRC = try rateControl(args)

    let writer = RecordWriter(policy: .encoded, maxMedia: queue)
    Lifecycle.shared.writer = writer
    writer.onWriteFailure = { Lifecycle.shared.shutdown(.ioError, reason: "stdout closed") }
    let encoder: H264Encoder
    do {
        encoder = try H264Encoder(EncoderConfig(width: width, height: height, fps: fps, bitrate: bitrate, keyframeIntervalSec: kfi, lowLatencyRateControl: lowLatencyRC), writer: writer)
    } catch HelperError.hardwareUnavailable(let m) {
        Lifecycle.shared.fail("hw_encoder_unavailable", m, exit: .hardwareUnavailable); dispatchMain()
    } catch HelperError.failed(let c, let m) {
        Lifecycle.shared.fail(c, m, exit: .internalError); dispatchMain()
    }
    encoder.emitEvents = args.flag("events")
    encoder.helloExtra = ["mode": "pattern", "frames": frames]
    writer.onNeedKeyframe = { [weak encoder] in encoder?.requestKeyframe(reason: "backpressure") }
    let source = try PatternSource(width: width, height: height, noise: args.flag("noise"))

    let stats = { () -> [String: Any] in ["encoder": encoder.statsJSON, "writer": writer.statsJSON] }
    let timer = startStatsTimer(5, stats)
    Lifecycle.shared.cleanup = { done in
        timer.cancel()
        encoder.finish()
        log("final-stats \(jsonString(stats()))")
        done()
    }
    startControlReader(encoder: encoder, stopOnEOF: false)

    let pace = !args.flag("no-pace")
    let t = Thread {
        var start = hostSeconds()
        var i = 0
        while frames == 0 || i < frames {
            if Lifecycle.shared.isShuttingDown { return }
            if pace {
                let deadline = start + Double(i) / Double(fps)
                let wait = deadline - hostSeconds()
                if wait > 0 { Thread.sleep(forTimeInterval: wait) }
            }
            guard let pb = source.frame(i) else {
                Lifecycle.shared.fail("pattern_frame_failed", "could not allocate pattern pixel buffer", exit: .internalError)
                return
            }
            if !pace {
                // still bounded: wait for the encoder instead of dropping when unpaced
                while encoder.inFlightCount >= encoder.config.maxInFlight { usleep(500) }
            }
            encoder.encode(pb, unixUs: unixMicros(), pts: hostTimeNow())
            if i == 0 {
                // Warm-up: the first IDR initialises the hardware encoder (tens of ms). Wait for it
                // before starting the pacing clock so paced frames are not dropped at startup.
                let limit = hostSeconds() + 1.0
                while encoder.inFlightCount > 0 && hostSeconds() < limit { usleep(500) }
                start = hostSeconds() - 1.0 / Double(fps)
            }
            i += 1
        }
        Lifecycle.shared.shutdown(.ok, reason: "pattern finished \(i) frames")
    }
    t.name = "pattern"
    t.qualityOfService = .userInteractive
    t.start()
    dispatchMain()
}

// MARK: - Capture

final class ScreenCapturer: NSObject, SCStreamOutput, SCStreamDelegate {
    let queue = DispatchQueue(label: "vantry.capture", qos: .userInteractive)
    private var stream: SCStream?
    private let encoder: H264Encoder
    private let fps: Int
    private var lastPixelBuffer: CVPixelBuffer?
    private var lastFrameHost: Double = 0
    let completeFrames = Counter()
    let idleFrames = Counter()

    init(encoder: H264Encoder, fps: Int) {
        self.encoder = encoder
        self.fps = fps
    }

    func start(display: SCDisplay, width: Int, height: Int, showCursor: Bool, completion: @escaping (Error?) -> Void) {
        let cfg = SCStreamConfiguration()
        cfg.width = width
        cfg.height = height
        cfg.minimumFrameInterval = CMTime(value: 1, timescale: CMTimeScale(fps))
        cfg.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange
        cfg.colorMatrix = CGDisplayStream.yCbCrMatrix_ITU_R_709_2
        cfg.colorSpaceName = CGColorSpace.sRGB
        cfg.queueDepth = 5
        cfg.showsCursor = showCursor
        cfg.capturesAudio = false
        let filter = SCContentFilter(display: display, excludingWindows: [])
        let stream = SCStream(filter: filter, configuration: cfg, delegate: self)
        do {
            try stream.addStreamOutput(self, type: .screen, sampleHandlerQueue: queue)
        } catch {
            completion(error); return
        }
        self.stream = stream
        stream.startCapture { completion($0) }
    }

    func stop(_ done: @escaping () -> Void) {
        guard let s = stream else { done(); return }
        s.stopCapture { err in
            if let err = err { log("stopCapture: \(err.localizedDescription)") }
            done()
        }
        stream = nil
    }

    func stream(_ stream: SCStream, didOutputSampleBuffer sampleBuffer: CMSampleBuffer, of type: SCStreamOutputType) {
        guard type == .screen, sampleBuffer.isValid else { return }
        guard let attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, createIfNecessary: false) as? [[SCStreamFrameInfo: Any]],
              let rawStatus = attachments.first?[.status] as? Int,
              let status = SCFrameStatus(rawValue: rawStatus) else { return }
        guard status == .complete, let pb = CMSampleBufferGetImageBuffer(sampleBuffer) else {
            _ = idleFrames.add(1)
            return
        }
        if completeFrames.add(1) == 1, ProcessInfo.processInfo.environment["VANTRY_NATIVE_DEBUG"] != nil {
            let att = CVBufferCopyAttachments(pb, .shouldPropagate) as? [String: Any] ?? [:]
            log("debug first frame \(CVPixelBufferGetWidth(pb))x\(CVPixelBufferGetHeight(pb)) fmt=\(CVPixelBufferGetPixelFormatType(pb)) attachments=\(att.filter { !$0.key.contains("ICC") })")
        }
        // Map host-clock PTS (capture time) to Unix epoch microseconds.
        let pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer)
        let now = hostSeconds()
        let ageUs = pts.isValid ? max(0, (now - pts.seconds) * 1_000_000) : 0
        let unixUs = unixMicros() &- UInt64(ageUs)
        lastPixelBuffer = pb
        lastFrameHost = now
        encoder.encode(pb, unixUs: unixUs, pts: pts.isValid ? pts : hostTimeNow())
    }

    /// ScreenCaptureKit delivers nothing while the screen is static. When a keyframe is
    /// requested (PLI / new viewer) re-encode the last frame so the peer gets an IDR now.
    func keyframeRequested() {
        queue.async { [self] in
            guard let pb = lastPixelBuffer, hostSeconds() - lastFrameHost > 2.0 / Double(fps) else { return }
            encoder.encode(pb, unixUs: unixMicros(), pts: hostTimeNow(), forceKeyframe: true)
        }
    }

    func stream(_ stream: SCStream, didStopWithError error: Error) {
        Lifecycle.shared.fail("capture_stopped", "SCStream stopped: \(error.localizedDescription)", exit: .captureFailed)
    }
}

func runCapture(_ args: Args) throws -> Never {
    let allowed: Set<String> = ["display", "width", "height", "fps", "bitrate", "keyframe-interval", "queue", "no-cursor", "allow-prompt", "events", "rate-control"]
    if let bad = args.unknownKeys(allowed: allowed).first { throw UsageError("unknown option --\(bad) for capture") }
    let displayArg = try args.int("display", 0, min: 0, max: Int(UInt32.max))
    let maxW = try args.int("width", 1920, min: 16, max: 8192)
    let maxH = try args.int("height", 1080, min: 16, max: 8192)
    let fps = try args.int("fps", 60, min: 1, max: 240)
    let bitrate = try args.int("bitrate", 6_000_000, min: 100_000, max: 200_000_000)
    let kfi = try args.double("keyframe-interval", 2, min: 0.1, max: 3600)
    let queue = try args.int("queue", 6, min: 1, max: 120)
    let lowLatencyRC = try rateControl(args)

    if !args.flag("allow-prompt") && !CGPreflightScreenCaptureAccess() {
        Lifecycle.shared.fail("screen_permission_denied",
                              "Screen Recording permission is not granted for the responsible app; grant it in System Settings > Privacy & Security > Screen Recording (helper did not prompt).",
                              exit: .permissionDenied)
        dispatchMain()
    }
    let displayID: CGDirectDisplayID = displayArg == 0 ? CGMainDisplayID() : CGDirectDisplayID(displayArg)
    guard let mode = CGDisplayCopyDisplayMode(displayID), mode.pixelWidth > 0 else {
        Lifecycle.shared.fail("display_not_found", "display \(displayArg) is not active", exit: .captureFailed)
        dispatchMain()
    }
    // Fit inside --width x --height keeping the source aspect ratio, never upscale, even dims.
    let pw = Double(mode.pixelWidth), ph = Double(mode.pixelHeight)
    let scale = min(Double(maxW) / pw, Double(maxH) / ph, 1.0)
    let width = max(16, Int((pw * scale).rounded()) & ~1)
    let height = max(16, Int((ph * scale).rounded()) & ~1)

    let writer = RecordWriter(policy: .encoded, maxMedia: queue)
    Lifecycle.shared.writer = writer
    writer.onWriteFailure = { Lifecycle.shared.shutdown(.ioError, reason: "stdout closed") }
    let encoder: H264Encoder
    do {
        encoder = try H264Encoder(EncoderConfig(width: width, height: height, fps: fps, bitrate: bitrate, keyframeIntervalSec: kfi, lowLatencyRateControl: lowLatencyRC), writer: writer)
    } catch HelperError.hardwareUnavailable(let m) {
        Lifecycle.shared.fail("hw_encoder_unavailable", m, exit: .hardwareUnavailable); dispatchMain()
    } catch HelperError.failed(let c, let m) {
        Lifecycle.shared.fail(c, m, exit: .internalError); dispatchMain()
    }
    encoder.emitEvents = args.flag("events")
    encoder.helloExtra = ["mode": "capture", "displayID": Int(displayID), "sourcePixelWidth": mode.pixelWidth, "sourcePixelHeight": mode.pixelHeight]
    let capturer = ScreenCapturer(encoder: encoder, fps: fps)
    writer.onNeedKeyframe = { [weak encoder, weak capturer] in
        encoder?.requestKeyframe(reason: "backpressure")
        capturer?.keyframeRequested()
    }
    let stats = { () -> [String: Any] in
        ["encoder": encoder.statsJSON, "writer": writer.statsJSON,
         "capture": ["completeFrames": capturer.completeFrames.current, "idleFrames": capturer.idleFrames.current]]
    }
    let timer = startStatsTimer(5, stats)
    Lifecycle.shared.cleanup = { done in
        timer.cancel()
        capturer.stop {
            capturer.queue.sync {} // drain in-flight sample handler
            encoder.finish()
            log("final-stats \(jsonString(stats()))")
            done()
        }
    }
    startControlReader(encoder: encoder, stopOnEOF: true, onKeyframe: { capturer.keyframeRequested() })

    log("capture: display \(displayID) \(mode.pixelWidth)x\(mode.pixelHeight) -> \(width)x\(height)@\(fps)")
    SCShareableContent.getExcludingDesktopWindows(false, onScreenWindowsOnly: true) { content, error in
        if let error = error {
            let ns = error as NSError
            let denied = ns.domain == SCStreamErrorDomain && ns.code == SCStreamError.userDeclined.rawValue
            Lifecycle.shared.fail(denied ? "screen_permission_denied" : "capture_unavailable",
                                  "SCShareableContent: \(error.localizedDescription) (\(ns.domain) \(ns.code))",
                                  exit: denied ? .permissionDenied : .captureFailed)
            return
        }
        guard let display = content?.displays.first(where: { $0.displayID == displayID }) else {
            Lifecycle.shared.fail("display_not_found", "ScreenCaptureKit has no display \(displayID)", exit: .captureFailed)
            return
        }
        capturer.start(display: display, width: width, height: height, showCursor: !args.flag("no-cursor")) { err in
            if let err = err {
                let ns = err as NSError
                let denied = ns.domain == SCStreamErrorDomain && ns.code == SCStreamError.userDeclined.rawValue
                Lifecycle.shared.fail(denied ? "screen_permission_denied" : "capture_start_failed",
                                      "SCStream.startCapture: \(err.localizedDescription) (\(ns.domain) \(ns.code))",
                                      exit: denied ? .permissionDenied : .captureFailed)
            } else {
                log("capture started")
            }
        }
    }
    dispatchMain()
}

// MARK: - Decoder

final class H264Decoder {
    private let lock = NSLock()
    private let writer: RecordWriter
    private var session: VTDecompressionSession?
    private var format: CMVideoFormatDescription?
    private var sps: Data?
    private var pps: Data?
    private var waitingForKey = true
    private var lastNeedKeyEvent = 0.0
    var emitEvents = false

    // stats
    let framesIn = Counter()
    let decoded = Counter()
    let skippedWaitingKey = Counter()
    let decodeErrors = Counter()
    let sessionsCreated = Counter()
    let decodeLatency = LatencyStat()
    let convertLatency = LatencyStat()
    let endToEndLatency = LatencyStat()
    private var lastSize = (0, 0)
    private(set) var hardware = false

    init(writer: RecordWriter) { self.writer = writer }

    private func needKeyframe(_ reason: String) {
        waitingForKey = true
        let now = hostSeconds()
        if emitEvents && now - lastNeedKeyEvent > 0.2 {
            lastNeedKeyEvent = now
            writer.enqueueControl(.event, jsonData(["event": "need_keyframe", "reason": reason]))
        }
    }

    private func teardownLocked() {
        if let s = session {
            VTDecompressionSessionWaitForAsynchronousFrames(s)
            VTDecompressionSessionInvalidate(s)
        }
        session = nil
    }

    private func rebuildLocked(sps newSPS: Data, pps newPPS: Data) -> Bool {
        var fmt: CMVideoFormatDescription?
        let st = newSPS.withUnsafeBytes { s -> OSStatus in
            newPPS.withUnsafeBytes { p -> OSStatus in
                let ptrs: [UnsafePointer<UInt8>] = [s.bindMemory(to: UInt8.self).baseAddress!, p.bindMemory(to: UInt8.self).baseAddress!]
                let sizes = [newSPS.count, newPPS.count]
                return CMVideoFormatDescriptionCreateFromH264ParameterSets(allocator: nil, parameterSetCount: 2, parameterSetPointers: ptrs, parameterSetSizes: sizes, nalUnitHeaderLength: 4, formatDescriptionOut: &fmt)
            }
        }
        guard st == noErr, let fmt = fmt else {
            log("CMVideoFormatDescriptionCreateFromH264ParameterSets failed: \(st)")
            return false
        }
        teardownLocked()
        let spec = [kVTVideoDecoderSpecification_RequireHardwareAcceleratedVideoDecoder: kCFBooleanTrue!] as CFDictionary
        let attrs = [
            kCVPixelBufferPixelFormatTypeKey: kCVPixelFormatType_32BGRA,
            kCVPixelBufferIOSurfacePropertiesKey: [:] as CFDictionary,
        ] as [CFString: Any] as CFDictionary
        var s: VTDecompressionSession?
        let cst = VTDecompressionSessionCreate(allocator: nil, formatDescription: fmt, decoderSpecification: spec, imageBufferAttributes: attrs, outputCallback: nil, decompressionSessionOut: &s)
        guard cst == noErr, let session = s else {
            Lifecycle.shared.fail("hw_decoder_unavailable", "VTDecompressionSessionCreate(H.264, RequireHardwareAcceleratedVideoDecoder) failed: OSStatus \(cst)", exit: .hardwareUnavailable)
            return false
        }
        guard copySessionBool(session, kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder).1 == true else {
            VTDecompressionSessionInvalidate(session)
            Lifecycle.shared.fail("hw_decoder_unavailable", "decoder session is not hardware accelerated", exit: .hardwareUnavailable)
            return false
        }
        hardware = true
        VTSessionSetProperty(session, key: kVTDecompressionPropertyKey_RealTime, value: kCFBooleanTrue)
        self.session = session
        format = fmt
        sps = newSPS
        pps = newPPS
        waitingForKey = true
        _ = sessionsCreated.add(1)
        let dims = CMVideoFormatDescriptionGetDimensions(fmt)
        log("decoder session #\(sessionsCreated.current): \(dims.width)x\(dims.height) profile-level-id=\(profileLevelId(fromSPS: newSPS) ?? "?") hardware=true")
        return true
    }

    /// payload = ts:u64be | keyFrame:u8 | Annex-B
    func handle(_ payload: Data) {
        lock.lock()
        defer { lock.unlock() }
        _ = framesIn.add(1)
        guard payload.count > 9 else { _ = decodeErrors.add(1); return }
        let ts = loadBE64(payload, 0)
        let body = payload.subdata(in: (payload.startIndex + 9)..<payload.endIndex)

        var newSPS: Data?
        var newPPS: Data?
        var hasIDR = false
        var avcc = Data()
        avcc.reserveCapacity(body.count + 16)
        body.withUnsafeBytes { raw in
            let p = raw.bindMemory(to: UInt8.self)
            for r in annexBNALRanges(raw) {
                let type = p[r.lowerBound] & 0x1f
                switch type {
                case 7: if newSPS == nil { newSPS = Data(raw[r]) }
                case 8: if newPPS == nil { newPPS = Data(raw[r]) }
                case 9, 10, 11, 12: break // AUD / end of seq / end of stream / filler
                default:
                    if type == 5 { hasIDR = true }
                    avcc.appendBE32(UInt32(r.count))
                    avcc.append(contentsOf: raw[r])
                }
            }
        }
        if let s = newSPS, let p = newPPS, s != sps || p != pps || session == nil {
            if !rebuildLocked(sps: s, pps: p) { needKeyframe("bad_parameter_sets"); return }
        }
        guard let session = session, let format = format, !avcc.isEmpty else {
            _ = skippedWaitingKey.add(1)
            needKeyframe("no_session")
            return
        }
        if waitingForKey {
            if !hasIDR { _ = skippedWaitingKey.add(1); needKeyframe("waiting_for_idr"); return }
            waitingForKey = false
        }

        var block: CMBlockBuffer?
        var st = CMBlockBufferCreateWithMemoryBlock(allocator: kCFAllocatorDefault, memoryBlock: nil, blockLength: avcc.count, blockAllocator: kCFAllocatorDefault, customBlockSource: nil, offsetToData: 0, dataLength: avcc.count, flags: kCMBlockBufferAssureMemoryNowFlag, blockBufferOut: &block)
        guard st == noErr, let block = block else { _ = decodeErrors.add(1); return }
        st = avcc.withUnsafeBytes { CMBlockBufferReplaceDataBytes(with: $0.baseAddress!, blockBuffer: block, offsetIntoDestination: 0, dataLength: avcc.count) }
        guard st == noErr else { _ = decodeErrors.add(1); return }
        var sample: CMSampleBuffer?
        var timing = CMSampleTimingInfo(duration: .invalid, presentationTimeStamp: CMTime(value: CMTimeValue(ts), timescale: 1_000_000), decodeTimeStamp: .invalid)
        var size = avcc.count
        st = CMSampleBufferCreateReady(allocator: kCFAllocatorDefault, dataBuffer: block, formatDescription: format, sampleCount: 1, sampleTimingEntryCount: 1, sampleTimingArray: &timing, sampleSizeEntryCount: 1, sampleSizeArray: &size, sampleBufferOut: &sample)
        guard st == noErr, let sample = sample else { _ = decodeErrors.add(1); return }

        let t0 = hostSeconds()
        // Synchronous decode (no async flag): the handler runs before DecodeFrame returns,
        // so there is no hidden decoder-side queue.
        let dst = VTDecompressionSessionDecodeFrame(session, sampleBuffer: sample, flags: [], infoFlagsOut: nil) { [self] status, flags, image, _, _ in
            if status != noErr || image == nil {
                if flags.contains(.frameDropped) { return }
                _ = decodeErrors.add(1)
                log("decode callback status \(status)")
                needKeyframe("decode_error")
                return
            }
            decodeLatency.add((hostSeconds() - t0) * 1000)
            emitRGBA(image!, ts: ts)
        }
        if dst != noErr {
            _ = decodeErrors.add(1)
            log("VTDecompressionSessionDecodeFrame failed: \(dst)")
            if dst == kVTInvalidSessionErr {
                teardownLocked()
                sps = nil; pps = nil
            }
            needKeyframe("decode_failed")
        }
    }

    private func emitRGBA(_ image: CVImageBuffer, ts: UInt64) {
        let t0 = hostSeconds()
        guard CVPixelBufferGetPixelFormatType(image) == kCVPixelFormatType_32BGRA else {
            Lifecycle.shared.fail("decoder_format", "decoder produced pixel format \(CVPixelBufferGetPixelFormatType(image)), expected BGRA", exit: .internalError)
            return
        }
        CVPixelBufferLockBaseAddress(image, .readOnly)
        defer { CVPixelBufferUnlockBaseAddress(image, .readOnly) }
        let w = CVPixelBufferGetWidth(image)
        let h = CVPixelBufferGetHeight(image)
        guard let base = CVPixelBufferGetBaseAddress(image), w > 0, h > 0 else { _ = decodeErrors.add(1); return }
        let rowBytes = CVPixelBufferGetBytesPerRow(image)
        let rgbaBytes = w * h * 4
        guard 16 + rgbaBytes <= kMaxPayload else {
            Lifecycle.shared.fail("frame_too_large", "\(w)x\(h) RGBA exceeds 64MiB record limit", exit: .protocolError)
            return
        }
        var payload = Data(count: 16 + rgbaBytes)
        let err: vImage_Error = payload.withUnsafeMutableBytes { raw in
            let p = raw.baseAddress!
            storeBE64(ts, p)
            storeBE32(UInt32(w), p + 8)
            storeBE32(UInt32(h), p + 12)
            var src = vImage_Buffer(data: base, height: vImagePixelCount(h), width: vImagePixelCount(w), rowBytes: rowBytes)
            var dst = vImage_Buffer(data: p + 16, height: vImagePixelCount(h), width: vImagePixelCount(w), rowBytes: w * 4)
            let map: [UInt8] = [2, 1, 0, 3] // BGRA -> RGBA
            return vImagePermuteChannels_ARGB8888(&src, &dst, map, vImage_Flags(kvImageNoFlags))
        }
        guard err == kvImageNoError else { _ = decodeErrors.add(1); log("vImagePermuteChannels failed \(err)"); return }
        if (w, h) != lastSize {
            lastSize = (w, h)
            log("decoder output size \(w)x\(h)")
        }
        convertLatency.add((hostSeconds() - t0) * 1000)
        endToEndLatency.add(Double(Int64(bitPattern: unixMicros() &- ts)) / 1000)
        _ = decoded.add(1)
        writer.enqueueRGBA(payload)
    }

    func finish() {
        lock.lock()
        teardownLocked()
        lock.unlock()
    }

    var statsJSON: [String: Any] {
        ["framesIn": framesIn.current, "decoded": decoded.current, "skippedWaitingKey": skippedWaitingKey.current,
         "decodeErrors": decodeErrors.current, "sessionsCreated": sessionsCreated.current, "hardware": hardware,
         "decodeLatency": decodeLatency.json, "bgraToRgbaLatency": convertLatency.json,
         "srcTimestampToRGBALatency": endToEndLatency.json]
    }
}

func runDecode(_ args: Args) throws -> Never {
    let allowed: Set<String> = ["queue", "events", "output-policy"]
    if let bad = args.unknownKeys(allowed: allowed).first { throw UsageError("unknown option --\(bad) for decode") }
    let queue = try args.int("queue", 2, min: 1, max: 30)
    let policy: RecordWriter.Policy
    switch args.string("output-policy") ?? "drop-oldest" {
    case "drop-oldest": policy = .dropOldest
    case "block": policy = .block
    default: throw UsageError("--output-policy must be drop-oldest or block")
    }
    let writer = RecordWriter(policy: policy, maxMedia: queue)
    Lifecycle.shared.writer = writer
    writer.onWriteFailure = { Lifecycle.shared.shutdown(.ioError, reason: "stdout closed") }
    guard VTIsHardwareDecodeSupported(kCMVideoCodecType_H264) else {
        Lifecycle.shared.fail("hw_decoder_unavailable", "VTIsHardwareDecodeSupported(H.264) is false", exit: .hardwareUnavailable)
        dispatchMain()
    }
    writer.enqueueControl(.hello, jsonData([
        "protocol": kProtocolVersion, "mode": "decode", "codec": "h264", "hardware": true,
        "output": "rgba", "helperVersion": kHelperVersion,
    ]))
    let decoder = H264Decoder(writer: writer)
    decoder.emitEvents = args.flag("events")
    let stats = { () -> [String: Any] in ["decoder": decoder.statsJSON, "writer": writer.statsJSON] }
    let timer = startStatsTimer(5, stats)
    Lifecycle.shared.cleanup = { done in
        timer.cancel()
        decoder.finish()
        log("final-stats \(jsonString(stats()))")
        done()
    }

    let t = Thread {
        var header = [UInt8](repeating: 0, count: 5)
        while !Lifecycle.shared.isShuttingDown {
            let r = header.withUnsafeMutableBytes { readExact(0, $0.baseAddress!, 5) }
            if r == .eof { Lifecycle.shared.shutdown(.ok, reason: "stdin EOF"); return }
            if r != .ok { Lifecycle.shared.fail("input_truncated", "stdin ended inside a record header", exit: .protocolError); return }
            let len = Int(header[1]) << 24 | Int(header[2]) << 16 | Int(header[3]) << 8 | Int(header[4])
            guard len <= kMaxPayload else {
                Lifecycle.shared.fail("payload_too_large", "record type \(header[0]) length \(len) exceeds 64MiB", exit: .protocolError)
                return
            }
            var payload = Data(count: len)
            if len > 0 {
                let pr = payload.withUnsafeMutableBytes { readExact(0, $0.baseAddress!, len) }
                if pr != .ok { Lifecycle.shared.fail("input_truncated", "stdin ended inside a \(len)-byte record", exit: .protocolError); return }
            }
            switch header[0] {
            case RecordType.h264.rawValue: decoder.handle(payload)
            case RecordType.hello.rawValue, RecordType.event.rawValue, RecordType.error.rawValue: break
            default: log("decode: ignoring record type \(header[0]) (\(len) bytes)")
            }
        }
    }
    t.name = "decode-input"
    t.qualityOfService = .userInteractive
    t.start()
    dispatchMain()
}

// MARK: - main

Lifecycle.shared.installSignalHandlers()
do {
    let args = try Args(CommandLine.arguments)
    switch args.mode {
    case "probe": runProbe()
    case "request-permission": try runPermissionRequest(args)
    case "capture": try runCapture(args)
    case "pattern": try runPattern(args)
    case "decode": try runDecode(args)
    case "-h", "--help", "help":
        FileHandle.standardError.write(Data((usageText + "\n").utf8)); exit(ExitCode.ok.rawValue)
    default: throw UsageError("unknown mode \(args.mode)")
    }
} catch let e as UsageError {
    log("usage error: \(e.message)\n\(usageText)")
    writeRecordSync(.error, errorPayload("usage", e.message))
    exit(ExitCode.usage.rawValue)
} catch HelperError.failed(let c, let m) {
    writeRecordSync(.error, errorPayload(c, m))
    exit(ExitCode.internalError.rawValue)
} catch {
    writeRecordSync(.error, errorPayload("internal", "\(error)"))
    exit(ExitCode.internalError.rawValue)
}
