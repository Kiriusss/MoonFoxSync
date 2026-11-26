import SwiftUI

struct LocalFilesTarget: Identifiable {
    let name: String
    let binding: LocalBinding
    let isGit: Bool
    var id: String { binding.unitId }
}

private struct LocalFileDeletionResult: Decodable {
    let removed, skipped: Int
    let details: [String]?
    let paused: Bool
}

struct LocalFileDeletionSheet: View {
    @ObservedObject var model: LibraryModel
    let target: LocalFilesTarget
    @Environment(\.dismiss) private var dismiss
    @State private var applying = false
    @State private var result: LocalFileDeletionResult?
    @State private var failure = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(result == nil ? "删除本机文件？" : "本机文件处理结果").font(.title2.bold())
            VStack(alignment: .leading, spacing: 4) {
                Text(target.name).font(.headline)
                Text(target.binding.path).font(.callout).foregroundStyle(.secondary)
                    .textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
            }
            if let result {
                Text("已删除 \(result.removed) 个本机文件，跳过 \(result.skipped) 项。")
                if let details = result.details, !details.isEmpty {
                    DisclosureGroup("查看详情") {
                        ScrollView {
                            Text(details.joined(separator: "\n"))
                                .font(.caption).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                        }.frame(maxHeight: 125)
                    }
                }
            } else {
                Text("删除与 NAS 备份一致的本机文件，并暂停此项目的本机同步。未备份或已修改的文件会留在原处，NAS 上的文件和历史版本会保留。")
                if target.isGit {
                    Text("已备份的 Git 工作树文件也会删除，.git 目录会保留。").foregroundStyle(.secondary)
                }
                if applying {
                    HStack {
                        ProgressView().controlSize(.small)
                        Text("正在核对 NAS 备份并处理本机文件…")
                    }
                }
            }
            if result?.paused == true || (!failure.isEmpty && model.bindings.contains(where: { $0.unitId == target.id && $0.localFilesDeleted == true })) {
                Text("此项目的本机同步已暂停。需要取回文件时，在版本历史中选择时间点，点「恢复到…」。")
                    .foregroundStyle(.secondary)
            }
            if !failure.isEmpty {
                Text(failure).font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
            }
            HStack {
                Spacer()
                if result != nil {
                    Button("完成") { dismiss() }.keyboardShortcut(.defaultAction)
                } else {
                    Button("取消") { dismiss() }.keyboardShortcut(.cancelAction)
                    Button("删除本机文件", role: .destructive) { Task { await remove() } }
                }
            }.disabled(applying)
        }
        .padding(22).frame(width: 465)
        .interactiveDismissDisabled(applying)
    }

    @MainActor private func remove() async {
        guard !applying, result == nil else { return }
        applying = true
        failure = ""
        defer { applying = false }
        do {
            let binding = target.binding
            let response = try await CLI.call(["local", "delete-files", binding.unitId, "--source", binding.path])
            result = try JSONDecoder().decode(LocalFileDeletionResult.self, from: response)
        } catch {
            failure = error.localizedDescription
        }
        if let bindings = try? await CLI.rows(["bindings"], LocalBinding.self) {
            model.bindings = bindings
        }
        model.refreshProjects()
        await FinderDomain.shared.refreshBadges()
    }
}
