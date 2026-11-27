import AppKit
import SwiftUI

private struct ScheduleReport: Decodable {
    let bindings: [LocalBinding]?
    let checked: [String: String]
    let changed: Bool
}

@MainActor
final class Schedule: ObservableObject {
    @Published private(set) var active = false
    @Published private(set) var running = false
    @Published private(set) var error = ""
    @Published private(set) var lastChecked: Date?
    var didCheck: (() -> Void)?
    private var process: Process?
    private var timer: Timer?
    private let events = SourceEvents()
    private var bindings: [LocalBinding]?
    private var preparation: UUID?
    private var generation = 0
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
        generation += 1
        preparation = nil
        bindings = nil
        events.stop()
        if let process, process.isRunning { process.interrupt() }
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
        guard (active || manual), process == nil, preparation == nil else { return }
        let ticket = UUID()
        preparation = ticket
        let current = generation
        // An offline mount or a pending system permission can stall observer
        // setup. Keep the UI and regular scheduled checking available.
        let fallback = Task {
            try? await Task.sleep(for: .seconds(2))
            guard !Task.isCancelled, preparation == ticket else { return }
            preparation = nil
            launch(manual: manual, generation: current, observed: false)
        }
        Task {
            defer { fallback.cancel() }
            if bindings == nil {
                let list = try? await CLI.rows(["bindings"], LocalBinding.self)
                guard current == generation else { return }
                bindings = list
            }
            let observed = await events.configure(bindings ?? [])
            guard preparation == ticket else { return }
            preparation = nil
            launch(manual: manual, generation: current, observed: observed)
        }
    }

    private func launch(manual: Bool, generation current: Int, observed: Bool) {
        guard let binary = Bundle.main.url(forResource: "kdrive-agent", withExtension: nil) else {
            error = "应用包缺少 kdrive-agent。"
            return
        }
        let child = Process()
        var args = ["--once", "--watch-pass"] + homeRouteArguments
        if defaults.bool(forKey: "awaySmall") { args += ["--away-small"] }
        child.executableURL = binary
        child.arguments = args
        child.qualityOfService = manual ? .userInitiated : .utility
        let output = Pipe()
        child.standardOutput = output
        let pipe = Pipe()
        child.standardError = pipe
        let input = Pipe()
        child.standardInput = input
        let pass = observed ? events.begin(manual: manual) : (versions: [:], clean: [:])
        let summary = Task.detached {
            let data = try? output.fileHandleForReading.readToEnd()
            return data.flatMap { try? JSONDecoder().decode(ScheduleReport.self, from: $0) }
        }
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
                let result = await summary.value
                guard let self, self.process === p else { return }
                self.process = nil
                self.running = false
                guard self.generation == current, p.terminationReason == .exit else { return }
                if let result {
                    self.events.acknowledge(result.checked, versions: pass.versions)
                    self.bindings = result.bindings ?? []
                    if result.changed { self.didCheck?() }
                }
                self.lastChecked = Date()
                if p.terminationStatus == 0 {
                    self.error = result == nil ? "未收到本轮检查结果，下次将重试。" : ""
                } else {
                    self.error = message.isEmpty ? "同步检查失败（\(p.terminationStatus)）" : message
                }
            }
        }
        do {
            let request = try JSONEncoder().encode(pass.clean)
            try child.run()
            try? output.fileHandleForWriting.close()
            try? pipe.fileHandleForWriting.close()
            try? input.fileHandleForReading.close()
            Task.detached {
                defer { try? input.fileHandleForWriting.close() }
                try? input.fileHandleForWriting.write(contentsOf: request)
            }
            error = ""
            process = child
            running = true
        } catch {
            try? output.fileHandleForWriting.close()
            try? input.fileHandleForWriting.close()
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
            Toggle("自动备份有变化的来源", isOn: $enabled)
            Text("本机目录按文件变化检查，每天复查一次。").font(.caption)
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
