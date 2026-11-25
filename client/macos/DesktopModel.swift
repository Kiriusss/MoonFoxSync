import AppKit
import SwiftUI

struct Library: Decodable, Identifiable, Hashable {
    let unitId, name, role, kind: String
    let head, scopeId: String?
    var id: String { unitId }
}
struct Revision: Decodable, Identifiable {
    let snapshotId: String
    let label, committedAt, outcome, mergeParentSnapshotId: String?
    var id: String { snapshotId }
}
struct Job: Decodable, Identifiable {
    let unitId, snapshotId, state: String
    let label, capturedAt: String?
    var pending: Bool { ["queued", "local_saved", "waiting", "pending"].contains(state) }
    var id: String { snapshotId }
}
private struct PendingSyncReport: Decodable {
    let total, uploaded, active, skipped, failed: Int
    let errors: [String]

    var summary: String {
        if total == 0 { return "队列中没有待上传的快照" }
        return [(uploaded, "已上传"), (active, "正在传输"), (skipped, "已跳过"), (failed, "失败")]
            .filter { $0.0 > 0 }
            .map { "\($0.1) \($0.0) 个" }
            .joined(separator: "，")
    }
}
struct Items<Element: Decodable>: Decodable { let items: [Element]? }
struct LocalError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
}

enum CLI {
    static func call(_ args: [String], input: String? = nil) async throws -> Data {
        try await Task.detached(priority: .userInitiated) {
            guard let tool = Bundle.main.url(forResource: "kdrive", withExtension: nil) else {
                throw LocalError(message: "应用包缺少 kdrive。")
            }
            let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false,
                                                    attributes: [.posixPermissions: 0o700])
            defer { try? FileManager.default.removeItem(at: dir) }
            let out = dir.appendingPathComponent("result")
            let err = dir.appendingPathComponent("error")
            for url in [out, err] { try Data().write(to: url) }
            let output = try FileHandle(forWritingTo: out)
            let errors = try FileHandle(forWritingTo: err)
            defer { try? output.close(); try? errors.close() }
            let p = Process()
            p.executableURL = tool
            p.arguments = args
            p.standardOutput = output
            p.standardError = errors
            let pipe = Pipe()
            p.standardInput = pipe
            try p.run()
            if let input { try pipe.fileHandleForWriting.write(contentsOf: Data((input + "\n").utf8)) }
            try pipe.fileHandleForWriting.close()
            p.waitUntilExit()
            guard p.terminationStatus == 0 else {
                throw LocalError(message: (try? String(contentsOf: err, encoding: .utf8))?
                    .trimmingCharacters(in: .whitespacesAndNewlines) ?? "操作失败")
            }
            return try Data(contentsOf: out)
        }.value
    }
    static func rows<T: Decodable>(_ args: [String], _: T.Type) async throws -> [T] {
        try JSONDecoder().decode(Items<T>.self, from: await call(args)).items ?? []
    }
}

struct LocalConnection: Decodable {
    var paired: Bool
    var serverUrl: String?
}
struct LocalBinding: Decodable {
    let unitId, path, profile: String
}

@MainActor
final class LibraryModel: ObservableObject {
    @Published var libraries = [Library]()
    @Published var revisions = [Revision]()
    @Published var jobs = [Job]()
    @Published var transfers = [LiveTransfer]()
    @Published var transferError = ""
    @Published var bindings = [LocalBinding]()
    @Published var cache = [CacheMember]()
    @Published var projects = [DesktopProject]()
    @Published var projectWarning = ""
    @Published var checkingProjects = false
    private var projectRefreshRequested = false
    private var gitRefreshRequested = false
    @Published var connection = LocalConnection(paired: false)
    @Published var reachable = false
    @Published var selected: String?
    @Published var busy = false
    @Published var message = ""
    @Published private(set) var syncingPending = false
    @Published private(set) var pendingSyncStatus = ""
    @Published private(set) var pendingSyncErrors: [String] = []
    private var queueRead = 0
    private(set) var loaded = false

    var library: Library? { libraries.first { $0.id == selected } }
    var pendingCount: Int { jobs.filter(\.pending).count }
    var connectionLabel: String {
        if !loaded { return "正在读取" }
        if !connection.paired { return "待配对" }
        return reachable ? "已连接" : "未连通"
    }

    func run(_ action: @escaping @MainActor () async throws -> Void) {
        guard !busy else { return }
        busy = true
        message = ""
        Task {
            defer { busy = false }
            do { try await action() } catch { message = error.localizedDescription }
        }
    }

    func syncPending(homeRouteArguments: [String]) {
        guard !syncingPending, pendingCount > 0 else { return }
        syncingPending = true
        pendingSyncStatus = ""
        pendingSyncErrors = []
        Task {
            var uploaded = false
            do {
                let data = try await CLI.call(["sync-pending", "--now"] + homeRouteArguments)
                let report = try JSONDecoder().decode(PendingSyncReport.self, from: data)
                pendingSyncStatus = report.summary
                pendingSyncErrors = report.errors
                uploaded = report.uploaded > 0
            } catch {
                pendingSyncStatus = "同步失败"
                pendingSyncErrors = [error.localizedDescription]
            }
            do { try await refreshJobs() }
            catch { pendingSyncErrors.append("读取队列失败：" + error.localizedDescription) }
            syncingPending = false
            refreshProjects()
            if uploaded, connection.paired { try? await refreshLibraries() }
        }
    }
    func refreshProjects(remoteGit: Bool = false) {
        if checkingProjects {
            projectRefreshRequested = true
            gitRefreshRequested = gitRefreshRequested || remoteGit
            return
        }
        checkingProjects = true
        Task {
            var fetch = remoteGit
            repeat {
                projectRefreshRequested = false
                gitRefreshRequested = false
                do {
                    let args = ["project", "status"] + (fetch ? ["--refresh-git"] : [])
                    let report = try JSONDecoder().decode(DesktopProjectReport.self, from: await CLI.call(args))
                    projects = report.items
                    projectWarning = report.warning ?? ""
                } catch { projectWarning = error.localizedDescription }
                fetch = gitRefreshRequested
            } while projectRefreshRequested
            checkingProjects = false
        }
    }
    func refreshJobs() async throws {
        queueRead += 1
        let read = queueRead
        let result = try await CLI.rows(["queue"], Job.self)
        if read == queueRead { jobs = result }
    }
    func refresh() async throws {
        defer { refreshProjects() }
        try await refreshJobs()
        bindings = try await CLI.rows(["bindings"], LocalBinding.self)
        try await refreshCache()
        connection = try JSONDecoder().decode(LocalConnection.self, from: await CLI.call(["status"]))
        loaded = true
        guard connection.paired else {
            reachable = false
            libraries = []; selected = nil; revisions = []
            return
        }
        try await refreshLibraries()
    }
    private func refreshLibraries() async throws {
        do {
            libraries = try await CLI.rows(["units"], Library.self)
            reachable = true
        } catch { reachable = false; throw error }
        if !libraries.contains(where: { $0.id == selected }) { selected = libraries.first?.id }
        try await loadHistory()
    }
    func refreshCache() async throws {
        cache = try await CLI.rows(["cache", "list"], CacheMember.self)
    }
    func loadHistory() async throws {
        guard let id = selected, library?.kind == "managed" else { revisions = []; return }
        let result = try await CLI.rows(["history", id], Revision.self)
        if selected == id { revisions = result }
    }
    func setLibrary(_ id: String) {
        guard !busy, selected != id else { return }
        selected = id; revisions = []
        run { try await self.loadHistory() }
    }
    func hasLocalRecords(_ id: String) -> Bool {
        bindings.contains { $0.unitId == id } || jobs.contains { $0.unitId == id } || cache.contains { $0.unitId == id }
    }
    func name(for unit: String) -> String {
        if let name = libraries.first(where: { $0.id == unit })?.name { return name }
        if let path = bindings.first(where: { $0.unitId == unit })?.path { return URL(fileURLWithPath: path).lastPathComponent }
        return String(unit.suffix(8))
    }
}

enum DesktopPage: String, CaseIterable {
    case overview = "同步概览", projects = "项目", libraries = "资料库", queue = "传输队列", cache = "下载缓存", settings = "设置"
    var symbol: String {
        switch self {
        case .overview: return "square.grid.2x2"
        case .projects: return "square.stack.3d.up"
        case .libraries: return "folder"
        case .queue: return "arrow.up.arrow.down"
        case .cache: return "arrow.down.to.line.compact"
        case .settings: return "slider.horizontal.3"
        }
    }
}
