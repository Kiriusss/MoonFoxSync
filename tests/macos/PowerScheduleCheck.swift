// swiftc -parse-as-library client/macos/{SyncPower,Schedule,SourceEvents}.swift \\
//   tests/macos/PowerScheduleCheck.swift -o /tmp/moonfox-power && /tmp/moonfox-power
import Foundation

// These tests exercise the real Process and policy timer, with isolated defaults,
// scripted power/time and a local child standing in for the network agent.
enum CLI {
    static func rows<T: Decodable>(_ args: [String], _ type: T.Type) async throws -> [T] { [] }
}

@main
struct PowerScheduleCheck {
    static let fs = FileManager.default

    static func child() async throws {
        let dir = URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent()
        let pid = String(ProcessInfo.processInfo.processIdentifier)
        let done = dir.appendingPathComponent("finish-" + pid)
        signal(SIGINT, SIG_IGN)
        let interrupt = DispatchSource.makeSignalSource(signal: SIGINT, queue: .global())
        interrupt.setEventHandler {
            try? Data().write(to: dir.appendingPathComponent("cancelled-" + pid))
            // A cooperative child may return success after receiving cancellation.
            Thread.sleep(forTimeInterval: 0.15)
            print(#"{"bindings":[],"checked":{},"changed":true}"#)
            exit(0)
        }
        interrupt.resume()
        _ = try FileHandle.standardInput.readToEnd()
        try Data().write(to: dir.appendingPathComponent("started-" + pid))
        while !fs.fileExists(atPath: done.path) { try await Task.sleep(for: .milliseconds(20)) }
        print(#"{"bindings":[],"checked":{},"changed":true}"#)
        interrupt.cancel()
    }

    @MainActor static func wait(_ reason: String, until condition: () -> Bool) async throws {
        for _ in 0..<200 {
            if condition() { return }
            try await Task.sleep(for: .milliseconds(30))
        }
        precondition(condition(), reason)
    }

    @MainActor static func main() async throws {
        if CommandLine.arguments.contains("--watch-pass") { try await child(); return }
        print("IOKit power source = " + SyncPower.current().rawValue)
        let suite = "test.moonfox.power." + UUID().uuidString
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let dir = fs.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try fs.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? fs.removeItem(at: dir) }
        let agent = dir.appendingPathComponent("fake-agent")
        try fs.copyItem(at: URL(fileURLWithPath: CommandLine.arguments[0]), to: agent)
        var source = SyncPower.battery
        var now = Date()
        func newSchedule() -> Schedule { Schedule(defaults: defaults, readPower: { source }, clock: { now }, agent: agent) }
        func ids(_ prefix: String) -> Set<String> {
            let files = (try? fs.contentsOfDirectory(atPath: dir.path)) ?? []
            return Set(files.filter { $0.hasPrefix(prefix) }.map { String($0.dropFirst(prefix.count)) })
        }
        func finish(_ pid: String) throws { try Data().write(to: dir.appendingPathComponent("finish-" + pid)) }
        var scheduled = newSchedule()
        defer { scheduled.stop() }
        var completions = 0
        scheduled.didCheck = { completions += 1 }
        scheduled.apply(enabled: true, url: "", interface: "", small: true)
        precondition(scheduled.batteryPolicy == .manual)
        for _ in 0..<4 { now.addTimeInterval(3600); scheduled.evaluate() }
        precondition(!scheduled.running && ids("started-").isEmpty)
        scheduled.checkNow()
        try await wait("manual did not start") { ids("started-").count == 1 }
        let manual = ids("started-").first!
        source = .external; scheduled.evaluate()
        source = .battery; scheduled.evaluate()
        try finish(manual)
        try await wait("power change interrupted manual check") { !scheduled.running && completions == 1 }
        precondition(ids("cancelled-").isEmpty)

        source = .external; scheduled.evaluate()
        try await wait("replug did not start eligible processing") { ids("started-").count == 2 }
        let automatic = ids("started-").subtracting([manual]).first!
        source = .battery; scheduled.evaluate()
        try await wait("unplug did not cancel own automatic check") { ids("cancelled-").contains(automatic) }
        source = .external; scheduled.evaluate() // the stale child is still exiting
        try await wait("rapid replug was lost") { ids("started-").count == 3 }
        precondition(completions == 1, "stale report reached UI or event watcher")
        let replacement = ids("started-").subtracting([manual,automatic]).first!
        try finish(replacement)
        try await wait("AC check completion") { completions == 2 }

        source = .battery; scheduled.evaluate()
        scheduled.apply(enabled: true, url: "", interface: "", small: true, battery: .hourly)
        let original = now
        now.addTimeInterval(3599); scheduled.evaluate()
        precondition(!scheduled.running && ids("started-").count == 3)
        scheduled.stop()
        scheduled = newSchedule()
        scheduled.start() // restarting with 1 second remaining must not reset or accelerate the schedule
        scheduled.evaluate()
        precondition(!scheduled.running)
        now = original.addingTimeInterval(3600); scheduled.evaluate()
        try await wait("hourly check was not due") { ids("started-").count == 4 }
        let hourly = ids("started-").subtracting([manual,automatic,replacement]).first!
        try finish(hourly)
        try await wait("hourly completion") { !scheduled.running }
        scheduled.evaluate()
        precondition(ids("started-").count == 4)

        scheduled.apply(enabled: true, url: "", interface: "", small: true, battery: .fourHourly)
        now.addTimeInterval(14399); scheduled.evaluate()
        precondition(!scheduled.running && ids("started-").count == 4)
        now.addTimeInterval(1); scheduled.evaluate()
        try await wait("four-hourly check was not due") { ids("started-").count == 5 }
        source = .unknown; scheduled.evaluate()
        try await wait("unknown power must suspend automatic work") { !scheduled.running }
        now.addTimeInterval(86400); scheduled.evaluate()
        precondition(!scheduled.running && ids("started-").count == 5)
        print("Passed battery default, manual operation, cancellation, rapid plug-in, throttling and persisted deadline checks")
    }
}
