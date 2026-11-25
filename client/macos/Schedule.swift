import AppKit
import SwiftUI

@MainActor
final class Schedule: ObservableObject {
    @Published private(set) var active = false
    @Published private(set) var running = false
    @Published private(set) var error = ""
    @Published private(set) var lastChecked: Date?
    var didCheck: (() -> Void)?
    private var process: Process?
    private var timer: Timer?
    let defaults = UserDefaults.standard

    init() {
        active = defaults.bool(forKey: "scheduleEnabled")
    }

    func apply(enabled: Bool, url: String, interface: String, small: Bool) {
        stop()
        defaults.set(enabled, forKey: "scheduleEnabled")
        defaults.set(url, forKey: "homeURL")
        defaults.set(interface, forKey: "homeInterface")
        defaults.set(small, forKey: "awaySmall")
        active = enabled
        start()
    }

    func start() {
        guard active, timer == nil else { return }
        check()
        timer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.check() }
        }
    }

    func stop() {
        timer?.invalidate()
        timer = nil
        if let process, process.isRunning { process.terminate() }
    }

    func toggle() {
        apply(enabled: !active, url: defaults.string(forKey: "homeURL") ?? "",
              interface: defaults.string(forKey: "homeInterface") ?? "", small: defaults.bool(forKey: "awaySmall"))
    }

    func checkNow() { check(manual: true) }

    var homeRouteArguments: [String] {
        guard let url = defaults.string(forKey: "homeURL"), !url.isEmpty,
              let interface = defaults.string(forKey: "homeInterface"), !interface.isEmpty else { return [] }
        return ["--home-url", url, "--home-interface", interface]
    }

    private func check(manual: Bool = false) {
        guard (active || manual), process == nil else { return }
        guard let binary = Bundle.main.url(forResource: "kdrive-agent", withExtension: nil) else {
            error = "应用包缺少 kdrive-agent。"
            return
        }
        let child = Process()
        var args = ["--once"] + homeRouteArguments
        if defaults.bool(forKey: "awaySmall") { args += ["--away-small"] }
        child.executableURL = binary
        child.arguments = args
        child.standardOutput = FileHandle.nullDevice
        let pipe = Pipe()
        child.standardError = pipe
        child.standardInput = FileHandle.nullDevice
        // Drain the pipe on a separate task so a large number of errors cannot block the child.
        let report = Task.detached {
            var tail = Data()
            do {
                while let bytes = try pipe.fileHandleForReading.read(upToCount: 4096), !bytes.isEmpty {
                    tail.append(bytes)
                    tail = Data(tail.suffix(4096))
                }
            } catch { return error.localizedDescription }
            return String(decoding: tail, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        }
        child.terminationHandler = { [weak self] p in
            Task { @MainActor in
                let message = await report.value
                guard let self, self.process === p else { return }
                self.process = nil
                self.running = false
                if p.terminationReason == .exit {
                    self.lastChecked = Date()
                    self.error = p.terminationStatus == 0 ? "" : (message.isEmpty ? "同步检查失败（\(p.terminationStatus)）" : message)
                    self.didCheck?()
                }
            }
        }
        do {
            try child.run()
            try? pipe.fileHandleForWriting.close()
            error = ""
            process = child
            running = true
        } catch {
            try? pipe.fileHandleForWriting.close()
            self.error = error.localizedDescription
        }
    }
}

struct ScheduleSettings: View {
    @ObservedObject var scheduler: Schedule
    @Environment(\.dismiss) private var dismiss
    @State private var enabled: Bool
    @State private var small: Bool
    @State private var home: String
    @State private var interface: String

    init(_ s: Schedule) {
        scheduler = s
        _enabled = State(initialValue: s.active)
        _small = State(initialValue: s.defaults.bool(forKey: "awaySmall"))
        _home = State(initialValue: s.defaults.string(forKey: "homeURL") ?? "")
        _interface = State(initialValue: s.defaults.string(forKey: "homeInterface") ?? "")
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("自动同步").font(.title2.bold())
            Toggle("应用运行期间每分钟检查绑定来源", isOn: $enabled)
            Group {
                TextField("家庭 NAS 的 HTTPS 地址", text: $home)
                TextField("家庭物理网卡，例如 en0", text: $interface)
                Text("家庭地址与网卡均留空时，只按外出策略处理。").font(.caption)
                Toggle("外出时自动上传小任务", isOn: $small)
                Text("每个任务最多 10 MiB，24 小时累计最多 100 MiB。其余任务等回家或手动放行。").font(.caption)
            }.disabled(!enabled)
            HStack {
                Spacer()
                Button("取消") { dismiss() }
                Button("保存") {
                    scheduler.apply(enabled: enabled, url: home.trimmingCharacters(in: .whitespacesAndNewlines),
                                    interface: interface.trimmingCharacters(in: .whitespacesAndNewlines), small: small)
                    dismiss()
                }.disabled(enabled && (home.isEmpty != interface.isEmpty)).keyboardShortcut(.defaultAction)
            }
        }.textFieldStyle(.roundedBorder).padding(24).frame(width: 460)
    }
}
