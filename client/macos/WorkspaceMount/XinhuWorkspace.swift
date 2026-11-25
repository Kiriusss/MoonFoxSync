import Darwin
import Foundation
import MachO

// launchd owns this helper and its child. Closing the desktop window or quitting
// the UI does not tear down a volume that existing workspace links depend on.
@main
struct XinhuWorkspace {
    struct Endpoint: Decodable { let url: URL; let mountPoint: String }
    struct Failure: LocalizedError { let reason: String; var errorDescription: String? { reason } }

    static func runTool(_ executable: String, _ args: [String], timeout: TimeInterval = 15) throws -> Bool {
        let p = Process(), done = DispatchSemaphore(value: 0)
        p.executableURL = URL(fileURLWithPath: executable)
        p.arguments = args
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        p.terminationHandler = { _ in done.signal() }
        try p.run()
        if done.wait(timeout: .now() + timeout) == .timedOut {
            p.terminate()
            if done.wait(timeout: .now() + 2) == .timedOut { kill(p.processIdentifier, SIGKILL); p.waitUntilExit() }
            return false
        }
        return p.terminationStatus == 0
    }

    static func unmountOwned(_ home: URL) throws {
        let point = home.appendingPathComponent(WorkspaceMountPaths.directory).path
        guard let active = WorkspaceMountPaths.volume(at: point) else { return }
        guard let saved = WorkspaceMountPaths.owner(home), saved.volume == point,
              active.type == "webdav", active.from == saved.from else {
            throw Failure(reason: "挂载目录被其他卷占用。")
        }
        if try !runTool("/sbin/umount", [point], timeout: 5) {
            guard try runTool("/sbin/umount", ["-f", point]) else { throw Failure(reason: "旧的只读工作区卷仍在使用。") }
        }
    }

    static func ready(_ input: FileHandle) throws -> Endpoint {
        var line = Data()
        while line.count < 8192 {
            guard let data = try input.read(upToCount: 1), let byte = data.first else { break }
            if byte == 10 { return try JSONDecoder().decode(Endpoint.self, from: line) }
            line.append(byte)
        }
        throw Failure(reason: "本机文件服务未能启动。请检查配对状态。")
    }

    static func serve() throws {
        let fm = FileManager.default
        let home = try WorkspaceMountPaths.clientHome()
        try fm.createDirectory(at: home, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let lockFD = open(home.appendingPathComponent("workspace-helper.lock").path, O_RDWR | O_CREAT | O_NOFOLLOW, 0o600)
        guard lockFD >= 0 else { throw POSIXError(.EACCES) }
        defer { close(lockFD) }
        guard flock(lockFD, LOCK_EX | LOCK_NB) == 0 else { return }
        try unmountOwned(home)
        let point = home.appendingPathComponent(WorkspaceMountPaths.directory)
        if !fm.fileExists(atPath: point.path) {
            try fm.createDirectory(at: point, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        }
        let attrs = try fm.attributesOfItem(atPath: point.path)
        guard attrs[.type] as? FileAttributeType == .typeDirectory,
              (attrs[.ownerAccountID] as? NSNumber)?.uint32Value == getuid(),
              try fm.contentsOfDirectory(atPath: point.path).isEmpty else {
            throw Failure(reason: "工作区挂载路径须为本账户的空目录。")
        }
        // launchd may supply the service label or a relative path for argv[0].
        var capacity: UInt32 = 0
        _ = _NSGetExecutablePath(nil, &capacity)
        var buffer = [CChar](repeating: 0, count: Int(capacity))
        guard _NSGetExecutablePath(&buffer, &capacity) == 0 else { throw Failure(reason: "找不到工作区服务的可执行文件。") }
        let helper = URL(fileURLWithPath: String(cString: buffer)).resolvingSymlinksInPath()
        let cli = helper.deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("Resources/kdrive")
        let p = Process(), pipe = Pipe()
        p.executableURL = cli
        p.arguments = ["provider", "webdav"]
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = pipe
        p.standardError = FileHandle.nullDevice
        try p.run()
        defer { if p.isRunning { p.terminate() } }
        let endpoint = try ready(pipe.fileHandleForReading)
        guard endpoint.mountPoint == point.path, endpoint.url.scheme == "http",
              endpoint.url.host == "127.0.0.1", endpoint.url.port != nil,
              endpoint.url.password == nil, endpoint.url.user == nil, endpoint.url.query == nil,
              endpoint.url.fragment == nil else { throw Failure(reason: "本机文件服务返回了无效挂载地址。") }

        let record = WorkspaceMountPaths.Mounted(volume: point.path, from: endpoint.url.absoluteString, helperPID: getpid())
        let receipt = home.appendingPathComponent(WorkspaceMountPaths.receipt)
        try JSONEncoder().encode(record).write(to: receipt, options: .atomic)
        guard chmod(receipt.path, 0o600) == 0 else { throw POSIXError(.EACCES) }
        guard try runTool("/sbin/mount_webdav", ["-o","rdonly,nobrowse","-v","MoonFoxWorkspace",endpoint.url.absoluteString,point.path]) else {
            throw Failure(reason: "macOS 未能挂载工作区只读文件服务。")
        }
        defer { try? unmountOwned(home) }
        guard WorkspaceMountPaths.online(home) else { throw Failure(reason: "工作区只读卷未就绪。") }
        let sig = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .global())
        signal(SIGTERM, SIG_IGN)
        sig.setEventHandler { if p.isRunning { p.terminate() } }
        sig.resume()
        defer { sig.cancel() }
        p.waitUntilExit()
    }

    static func main() {
        do { try serve() } catch {
            // Keep errors local and never include the DAV bearer URL or NAS credentials.
            if let e = error as? Failure { fputs(e.reason + "\n", stderr) } else { fputs("心狐同步工作区服务未就绪（" + String((error as NSError).code) + "）。\n", stderr) }
            exit(1)
        }
    }
}
