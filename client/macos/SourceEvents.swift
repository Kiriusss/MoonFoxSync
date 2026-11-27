import CoreServices
import Darwin
import Foundation

struct LocalBinding: Decodable {
    let unitId, path, profile: String
    let localFilesDeleted: Bool?
}

// A token is valid only while this process continuously observes the same root.
// Events arriving during capture must survive that capture's acknowledgement.
@MainActor
final class SourceEvents {
    private struct Source: Equatable {
        let path: String
        let profile: String
        let watched: String
    }

    private var sources: [String: Source] = [:]
    private var stream: FSEventStreamRef?
    private var revisions: [String: UInt64] = [:]
    private var serial: UInt64 = 0
    private var completed: [String: (token: String, time: Date)] = [:]
    private var needsRestart = false

    func configure(_ bindings: [LocalBinding]) {
        var next: [String: Source] = [:]
        for b in bindings where b.localFilesDeleted != true {
            let url = URL(fileURLWithPath: b.path)
            let directory = b.profile == "file" ? url.deletingLastPathComponent() : url
            guard (try? directory.resourceValues(forKeys: [.volumeIsLocalKey]).volumeIsLocal) == true,
                  let resolved = realpath(directory.path, nil) else { continue }
            // FSEvents uses the kernel spelling, e.g. /private/var rather than
            // Foundation's shortened /var. Keep the leaf of a file binding literal.
            defer { free(resolved) }
            guard let watched = String(validatingUTF8: resolved) else { continue }
            let path = b.profile == "file" ? (watched as NSString).appendingPathComponent(url.lastPathComponent) : watched
            next[b.unitId] = Source(path: path, profile: b.profile, watched: watched)
        }
        guard needsRestart || next != sources || (stream == nil && !next.isEmpty) else { return }
        stop()
        sources = next
        guard !next.isEmpty else { return }
        var context = FSEventStreamContext(version: 0, info: Unmanaged.passUnretained(self).toOpaque(),
                                           retain: nil, release: nil, copyDescription: nil)
        let roots = Array(Set(next.values.map(\.watched)))
        let flags = kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagWatchRoot | kFSEventStreamCreateFlagNoDefer
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
        }, &context, roots as CFArray, FSEventStreamEventId(kFSEventStreamEventIdSinceNow), 0.5, UInt32(flags)) else { return }
        FSEventStreamSetDispatchQueue(created, .main)
        stream = created
        guard FSEventStreamStart(created) else {
            FSEventStreamInvalidate(created)
            FSEventStreamRelease(created)
            stream = nil
            return
        }
        for id in next.keys { invalidate(id) }
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

    func stop() {
        if let stream {
            FSEventStreamStop(stream)
            FSEventStreamInvalidate(stream)
            FSEventStreamRelease(stream)
        }
        stream = nil
        sources.removeAll()
        revisions.removeAll()
        completed.removeAll()
        needsRestart = false
    }
}
