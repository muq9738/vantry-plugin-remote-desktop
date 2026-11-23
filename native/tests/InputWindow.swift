import AppKit
import ApplicationServices

func emit(_ object: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys]) else { return }
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data([10]))
}

final class InputWindow: NSObject, NSApplicationDelegate, NSTextFieldDelegate {
    var window: NSWindow!
    var field: NSTextField!
    var clock: NSTextField!
    var timer: Timer?
    var frame: UInt64 = 0
    var clicks = 0
    let previous = NSWorkspace.shared.frontmostApplication
    func applicationDidFinishLaunching(_ notification: Notification) {
        let size = NSSize(width: 640, height: 380)
        window = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = "Vantry — controlled input acceptance"
        window.center()
        let view = window.contentView!
        clock = NSTextField(labelWithString: "")
        clock.font = .monospacedDigitSystemFont(ofSize: 26, weight: .medium)
        clock.frame = NSRect(x: 24, y: 270, width: 590, height: 60)
        view.addSubview(clock)
        let description = NSTextField(wrappingLabelWithString: "Temporary acceptance window. Remote clicks and test text are logged; this window closes automatically.")
        description.frame = NSRect(x: 24, y: 205, width: 590, height: 50)
        view.addSubview(description)
        field = NSTextField(frame: NSRect(x: 24, y: 70, width: 590, height: 36))
        field.placeholderString = "VANTRY_INPUT_OK"
        field.delegate = self
        view.addSubview(field)
        let button = NSButton(title: "Focus test input", target: self, action: #selector(clicked))
        button.bezelStyle = .rounded
        button.frame = NSRect(x: 210, y: 125, width: 220, height: 52)
        view.addSubview(button)
        window.makeKeyAndOrderFront(nil)
        NSApplication.shared.activate(ignoringOtherApps: true)
        let local = button.convert(NSPoint(x: button.bounds.midX, y: button.bounds.midY), to: nil)
        let global = window.convertPoint(toScreen: local)
        let screen = CGDisplayBounds(CGMainDisplayID())
        emit(["type": "ready", "pid": ProcessInfo.processInfo.processIdentifier,
              "capturePermission": CGPreflightScreenCaptureAccess(), "inputPermission": CGPreflightPostEventAccess(),
              "buttonX": global.x / max(1, screen.width - 1),
              "buttonY": (NSScreen.screens[0].frame.height - global.y) / max(1, screen.height - 1)])
        timer = Timer.scheduledTimer(withTimeInterval: 1.0/30.0, repeats: true) { [weak self] _ in
            guard let self else { return }
            self.frame += 1
            self.clock.stringValue = String(format: "%.3f  ·  %llu", Date().timeIntervalSince1970, self.frame)
            self.clock.textColor = NSColor(calibratedHue: CGFloat(self.frame % 180)/180, saturation: 0.75, brightness: 0.8, alpha: 1)
        }
        Timer.scheduledTimer(withTimeInterval: 60, repeats: false) { [weak self] _ in self?.finish() }
    }
    @objc func clicked() {
        clicks += 1
        window.makeFirstResponder(field)
        emit(["type": "click", "count": clicks])
    }
    func controlTextDidChange(_ notification: Notification) {
        emit(["type": "text", "value": field.stringValue])
    }
    func finish() {
        timer?.invalidate()
        emit(["type": "closed", "clicks": clicks, "text": field.stringValue])
        if NSWorkspace.shared.frontmostApplication?.processIdentifier == ProcessInfo.processInfo.processIdentifier {
            previous?.activate(options: [.activateIgnoringOtherApps])
        }
        NSApplication.shared.terminate(nil)
    }
}
let application = NSApplication.shared
let delegate = InputWindow()
application.delegate = delegate
application.setActivationPolicy(.regular)
DispatchQueue.global().async {
    _ = FileHandle.standardInput.readDataToEndOfFile()
    DispatchQueue.main.async { delegate.finish() }
}
application.run()
