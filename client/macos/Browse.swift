import AppKit
import SwiftUI

struct VersionTarget: Identifiable {
    let unit: String
    let version: String?
    let title: String
    var id: String { unit + (version ?? "") }
}
struct Member: Decodable, Identifiable {
    let path, kind: String
    let size: Int64?
    var id: String { path }
}
struct CacheMember: Decodable, Identifiable {
    var id: String { unitId + snapshotId + path }
    let size: Int64
    let unitId, snapshotId, path, state: String
    let localPath: String?
}
struct Browse: View {
    @ObservedObject var model: LibraryModel
    let target: VersionTarget
    @Environment(\.dismiss) private var dismiss
    @State private var folder = ""
    @State private var rows = [Member]()
    @State private var cached = Set<String>()
    @State private var busy = false
    @State private var notice = ""
    @State private var ready = false
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack {
                Image(systemName: target.version == nil ? "externaldrive" : "shippingbox")
                    .font(.title2).foregroundStyle(DeskColor.accent)
                    .frame(width: 40, height: 40).background(DeskColor.selection, in: RoundedRectangle(cornerRadius: 10))
                VStack(alignment: .leading, spacing: 3) {
                    Text(target.title).font(.title2.bold())
                    Text(target.version.map { "快照 " + String($0.suffix(10)) } ?? "NAS 当前目录")
                        .font(.caption).foregroundStyle(DeskColor.muted)
                }
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button("完成") { dismiss() }
            }
            HStack {
                Button { move(to: folder.split(separator: "/").dropLast().joined(separator: "/")) }
                    label: { Image(systemName: "chevron.left") }
                    .disabled(folder.isEmpty || busy)
                Text(folder.isEmpty ? "/" : "/" + folder).textSelection(.enabled)
                Spacer()
            }
            ForEach(model.transfers.filter { $0.unitId == target.unit && $0.direction == "download" }) { transfer in
                TransferDetails(transfer: transfer, project: target.title)
            }
            if ready && rows.isEmpty { ContentUnavailableView("空目录", systemImage: "folder") }
            else {
                List(rows) { row in
                    HStack {
                        Image(systemName: row.kind == "directory" ? "folder" : "doc")
                        Text(row.path.split(separator: "/").last.map(String.init) ?? row.path)
                        Spacer()
                        if let size = row.size {
                            Text(ByteCountFormatter.string(fromByteCount: size, countStyle: .file))
                                .foregroundStyle(.secondary)
                        }
                        if row.kind == "directory" {
                            Button("进入") { move(to: row.path) }
                        } else if row.kind == "file" && target.version == nil {
                            Button("下载到…") { download(row) }
                        } else if row.kind == "file" {
                            Button(cached.contains(row.path) ? "在 Finder 中显示" : "取回本机") {
                                act(row, release: false)
                            }
                            if cached.contains(row.path) {
                                Button("释放缓存") { act(row, release: true) }
                            }
                        }
                    }
                }.disabled(busy)
            }
            if !notice.isEmpty { Text(notice).font(.callout).textSelection(.enabled) }
        }.padding(22).frame(minWidth: 620, minHeight: 400).background(DeskColor.background)
        .onAppear { move(to: "") }
    }
    private func refreshCache() async throws {
        let entries = try await CLI.rows(["cache", "list"], CacheMember.self)
        cached = Set(entries.filter { $0.unitId == target.unit && $0.snapshotId == target.version && $0.localPath != nil }.map(\.path))
    }
    private func move(to path: String) {
        guard !busy else { return }; busy = true
        Task { @MainActor in
            defer { busy = false }
            do {
                var args = ["ls", target.unit, "--path", path]
                if let version = target.version { args += ["--snapshot", version] }
                let items = try await CLI.rows(args, Member.self)
                rows = items; folder = path; ready = true; notice = ""
                if target.version != nil { try await refreshCache() }
            } catch { notice = error.localizedDescription }
        }
    }
    private func download(_ member: Member) {
        guard !busy else { return }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = String(member.path.split(separator: "/").last ?? "下载文件")
        guard panel.runModal() == .OK, let destination = panel.url else { return }
        busy = true
        Task { @MainActor in
            defer { busy = false }
            do {
                _ = try await CLI.call(["get", target.unit, "--path", member.path, "--to", destination.path])
                NSWorkspace.shared.activateFileViewerSelecting([destination])
                notice = "已保存至 " + destination.path
            } catch { notice = error.localizedDescription }
        }
    }
    private func act(_ member: Member, release: Bool) {
        guard !busy, let version = target.version else { return }; busy = true
        Task { @MainActor in
            defer { busy = false }
            do {
                let data = try await CLI.call(["cache", release ? "evict" : "fetch", target.unit, version, "--path", member.path])
                let entry = try JSONDecoder().decode(CacheMember.self, from: data)
                if release { notice = "本机下载缓存已释放，此版本保留在梦枢天罗。" }
                else if let path = entry.localPath {
                    NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
                    notice = "已取回 " + member.path
                }
                if target.version != nil { try await refreshCache() }
            } catch { notice = error.localizedDescription }
        }
    }
}
