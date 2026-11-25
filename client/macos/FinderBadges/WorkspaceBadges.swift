import Foundation

// Compare the paths as displayed by Finder. Resolving a symbolic link here
// would replace an archived workspace path with its private NAS mount path.
struct WorkspaceBadges: Equatable {
    var roots: Set<String> = []
    var online: Set<String> = []
    var local: Set<String> = []

    init() {}
    init(data: Data) throws {
        struct Snapshot: Decodable { let roots,online: [String]; let local: [String]? }
        let s = try JSONDecoder().decode(Snapshot.self, from: data)
        roots = Set(s.roots.compactMap(Self.clean))
        func registered(_ values: [String], roots: Set<String>) -> Set<String> {
            Set(values.compactMap(Self.clean).filter { path in
                roots.contains(path) || roots.contains { path.hasPrefix($0 + "/") }
            })
        }
        online = registered(s.online,roots:roots)
        local = registered(s.local ?? [],roots:roots).subtracting(online)
    }
    private static func clean(_ path: String) -> String? {
        guard path.hasPrefix("/"), !path.contains("\0") else { return nil }
        let parts = path.split(separator:"/")
        guard !parts.isEmpty, !parts.contains(".."), !parts.contains(".") else { return nil }
        return "/" + parts.joined(separator:"/")
    }
    func badge(for path: String) -> String {
        if online.contains(path) { return "moonfox.online" }
        if local.contains(path) { return "moonfox.local" }
        return ""
    }
    func hydrationURL(for path: String) -> URL? {
        guard online.contains(path) else { return nil }
        var url = URLComponents()
        url.scheme = "xinhu-sync"
        url.host = "workspace"
        url.path = "/hydrate"
        url.queryItems = [URLQueryItem(name:"path",value:path)]
        return url.url
    }
}
