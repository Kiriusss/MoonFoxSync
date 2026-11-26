import SwiftUI

enum LibraryRemoval: Identifiable {
    case forget(String, String), delete(String, String), reset

    var unit: String? {
        switch self { case let .forget(unit, _), let .delete(unit, _): return unit; case .reset: return nil }
    }
    var label: String {
        switch self { case let .forget(_, name), let .delete(_, name): return name; case .reset: return "这台 Mac" }
    }
    var verb: String {
        switch self { case .forget: return "forget"; case .delete: return "delete"; case .reset: return "reset" }
    }
    var title: String {
        switch self {
        case .forget: return "移除本机记录"
        case .delete: return "从 NAS 删除资料库"
        case .reset: return "清除本机同步数据"
        }
    }
    var id: String { verb + (unit ?? "") }
}

extension ReadingRoom {
    @ViewBuilder func deletionMenu(unit: String, name: String, local: Bool, nas: Bool) -> some View {
        if local || nas {
            Menu {
                if local {
                    if let binding = model.bindings.first(where: { $0.unitId == unit }),
                       binding.profile == "project" || binding.profile == "file" {
                        Button("删除本机文件…", role: .destructive) {
                            let isGit = model.projects.contains { $0.id == unit && $0.kind == "git" }
                            localFilesTarget = LocalFilesTarget(name: name, binding: binding, isGit: isGit)
                        }
                    }
                    Button("移除本机记录…", role: .destructive) { deletionTarget = .forget(unit, name) }
                }
                if nas {
                    Button("从 NAS 删除…", role: .destructive) { deletionTarget = .delete(unit, name) }.disabled(model.busy)
                }
            } label: { Image(systemName: "ellipsis.circle") }
            .fixedSize().menuStyle(.borderlessButton).help("资料库管理").accessibilityLabel("资料库管理 · " + name)
            .disabled(model.busy && !local)
        }
    }
}

private struct RemovalPlan: Decodable {
    let token: String
    let name, path: String?
    let units, snapshots, devices, bindings, queued, cached, nasOnly, holds, uploads: Int?
}

struct LibraryRemovalSheet: View {
    @ObservedObject var model: LibraryModel
    let target: LibraryRemoval
    @Environment(\.dismiss) private var dismiss
    @State private var plan: RemovalPlan?
    @State private var typedName = ""
    @State private var loading = true
    @State private var applying = false
    @State private var error = ""

    private var confirmed: Bool {
        guard let plan, !plan.token.isEmpty else { return false }
        if case .delete = target { return plan.name?.isEmpty == false && typedName == plan.name }
        return true
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 13) {
            HStack {
                Text(target.title).font(.title2.bold())
                Spacer()
                if loading || applying { ProgressView().controlSize(.small) }
            }
            Text(plan?.name.flatMap { $0.isEmpty ? nil : $0 } ?? target.label).font(.headline)
            if let plan {
                switch target {
                case .delete:
                    Text("将删除 NAS 上此资料库及其子项目的版本历史。所有获授权设备都会失去这些版本。")
                    LabeledContent("资料库与项目数", value: String(plan.units ?? 0))
                    LabeledContent("版本数", value: String(plan.snapshots ?? 0))
                    LabeledContent("受影响的设备数", value: String(plan.devices ?? 0))
                    if let holds = plan.holds, holds > 0 {
                        Text("将一并移除 \(holds) 项版本保留记录，相关文件可能仅存于 NAS。").foregroundStyle(.orange)
                    }
                    if let uploads = plan.uploads, uploads > 0 {
                        Text("将取消 \(uploads) 项进行中的上传。").foregroundStyle(.orange)
                    }
                    TextField("输入完整名称以确认", text: $typedName).textFieldStyle(.roundedBorder)
                case .forget:
                    Text("移除此项目的本机绑定、快照和待传记录。工作区原文件和 NAS 上的备份会保留。")
                    if let path = plan.path, !path.isEmpty { Text(path).font(.caption).textSelection(.enabled) }
                    counts(plan)
                case .reset:
                    Text("清除这台 Mac 的绑定、快照、缓存和配对凭据。工作区原文件和 NAS 备份会保留，再次使用需要重新配对。")
                    LabeledContent("本机绑定数", value: String(plan.bindings ?? 0))
                    counts(plan)
                }
            } else if loading { Text("正在计算影响范围…").foregroundStyle(.secondary) }
            if !error.isEmpty { Text(error).font(.callout).foregroundStyle(.secondary).textSelection(.enabled) }
            HStack {
                if !error.isEmpty { Button("重新预览") { Task { await load() } }.disabled(loading) }
                Spacer()
                Button("取消") { dismiss() }.keyboardShortcut(.cancelAction)
                Button(target.title, role: .destructive) { Task { await apply() } }.disabled(!confirmed || loading)
            }.disabled(applying)
        }.padding(22).frame(width:450)
        .interactiveDismissDisabled(applying)
        .task { await load() }
    }

    @ViewBuilder private func counts(_ plan: RemovalPlan) -> some View {
        LabeledContent("本机快照数", value: String(plan.snapshots ?? 0))
        LabeledContent("待上传数", value: String(plan.queued ?? 0))
        if let cached = plan.cached, cached > 0 {
            LabeledContent("移除缓存记录", value: "\(cached)（含仅 NAS \(plan.nasOnly ?? 0) 项）")
        }
    }
    private func args(preview: Bool) -> [String] {
        var args = ["library", target.verb + (preview ? "-plan" : "")]
        if let unit = target.unit { args.append(unit) }
        return args
    }
    @MainActor private func load() async {
        plan = nil; typedName = ""; loading = true; error = ""
        defer { loading = false }
        do {
            plan = try JSONDecoder().decode(RemovalPlan.self, from: await CLI.call(args(preview: true)))
        } catch { self.error = error.localizedDescription }
    }
    @MainActor private func apply() async {
        guard confirmed, let plan, !applying else { return }
        applying = true
        defer { applying = false }
        var command = args(preview: false) + ["--token", plan.token]
        if case .delete = target { command += ["--name", typedName] }
        do {
            _ = try await CLI.call(command)
            do {
                try await model.refresh()
                model.message = target.title + "已完成。"
            } catch { model.message = target.title + "已完成。刷新状态失败：" + error.localizedDescription }
            dismiss()
        } catch {
            let detail = error.localizedDescription
            let retryable = ["使用原确认码重试", "此项目正在使用", "此项目正在清理", "本机正在执行同步或清理", "旧版维护锁"].contains { detail.contains($0) }
            if !retryable { self.plan = nil }
            self.error = error.localizedDescription
        }
    }
}
