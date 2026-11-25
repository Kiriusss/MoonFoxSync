import AppKit
import SwiftUI

struct WorkspaceRequest: Identifiable {
    var id = UUID()
    var directory: String?
    var source: String?
}
private struct ArchivedMember: Codable, Identifiable {
    let unitId, snapshotId, path, sourcePath, sha256, state: String
    let size: Int64
    var id: String { sourcePath }
    var bytes: String { ByteCountFormatter.string(fromByteCount: size, countStyle: .file) }
}
private struct ReleasePreview: Decodable { let items: [ArchivedMember]; let skipped: [String] }
private struct ReleaseBatchResult: Decodable { let completed: Int; let errors: [String] }

struct WorkspaceSpace: View {
    @Environment(\.dismiss) private var dismiss
    let request: WorkspaceRequest
    @State private var online: [ArchivedMember] = []
    @State private var proposed: [ArchivedMember] = []
    @State private var skipped: [String] = []
    @State private var message = ""
    @State private var busy = false
    @State private var badges = false
    @State private var confirmRelease = false
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("原位置按需").font(.title2.bold())
                Spacer()
                Button("完成") { dismiss() }.disabled(busy)
            }
            Text("释放后仍从原路径打开，联网时从 NAS 读取。需要编辑或离线使用时，先取回到原位置。").foregroundStyle(.secondary)
            HStack {
                Button("选择文件或文件夹…") { choose() }.disabled(busy)
                Spacer()
                Button(badges ? "管理 Finder 徽标…" : "启用 Finder 状态徽标…") { FinderDomain.shared.openBadgeSettings() }
            }
            if !proposed.isEmpty {
                HStack {
                    Text("准备释放 · "+String(proposed.count)+" 个文件").font(.headline)
                    Spacer()
                    Text(ByteCountFormatter.string(fromByteCount: proposed.reduce(0) { $0 + $1.size }, countStyle: .file))
                }
                List(proposed) { member in
                    HStack { Text(member.sourcePath).lineLimit(1).truncationMode(.middle).help(member.sourcePath); Spacer(); Text(member.bytes) }
                }.frame(minHeight: 75,maxHeight: 145)
                HStack {
                    Button("取消选择") { proposed=[]; skipped=[] }
                    Spacer()
                    Button("释放所选文件的本机空间…") { confirmRelease=true }.buttonStyle(.borderedProminent)
                }.disabled(busy)
            }
            if !skipped.isEmpty {
                DisclosureGroup("跳过 "+String(skipped.count)+" 项") { ScrollView { Text(skipped.joined(separator:"\n")).textSelection(.enabled) }.frame(maxHeight: 85) }
            }
            Divider()
            Text("原位置文件").font(.headline)
            if online.isEmpty {
                Text("尚无按需文件").foregroundStyle(.secondary).padding(.vertical,8)
            } else {
                List(online) { member in
                    HStack {
                        Image(systemName:member.state == "online" ? "icloud" : "arrow.triangle.2.circlepath").foregroundStyle(DeskColor.accent)
                        VStack(alignment:.leading,spacing:2) {
                            Text((member.sourcePath as NSString).lastPathComponent)
                            Text(member.sourcePath).font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle).help(member.sourcePath)
                        }
                        Spacer()
                        Text(member.state == "online" ? member.bytes : "等待完成").font(.caption)
                        Button("取回原文件") { Task { await hydrate(member) } }.disabled(busy)
                    }
                }.frame(minHeight:95,maxHeight:180)
            }
            if busy { ProgressView().controlSize(.small) }
            if !message.isEmpty { Text(message).font(.callout).textSelection(.enabled) }
        }.padding(22).frame(minWidth:650,minHeight:355)
        .interactiveDismissDisabled(busy)
        .task {
            await reload()
            if let source=request.source, let item=online.first(where: { $0.sourcePath == source }) {
                let alert=NSAlert()
                alert.messageText="取回原文件？"
                alert.informativeText=item.sourcePath+"\n"+item.bytes
                alert.addButton(withTitle:"取回"); alert.addButton(withTitle:"取消")
                if alert.runModal() == .alertFirstButtonReturn { await hydrate(item) }
            }
        }
        .onReceive(NotificationCenter.default.publisher(for:NSApplication.didBecomeActiveNotification)) { _ in badges=FinderDomain.shared.badgesEnabled }
        .alert("释放所选文件的本机空间？",isPresented:$confirmRelease) {
            Button("取消",role:.cancel) {}
            Button("释放空间") { Task { await release() } }
        } message: { Text("每个文件会在原位置换成可联网读取的引用。NAS 保留对应快照。正在使用或内容不匹配的文件会跳过。") }
    }
    @MainActor private func reload() async {
        do {
            online = try await CLI.rows(["workspace","list"],ArchivedMember.self)
            badges=FinderDomain.shared.badgesEnabled
            await FinderDomain.shared.refreshBadges()
        } catch { message=error.localizedDescription }
    }
    @MainActor private func choose() {
        let panel=NSOpenPanel()
        panel.canChooseDirectories=true;panel.canChooseFiles=true;panel.allowsMultipleSelection=true
        panel.resolvesAliases=false;panel.prompt="预览释放范围"
        if let path=request.directory { panel.directoryURL=URL(fileURLWithPath:path) }
        guard panel.runModal() == .OK else { return }
        let urls=panel.urls
        Task {
            busy=true;message="";proposed=[];skipped=[];defer{busy=false}
            var unique=[String:ArchivedMember]()
            for url in urls {
                do {
                    let p=try JSONDecoder().decode(ReleasePreview.self,from:await CLI.call(["workspace","plan","--source",url.path]))
                    for item in p.items {
                        if item.state == "local" {
                            unique[item.id] = item
                        } else {
                            let reason = item.state == "online" ? "已在原位置按需读取" : "有待完成的操作，请在原位置文件列表中取回"
                            skipped.append(item.sourcePath + " · " + reason)
                        }
                    }
                    skipped+=p.skipped
                } catch { skipped.append(url.lastPathComponent+" · "+error.localizedDescription) }
            }
            proposed=unique.values.sorted{$0.sourcePath<$1.sourcePath}
            if proposed.isEmpty { message = skipped.isEmpty ? "所选位置没有可释放的新文件。" : "没有可释放的新文件。展开跳过项可查看原因。" }
        }
    }
    @MainActor private func release() async {
        guard !busy else {return}
        busy=true;defer{busy=false};message="正在核对 NAS 版本并逐个释放…"
        do {
            try await FinderDomain.shared.ensureMounted()
            let body=String(decoding:try JSONEncoder().encode(proposed),as:UTF8.self)
            let result=try JSONDecoder().decode(ReleaseBatchResult.self,from:await CLI.call(["workspace","release-batch"],input:body))
            proposed=[];skipped+=result.errors;message="已在原位置释放 "+String(result.completed)+" 个文件。"
        } catch {message=error.localizedDescription}
        await reload()
    }
    @MainActor private func hydrate(_ member:ArchivedMember) async {
        guard !busy else {return}
        busy=true;defer{busy=false};message="正在取回 "+member.path
        do { _=try await CLI.call(["workspace","hydrate","--source",member.sourcePath]);message="已取回，可直接编辑原位置的文件。" }catch{message=error.localizedDescription}
        await reload()
    }
}
