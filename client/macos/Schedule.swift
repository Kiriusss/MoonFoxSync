import AppKit
import SwiftUI
import IOKit.ps
import notify

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
    @Published private(set) var power: SyncPower
    @Published private(set) var batteryPolicy: BatterySyncPolicy
    let defaults: UserDefaults
    private let readPower: () -> SyncPower
    private let clock: () -> Date
    private let agent: URL?
    private var started = false
    private var powerToken: Int32?
    private var wakeObserver: NSObjectProtocol?
    private var workIsManual = false
    private var retryWhenIdle = false

    init(defaults: UserDefaults = .standard, readPower: @escaping () -> SyncPower = SyncPower.current,
         clock: @escaping () -> Date = Date.init,
         agent: URL? = Bundle.main.url(forResource: "kdrive-agent", withExtension: nil)) {
        self.defaults = defaults
        self.readPower = readPower
        self.clock = clock
        self.agent = agent
        power = readPower()
        batteryPolicy = BatterySyncPolicy(rawValue: defaults.string(forKey: "batterySyncPolicy") ?? "") ?? .manual
        active = defaults.bool(forKey: "scheduleEnabled")
    }

    var status: String {
        guard active else { return "自动同步已暂停" }
        switch power {
        case .external: return "自动同步已开启"
        case .battery: return "电池 · " + batteryPolicy.label
        case .unknown: return "电源状态未知 · 仅手动同步"
        }
    }

    func apply(enabled: Bool, url: String, interface: String, small: Bool, battery: BatterySyncPolicy? = nil) {
        let selected = battery ?? batteryPolicy
        let batteryChanged = selected != batteryPolicy
        let resetBatteryDelay = batteryChanged || enabled != active
        let routeChanged = url != (defaults.string(forKey: "homeURL") ?? "")
            || interface != (defaults.string(forKey: "homeInterface") ?? "")
            || small != defaults.bool(forKey: "awaySmall")
        guard resetBatteryDelay || routeChanged else {
            if !started { start() }
            return
        }
        let replan = routeChanged || enabled != active || (batteryChanged && power != .external)
        if replan { cancelAutomatic() }
        defaults.set(enabled, forKey: "scheduleEnabled")
        defaults.set(url, forKey: "homeURL")
        defaults.set(interface, forKey: "homeInterface")
        defaults.set(small, forKey: "awaySmall")
        defaults.set(selected.rawValue, forKey: "batterySyncPolicy")
        active = enabled
        batteryPolicy = selected
        if resetBatteryDelay { setBatteryDeadline() }
        if !enabled {
            events.stop()
            bindings = nil
        }
        if !started { start() } else if replan { evaluate(retryIfBusy: true) }
    }

    private var batteryDeadline: Date? { defaults.object(forKey: "batterySyncDue") as? Date }

    private func setBatteryDeadline() {
        if active, power == .battery, let interval = batteryPolicy.interval {
            defaults.set(clock().addingTimeInterval(interval), forKey: "batterySyncDue")
        } else {
            defaults.removeObject(forKey: "batterySyncDue")
        }
    }

    func start() {
        guard !started else { return }
        started = true
        var token: Int32 = 0
        if notify_register_dispatch(kIOPSNotifyPowerSource, &token, .main, { [weak self] _ in
            MainActor.assumeIsolated { self?.powerNotification() }
        }) == NOTIFY_STATUS_OK { powerToken = token }
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(forName: NSWorkspace.didWakeNotification,
                object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.evaluate() }
        }
        evaluate()
    }

    func stop() {
        started = false
        timer?.invalidate()
        timer = nil
        if let token = powerToken { notify_cancel(token); powerToken = nil }
        if let wakeObserver { NSWorkspace.shared.notificationCenter.removeObserver(wakeObserver); self.wakeObserver = nil }
        generation += 1
        preparation = nil
        retryWhenIdle = false
        bindings = nil
        events.stop()
        if let process, process.isRunning { process.interrupt() }
    }

    private func cancelAutomatic() {
        retryWhenIdle = false
        guard !workIsManual, preparation != nil || process != nil else { return }
        generation += 1
        preparation = nil
        if let process, process.isRunning { process.interrupt() }
    }

    // Ignore duplicate IOKit notifications so they cannot accelerate the AC timer.
    private func powerNotification() {
        guard started, readPower() != power else { return }
        evaluate()
    }

    func evaluate(retryIfBusy: Bool = false) {
        guard started else { return }
        let next = readPower()
        let pluggedIn = next == .external && next != power
        if next != power {
            if next != .external { cancelAutomatic() }
            power = next
            setBatteryDeadline()
        }
        if power == .external, batteryDeadline != nil { setBatteryDeadline() }
        let now = clock()
        if active, power == .battery, let interval = batteryPolicy.interval,
           batteryDeadline.map({ $0.timeIntervalSince(now) > interval }) ?? true {
            setBatteryDeadline()
        }
        let due = power == .external || (power == .battery && batteryPolicy.interval != nil && batteryDeadline.map { $0 <= now } == true)
        if active && due {
            if process == nil && preparation == nil {
                if power == .battery { setBatteryDeadline() }
                check()
            } else if pluggedIn || retryIfBusy || power == .battery {
                retryWhenIdle = true
            }
        }
        timer?.invalidate()
        timer = nil
        guard active else { return }
        var delay: TimeInterval?
        if power == .external { delay = 60 }
        if power == .battery, let due = batteryDeadline, batteryPolicy.interval != nil {
            let remaining = due.timeIntervalSince(now)
            delay = remaining > 0 ? remaining : 60
        }
        if powerToken == nil || power == .unknown { delay = min(delay ?? 60, 60) }
        if let delay {
            let nextTimer = Timer.scheduledTimer(withTimeInterval: delay, repeats: false) { [weak self] _ in
                Task { @MainActor in self?.evaluate() }
            }
            nextTimer.tolerance = min(10, delay / 10)
            timer = nextTimer
        }
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
        workIsManual = manual
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
        guard current == generation else { return }
        if !manual && readPower() != power { evaluate(); return }
        guard manual || active && (power == .external || power == .battery && batteryPolicy.interval != nil) else { return }
        guard let binary = agent else {
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
                defer {
                    if self.retryWhenIdle {
                        self.retryWhenIdle = false
                        self.evaluate()
                    }
                }
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
    @State private var battery: BatterySyncPolicy
    @State private var home: String
    @State private var interface: String

    init(_ s: Schedule) {
        scheduler = s
        _enabled = State(initialValue: s.active)
        _battery = State(initialValue: s.batteryPolicy)
        _small = State(initialValue: s.defaults.bool(forKey: "awaySmall"))
        _home = State(initialValue: s.defaults.string(forKey: "homeURL") ?? "")
        _interface = State(initialValue: s.defaults.string(forKey: "homeInterface") ?? "")
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("自动同步").font(.title2.bold())
            Toggle("自动备份有变化的来源", isOn: $enabled)
            Text("接入电源时按来源设置检查，未变化的本机目录每天复查一次。").font(.caption)
            Picker("使用电池时", selection: $battery) {
                ForEach(BatterySyncPolicy.allCases, id: \.self) { Text($0.label).tag($0) }
            }
            Text("使用电池时仍可立即检查或上传待传输任务。").font(.caption)
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
                                    interface: interface.trimmingCharacters(in: .whitespacesAndNewlines), small: small, battery: battery)
                    dismiss()
                }.disabled(enabled && (home.isEmpty != interface.isEmpty)).keyboardShortcut(.defaultAction)
            }
        }.textFieldStyle(.roundedBorder).padding(24).frame(width: 460)
    }
}
