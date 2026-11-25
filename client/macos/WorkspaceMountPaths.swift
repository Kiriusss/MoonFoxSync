import Darwin
import Foundation

enum WorkspaceMountPaths {
    static func clientHome() throws -> URL {
        let fm = FileManager.default
        let url: URL
        if let custom = ProcessInfo.processInfo.environment["KDRIVE_HOME"], !custom.isEmpty {
            url = URL(fileURLWithPath: custom, isDirectory: true)
        } else {
            url = try fm.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: false)
                .appendingPathComponent("XinhuSync", isDirectory: true)
        }
        try fm.createDirectory(at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        guard let resolved = realpath(url.path, nil) else { throw POSIXError(.ENOENT) }
        defer { free(resolved) }
        return URL(fileURLWithPath: String(cString: resolved), isDirectory: true)
    }
    static let directory = "workspace-volume"
    static let receipt = "workspace-mount.json"
    struct Mounted: Codable {
        let volume: String
        let from: String
        let helperPID: Int32
    }
    static func volume(at point: String) -> (type: String, from: String)? {
        var mounts: UnsafeMutablePointer<statfs>?
        let count = getmntinfo(&mounts, MNT_NOWAIT)
        guard let mounts, count > 0 else { return nil }
        for i in 0..<Int(count) {
            var entry = mounts[i]
            let path = withUnsafeBytes(of: &entry.f_mntonname) { String(cString: $0.baseAddress!.assumingMemoryBound(to: CChar.self)) }
            if path != point { continue }
            let type = withUnsafeBytes(of: &entry.f_fstypename) { String(cString: $0.baseAddress!.assumingMemoryBound(to: CChar.self)) }
            let from = withUnsafeBytes(of: &entry.f_mntfromname) { String(cString: $0.baseAddress!.assumingMemoryBound(to: CChar.self)) }
            return (type, from)
        }
        return nil
    }
    static func owner(_ home: URL) -> Mounted? {
        let location = home.appendingPathComponent(receipt)
        guard let bytes = try? Data(contentsOf: location) else { return nil }
        return try? JSONDecoder().decode(Mounted.self, from: bytes)
    }
    static func online(_ home: URL) -> Bool {
        guard let saved = owner(home), saved.helperPID > 0, kill(saved.helperPID, 0) == 0 else { return false }
        let path = home.appendingPathComponent(directory).path
        guard path == saved.volume, let active = volume(at: path) else { return false }
        return active.type == "webdav" && active.from == saved.from
    }
}
