import CoreServices
import Darwin
import Foundation

struct LocalBinding: Decodable, Sendable {
    let unitId, path, profile: String
    let localFilesDeleted: Bool?
}

// A token is valid only while this process continuously observes the same root.
// Events arriving during capture must survive that capture's acknowledgement.
@MainActor
final class SourceEvents {
    private struct Source: Equatable, Sendable {
        let path: String
        let profile: String
        let watched: String
        let device: dev_t
        let inode: ino_t
    }

    private var sources: [String: Source] = [:]
    private var stream: FSEventStreamRef?
    private var revisions: [String: UInt64] = [:]
    private var serial: UInt64 = 0
    private var completed: [String: (token: String, time: Date)] = [:]
    private var needsRestart = false

    private var setup: UUID?

    nonisolated private static func resolve(_ bindings: [LocalBinding]) -> [String: Source] {
        var next: [String: Source] = [:]
        for b in bindings where b.localFilesDeleted != true {
            let url = URL(fileURLWithPath: b.path)
            let directory = b.profile == "file" ? url.deletingLastPathComponent() : url
            guard (try? directory.resourceValues(forKeys: [.volumeIsLocalKey]).volumeIsLocal) == true,
                  let resolved = realpath(directory.path, nil) else { continue }
            // FSEvents uses the kernel spelling, e.g. /private/var rather than
            // Foundation's shortened /var. Keep the leaf of a file binding literal.
            defer { free(resolved) }
            var info = stat()
            guard let watched = String(validatingUTF8: resolved), lstat(resolved, &info) == 0,
                  info.st_mode & S_IFMT == S_IFDIR else { continue }
            let path = b.profile == "file" ? (watched as NSString).appendingPathComponent(url.lastPathComponent) : watched
            next[b.unitId] = Source(path: path, profile: b.profile, watched: watched, device: info.st_dev, inode: info.st_ino)
        }
        return next
    }

    @discardableResult
    func configure(_ bindings: [LocalBinding]) async -> Bool {
        guard setup == nil else { return false }
        let ticket = UUID()
        setup = ticket
        defer { if setup == ticket { setup = nil } }
        let next = await Task.detached(priority: .utility) { Self.resolve(bindings) }.value
        guard setup == ticket else { return false }
        guard needsRestart || next != sources || (stream == nil && !next.isEmpty) else { return true }
        closeStream()
        sources = next
        guard !next.isEmpty else { return true }
        let roots = Array(Set(next.values.map(\.watched)))
        let candidate = await Task.detached(priority: .utility) { Self.subscribe(roots, owner: self) }.value
        guard setup == ticket else {
            if let candidate { Self.dispose(candidate.ref) }
            return false
        }
        guard let candidate else { return false }
        stream = candidate.ref
        for id in next.keys { invalidate(id) }
        return true
    }

    // The newly started stream is transferred once, from the setup worker to
    // the main actor. Its callbacks are always delivered on the main queue.
    private struct Subscription: @unchecked Sendable { let ref: FSEventStreamRef }

    nonisolated private static func subscribe(_ roots: [String], owner: SourceEvents) -> Subscription? {
        var context = FSEventStreamContext(version: 0, info: Unmanaged.passUnretained(owner).toOpaque(),
                                           retain: nil, release: nil, copyDescription: nil)
        // WatchRoot synchronously opens every ancestor, including unrelated
        // protected locations. Recheck the bound directory's identity each pass.
        let flags = kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer
        guard let created = FSEventStreamCreate(nil, { origin, info, count, paths, flags, _ in
            guard let info else { return }
            let watcher = Unmanaged<SourceEvents>.fromOpaque(info).takeUnretainedValue()
            MainActor.assumeIsolated {
                guard watcher.stream == origin else { return }
                let names = paths.assumingMemoryBound(to: UnsafePointer<CChar>.self)
                let lost = kFSEventStreamEventFlagUserDropped | kFSEventStreamEventFlagKernelDropped | kFSEventStreamEventFlagEventIdsWrapped
                    | kFSEventStreamEventFlagRootChanged | kFSEventStreamEventFlagMount | kFSEventStreamEventFlagUnmount
                var dirty = Set<String>()
                for i in 0..<count {
                    if flags[i] & UInt32(lost) != 0 {
                        watcher.needsRestart = true
                        watcher.completed.removeAll()
                        watcher.revisions.removeAll()
                        return
                    }
                    guard let path = String(validatingUTF8: names[i]) else {
                        dirty.formUnion(watcher.sources.keys)
                        continue
                    }
                    for (id, source) in watcher.sources where !dirty.contains(id) {
                        if SourceEvents.matches(path, root: source.path, profile: source.profile, flags: flags[i]) { dirty.insert(id) }
                    }
                }
                for id in dirty { watcher.invalidate(id) }
            }
        }, &context, roots as CFArray, FSEventStreamEventId(kFSEventStreamEventIdSinceNow), 0.5, UInt32(flags)) else { return nil }
        FSEventStreamSetDispatchQueue(created, .main)
        guard FSEventStreamStart(created) else {
            FSEventStreamInvalidate(created)
            FSEventStreamRelease(created)
            return nil
        }
        return Subscription(ref: created)
    }

    private func invalidate(_ id: String) {
        serial &+= 1
        revisions[id] = serial
        completed.removeValue(forKey: id)
    }

    static func matches(_ path: String, root: String, profile: String, flags: UInt32) -> Bool {
        // A rename or an event coalesced at an ancestor invalidates its descendants.
        if path == "/" || path == root || root.hasPrefix(path + "/") { return true }
        let prefix = root.hasSuffix("/") ? root : root + "/"
        guard profile != "file", path.hasPrefix(prefix) else { return false }
        let relative = path.dropFirst(prefix.count)
        if profile == "codex" {
            let first = relative.prefix { $0 != "/" }
            return ["history.jsonl", "session_index.jsonl", "sessions", "archived_sessions"].contains(String(first))
        }
        let structural = kFSEventStreamEventFlagItemCreated | kFSEventStreamEventFlagItemRemoved | kFSEventStreamEventFlagItemRenamed
        if flags & UInt32(kFSEventStreamEventFlagItemIsFile) != 0, flags & UInt32(structural) == 0,
           relative == ".DS_Store" || relative.hasSuffix("/.DS_Store") { return false }
        // Backup selection can depend on Git's index and ignore settings.
        // Object storage, reflogs, and fetch bookkeeping never enter a snapshot.
        return !relative.hasPrefix(".git/objects/") && !relative.hasPrefix(".git/logs/")
            && relative != ".git/FETCH_HEAD" && relative != ".git/index.lock"
    }

    func begin(manual: Bool) -> (versions: [String: UInt64], clean: [String: String]) {
        guard stream != nil, !needsRestart else { return ([:], [:]) }
        let now = Date()
        let recent = completed.filter { now.timeIntervalSince($0.value.time) >= 0 && now.timeIntervalSince($0.value.time) < 86400 }
        return (revisions, manual ? [:] : recent.mapValues(\.token))
    }

    func acknowledge(_ checked: [String: String], versions: [String: UInt64]) {
        guard stream != nil, !needsRestart else { return }
        for (id, token) in checked {
            if let version = versions[id], version == revisions[id] {
                completed[id] = (token, Date())
            }
        }
    }

    nonisolated private static func dispose(_ ref: FSEventStreamRef) {
        FSEventStreamStop(ref)
        FSEventStreamInvalidate(ref)
        FSEventStreamRelease(ref)
    }

    func stop() {
        setup = nil
        closeStream()
    }

    private func closeStream() {
        if let stream { Self.dispose(stream) }
        stream = nil
        sources.removeAll()
        revisions.removeAll()
        completed.removeAll()
        needsRestart = false
    }
}
