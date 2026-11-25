import AppKit
import FinderSync
import ServiceManagement

@MainActor
final class FinderDomain {
    static let shared = FinderDomain()
    private let agent = SMAppService.agent(plistName: "local.kirius.xinhu.workspace.plist")
    private var isolatedHelper: Process?
    private var mountTask: Task<Void, Error>?
    private var badgeGeneration: UInt64 = 0

    func ensureMounted() async throws {
        if let existing = mountTask { return try await existing.value }
        let task = Task { try await mount() }
        mountTask = task
        defer { mountTask = nil }
        try await task.value
    }

    private func mount() async throws {
        let home = try WorkspaceMountPaths.clientHome()
        if WorkspaceMountPaths.online(home) { return }
        if ProcessInfo.processInfo.environment["KDRIVE_HOME"]?.isEmpty == false {
            // An explicit isolated client home may have separate pairing credentials.
            // Keep it out of the login service for the default user profile.
            if isolatedHelper?.isRunning != true {
                let p = Process()
                p.executableURL = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/XinhuWorkspace")
                p.standardInput = FileHandle.nullDevice
                p.standardOutput = FileHandle.nullDevice
                p.standardError = FileHandle.nullDevice
                try p.run()
                isolatedHelper = p
            }
        } else {
            if agent.status == .notRegistered || agent.status == .notFound { try agent.register() }
            guard agent.status == .enabled else {
                if agent.status == .requiresApproval {
                    throw LocalError(message: "请在系统设置的「登录项与扩展」允许心狐同步在后台运行，再重试释放空间。")
                }
                throw LocalError(message: "工作区后台服务尚未启用，请确认应用已安装到「应用程序」。")
            }
        }
        for _ in 0..<100 {
            if WorkspaceMountPaths.online(home) { return }
            try await Task.sleep(nanoseconds: 150_000_000)
        }
        throw LocalError(message: "工作区只读卷尚未就绪。请核对 NAS 配对和后台运行权限，再重试。")
    }

    /// A Finder request is only an advisory path; the engine must verify its own
    /// registered online entry and anchor before changing anything on disk.
    static func hydrationPath(from url: URL) -> String? {
        guard let c = URLComponents(url: url, resolvingAgainstBaseURL: false),
              c.scheme == "xinhu-sync", c.host == "workspace", c.path == "/hydrate",
              c.user == nil, c.password == nil, c.port == nil, c.fragment == nil,
              let items = c.queryItems, items.count == 1, items[0].name == "path",
              let path = items[0].value, path.hasPrefix("/"), !path.contains("\0"),
              (path as NSString).standardizingPath == path else { return nil }
        return path
    }

    var badgesEnabled: Bool { FIFinderSyncController.isExtensionEnabled }
    func openBadgeSettings() { FIFinderSyncController.showExtensionManagementInterface() }

    func refreshBadges() async {
        struct Entries: Decodable { let items: [Entry] }
        struct Entry: Decodable { let sourcePath: String; let state: String }
        struct Badges: Encodable { let roots, online: [String] }
        badgeGeneration &+= 1
        let generation = badgeGeneration
        do {
            guard let group = Bundle.main.object(forInfoDictionaryKey: "MoonFoxAppGroup") as? String,
                  let destination = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: group) else { return }
            let entries = try JSONDecoder().decode(Entries.self, from: await CLI.call(["workspace","list"])).items
            let bindings = try await CLI.rows(["bindings"], LocalBinding.self)
            guard generation == badgeGeneration else { return }
            var roots = Set<String>()
            for binding in bindings {
                var directory = ObjCBool(false)
                let exists = FileManager.default.fileExists(atPath: binding.path, isDirectory: &directory)
                let root = exists && directory.boolValue ? binding.path : (binding.path as NSString).deletingLastPathComponent
                if !root.isEmpty && root != "/" { roots.insert(root) }
            }
            let online = entries.filter { $0.state == "online" || $0.state == "hydrating" }.map(\.sourcePath)
            let state = Badges(roots: roots.sorted(), online: online.sorted())
            let bytes = try JSONEncoder().encode(state), url = destination.appendingPathComponent("workspace-badges.json")
            if (try? Data(contentsOf: url)) == bytes { return }
            try bytes.write(to: url, options: .atomic)
        } catch { /* Overlay availability never blocks backup or file recall. */ }
    }
}
