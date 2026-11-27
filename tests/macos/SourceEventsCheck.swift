// Run on macOS with swiftc -parse-as-library client/macos/SourceEvents.swift
// tests/macos/SourceEventsCheck.swift -o /tmp/moonfox-events, then run that binary.
import Foundation

@main
struct SourceEventsCheck {
    @MainActor
    static func waitFor(_ condition: () -> Bool) async throws {
        for _ in 0..<100 {
            if condition() { return }
            try await Task.sleep(for: .milliseconds(100))
        }
        precondition(condition(), "Expected filesystem event was not delivered")
    }

    @MainActor
    static func main() async throws {
        let fs = FileManager.default
        let root = fs.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let project = root.appendingPathComponent("work")
        let codex = root.appendingPathComponent("codex")
        let sessions = codex.appendingPathComponent("sessions")
        let single = root.appendingPathComponent("single.txt")
        try fs.createDirectory(at: project, withIntermediateDirectories: true)
        try fs.createDirectory(at: sessions, withIntermediateDirectories: true)
        try Data().write(to: single)
        defer { try? fs.removeItem(at: root) }

        let watcher = SourceEvents()
        let roots = [LocalBinding(unitId: "p", path: project.path, profile: "project", localFilesDeleted: nil),
                     LocalBinding(unitId: "c", path: codex.path, profile: "codex", localFilesDeleted: nil),
                     LocalBinding(unitId: "f", path: single.path, profile: "file", localFilesDeleted: nil)]
        watcher.configure(roots)
        defer { watcher.stop() }
        try await Task.sleep(for: .seconds(1))
        let baseline = ["p": "project-token", "c": "codex-token", "f": "file-token"]
        let start = watcher.begin(manual: false)
        watcher.acknowledge(baseline, versions: start.versions)
        try Data([1]).write(to: codex.appendingPathComponent("logs.sqlite"))
        try await Task.sleep(for: .seconds(1))
        precondition(watcher.begin(manual: false).clean == baseline)
        precondition(watcher.begin(manual: true).clean.isEmpty)

        try Data([2]).write(to: project.appendingPathComponent("source.bin"))
        try await waitFor { watcher.begin(manual: false).clean["p"] == nil }
        precondition(watcher.begin(manual: false).clean.count == 2)
        watcher.acknowledge(["p": "stale"], versions: start.versions)
        precondition(watcher.begin(manual: false).clean["p"] == nil)

        try Data([3]).write(to: sessions.appendingPathComponent("rollout-test.jsonl"))
        try await waitFor { watcher.begin(manual: false).clean["c"] == nil }
        precondition(watcher.begin(manual: false).clean == ["f": "file-token"])
        try Data([4]).write(to: single)
        try await waitFor { watcher.begin(manual: false).clean.isEmpty }
        watcher.stop()
        watcher.configure(roots)
        watcher.acknowledge(baseline, versions: start.versions)
        precondition(watcher.begin(manual: false).clean.isEmpty)
        print("Passed native filesystem change and scan acknowledgement checks")
    }
}
