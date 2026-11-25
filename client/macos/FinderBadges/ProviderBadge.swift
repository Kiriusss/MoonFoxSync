import AppKit
import FinderSync
import Darwin

@objc(MoonFoxFinderBadges)
final class MoonFoxFinderBadges: FIFinderSync {
    private let controller = FIFinderSyncController.default()
    private var snapshot = WorkspaceBadges()
    private var monitor: DispatchSourceFileSystemObject?
    private let file: URL?

    override init() {
        let info = Bundle.main.infoDictionary
        file = (info?["MoonFoxAppGroup"] as? String)
            .flatMap { FileManager.default.containerURL(forSecurityApplicationGroupIdentifier:$0) }?
            .appendingPathComponent("workspace-badges.json")
        super.init()
        register("moonfox.online", "仅 NAS", "icloud.and.arrow.down.fill", .systemBlue)
        register("moonfox.local", "已取回到此设备", "checkmark.circle.fill", .systemGreen)
        guard let file else { return }
        // Observe the directory. The host atomically replaces the JSON file.
        let fd = open(file.deletingLastPathComponent().path, O_EVTONLY | O_CLOEXEC)
        if fd >= 0 {
            let source=DispatchSource.makeFileSystemObjectSource(fileDescriptor:fd,eventMask:[.write,.rename,.delete],queue:.main)
            source.setEventHandler { [weak self] in self?.refresh() }
            source.setCancelHandler { close(fd) }
            source.resume()
            monitor=source
        }
        refresh()
    }
    deinit { monitor?.cancel() }

    private func register(_ id: String,_ label: String,_ symbol: String,_ color: NSColor) {
        guard let raw=NSImage(systemSymbolName:symbol,accessibilityDescription:label) else { return }
        let image=raw.withSymbolConfiguration(.init(paletteColors:[color])) ?? raw
        controller.setBadgeImage(image,label:label,forBadgeIdentifier:id)
    }
    private func refresh() {
        guard let file else { return }
        // A removed manifest (including on account reset) clears all badges.
        let next = (try? WorkspaceBadges(data:Data(contentsOf:file))) ?? WorkspaceBadges()
        guard next != snapshot else { return }
        let old=snapshot
        snapshot=next
        if old.roots != next.roots {
            controller.directoryURLs=Set(next.roots.map { URL(fileURLWithPath:$0) })
        }
        let changed = old.online.symmetricDifference(next.online).union(old.local.symmetricDifference(next.local))
        for path in changed {
            controller.setBadgeIdentifier(next.badge(for:path),for:URL(fileURLWithPath:path))
        }
    }
    override func requestBadgeIdentifier(for url: URL) {
        controller.setBadgeIdentifier(snapshot.badge(for:url.path),for:url)
    }
    override func menu(for menuKind: FIMenuKind) -> NSMenu? {
        guard menuKind == .contextualMenuForItems, let files=controller.selectedItemURLs(), files.count == 1,
              let selected=files.first, snapshot.online.contains(selected.path) else { return nil }
        let menu=NSMenu(title:"心狐同步")
        let action=menu.addItem(withTitle:"取回到此设备",action:#selector(hydrate(_:)),keyEquivalent:"")
        action.target=self
        action.representedObject=selected.path
        return menu
    }
    @objc private func hydrate(_ sender: NSMenuItem) {
        refresh()
        guard let path=sender.representedObject as? String, let url=snapshot.hydrationURL(for:path) else { return }
        NSWorkspace.shared.open(url)
    }
}
