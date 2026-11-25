import SwiftUI

struct TimelineAction: Identifiable {
    enum Operation: String {
        case restore = "恢复为当前版本", delete = "删除并重建", policy = "历史保存策略"
    }
    let id = UUID()
    let library: Library
    let operation: Operation
    var revision: Revision?
}

private struct RetentionRule: Decodable {
    let mode: String
    let value: Int
}
private struct RetentionPreview: Decodable {
    let mode: String
    let value, removeCount, protectedCount: Int
}
private struct RestoreReceipt: Decodable { let snapshotId: String }
private struct DeleteReceipt: Decodable { let deleted: Bool; let snapshotId: String; let rebuiltPoints: [RebuiltPoint]? }
private struct RebuiltPoint: Decodable, Identifiable {
    let snapshotId: String
    let label: String?
    let parentsBefore, parentsAfter: [String]?
    var id: String { snapshotId }
}
private struct DeletePreview: Decodable {
    let snapshotId, expectedHead: String
    let planToken, protectedReason: String?
    let rebuiltPoints: [RebuiltPoint]?
    var changes: [RebuiltPoint] { rebuiltPoints ?? [] }
}

struct TimelineControls: View {
    let action: TimelineAction
    @Environment(\.dismiss) private var dismiss
    @State private var head: String?
    @State private var request = UUID().uuidString.lowercased()
    @State private var previousRestore: [String]?
    @State private var mode = "all"
    @State private var amount = "30"
    @State private var label = ""
    @State private var preview: RetentionPreview?
    @State private var deletePreview: DeletePreview?
    @State private var showingConfirmation = false
    @State private var ready = false
    @State private var busy = false
    @State private var error = ""
    @State private var result = ""

    private var title: String { action.operation.rawValue }
    private var isPolicy: Bool { action.operation == .policy }
    private var target: String { action.revision?.snapshotId ?? "" }
    private var value: Int? {
        if mode == "all" { return 0 }
        let limit = mode == "days" ? 3650 : 10000
        guard let number = Int(amount.trimmingCharacters(in:.whitespacesAndNewlines)), (1...limit).contains(number) else { return nil }
        return number
    }
    private var ruleText: String {
        switch mode {
        case "latest": return "保留最近 " + amount + " 个时间点"
        case "days": return "保留最近 " + amount + " 天的时间点"
        default: return "保留全部时间点"
        }
    }
    private var details: String {
        switch action.operation {
        case .restore: return "NAS 将以所选版本的内容发布一个新主线版本。电脑上的工作目录保持原样。"
        case .delete: return "NAS 将删除所选时间点，再为依赖它的版本重建父版本关系。这些版本的文件内容保持原样。"
        case .policy: return "策略会将范围外的时间点从 NAS 版本历史中隐藏。当前主线、未合并分叉及受保护副本会保留。"
        }
    }
    var body: some View {
        VStack(alignment:.leading,spacing:14) {
            HStack {
                VStack(alignment:.leading,spacing:3) {
                    Text(title).font(.title2.bold())
                    Text(action.library.name).font(.caption).foregroundStyle(DeskColor.muted)
                }
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button("完成") { dismiss() }.disabled(busy)
            }
            if !result.isEmpty {
                Label(result,systemImage:"checkmark.circle.fill").foregroundStyle(.green).textSelection(.enabled)
            } else {
                if let revision = action.revision {
                    version("操作时间点",revision.snapshotId,revision.label)
                }
                version("当前 NAS 主线",head,nil)
                if isPolicy {
                    Picker("保留范围",selection:$mode) {
                        Text("保留全部").tag("all")
                        Text("按最近个数").tag("latest")
                        Text("按最近天数").tag("days")
                    }.pickerStyle(.segmented).disabled(!ready || busy)
                    if mode != "all" {
                        HStack {
                            TextField("数量",text:$amount).textFieldStyle(.roundedBorder).frame(width:80)
                            Text(mode == "days" ? "天" : "个时间点")
                            Text(mode == "days" ? "1–3650" : "1–10000").font(.caption).foregroundStyle(DeskColor.muted)
                        }.disabled(!ready || busy)
                    }
                }
                if action.operation == .restore {
                    TextField("新版本说明",text:$label).textFieldStyle(.roundedBorder).disabled(busy || !ready)
                }
                Text(details).font(.callout).foregroundStyle(DeskColor.muted).fixedSize(horizontal:false,vertical:true)
                if let deletion = deletePreview {
                    if let reason = deletion.protectedReason, !reason.isEmpty {
                        Label("此时间点受保护 · " + reason,systemImage:"lock").font(.callout).foregroundStyle(.orange)
                    } else if deletion.changes.isEmpty {
                        Text("其他时间点已有独立基线，无需重建。").font(.callout)
                    } else {
                        Text("将重建 " + String(deletion.changes.count) + " 个时间点").font(.headline)
                        ScrollView {
                            LazyVStack(alignment:.leading,spacing:9) {
                                ForEach(deletion.changes) { point in
                                    VStack(alignment:.leading,spacing:3) {
                                        Text((point.label.flatMap { $0.isEmpty ? nil : $0 } ?? "时间点") + " · " + short(point.id)).font(.callout)
                                        Text("原父版本  " + parentNames(point.parentsBefore))
                                        Text("新父版本  " + parentNames(point.parentsAfter))
                                    }.font(.caption).textSelection(.enabled)
                                }
                            }.frame(maxWidth:.infinity,alignment:.leading)
                        }.frame(maxHeight:142)
                    }
                }
                if !error.isEmpty {
                    Text(error).font(.callout).foregroundStyle(.red).textSelection(.enabled)
                    if previousRestore != nil { Button("重试已确认的恢复请求") { Task { await retryRestore() } }.disabled(busy) }
                    Button("重新读取 NAS 状态") { Task { await reload() } }.disabled(busy)
                }
                Spacer(minLength:4)
                HStack {
                    Spacer()
                    Button(isPolicy ? "预览保存策略…" : "核对" + title + "…") {
                        if isPolicy { Task { await previewPolicy() } }
                        else { showingConfirmation = true }
                    }.buttonStyle(DeskAction(primary:true)).disabled(!ready || busy || (isPolicy && value == nil) ||
                        (!isPolicy && head == nil) || (action.operation == .restore && head == target) ||
                        (action.operation == .delete && deletePreview?.planToken?.isEmpty != false) ||
                        (deletePreview?.protectedReason?.isEmpty == false))
                }
            }
        }.padding(20).frame(width:530).frame(minHeight:355)
        .interactiveDismissDisabled(busy)
        .task { await reload() }
        .onChange(of:label) { _,_ in request = UUID().uuidString.lowercased() }
        .alert(isPolicy ? "应用历史保存策略？" : title + "？",isPresented:$showingConfirmation) {
            Button("取消",role:.cancel) { }
            if action.operation == .delete {
                Button("确认删除并重建",role:.destructive) { Task { await submit() } }
            } else if action.operation == .delete {
                Text("将删除 " + short(target) + "，重建 " + String(deletePreview?.changes.count ?? 0) + " 个时间点。当前 NAS 主线 " + short(head) + "。")
            } else {
                Button(isPolicy ? "确认应用策略" : "确认恢复为当前版本") { Task { await submit() } }
            }
        } message: {
            if isPolicy, let preview {
                Text(ruleText + "。NAS 预计隐藏 " + String(preview.removeCount) + " 个时间点，额外保护 " + String(preview.protectedCount) + " 个。")
            } else {
                Text((action.revision?.label ?? "") + " · " + short(target) + "\n当前 NAS 主线 " + short(head) + "\n" + details)
            }
        }
    }
    private func short(_ id:String?) -> String { id.flatMap { $0.isEmpty ? nil : String($0.suffix(12)) } ?? "暂无版本" }
    private func parentNames(_ ids:[String]?) -> String {
        guard let ids, !ids.isEmpty else { return "无父版本 · 独立基线" }
        return ids.map { short($0) }.joined(separator:"、")
    }
    private func version(_ name:String,_ id:String?,_ caption:String?) -> some View {
        HStack(alignment:.firstTextBaseline) {
            Text(name).foregroundStyle(DeskColor.muted)
            Spacer()
            if let caption, !caption.isEmpty { Text(caption).lineLimit(1) }
            Text(short(id)).font(.caption.monospaced()).textSelection(.enabled).help(id ?? "")
        }.font(.callout)
    }
    private func currentLibrary() async throws -> Library {
        let libraries = try await CLI.rows(["units"],Library.self)
        guard let library = libraries.first(where: { $0.id == action.library.id && $0.kind == "managed" && $0.role == "sync" }) else {
            throw LocalError(message:"此设备需要该资料库的同步权限。")
        }
        return library
    }
    @MainActor private func reload() async {
        guard !busy else { return }
        busy = true
        ready = false
        preview = nil
        deletePreview = nil
        defer { busy = false }
        do {
            let library = try await currentLibrary()
            if library.head != head { request = UUID().uuidString.lowercased() }
            head = library.head
            previousRestore = nil
            if isPolicy {
                let raw = try await CLI.call(["timeline","policy",library.id])
                let rule = try JSONDecoder().decode(RetentionRule.self,from:raw)
                mode = rule.mode
                amount = String(rule.value > 0 ? rule.value : 30)
            } else if action.operation == .delete, let head {
                let raw = try await CLI.call(["timeline","preview-delete",library.id,"--snapshot",target,"--head",head])
                let plan = try JSONDecoder().decode(DeletePreview.self,from:raw)
                guard plan.snapshotId == target && plan.expectedHead == head else { throw LocalError(message:"删除计划与所选时间点不同，请重新读取。") }
                deletePreview = plan
            } else if label.isEmpty {
                label = "恢复 " + (action.revision?.label.flatMap { $0.isEmpty ? nil : $0 } ?? short(target))
            }
            error = action.operation == .restore && head != nil && head == target ? "所选时间点已是当前主线。" : ""
            ready = true
        } catch { self.error = error.localizedDescription }
    }
    @MainActor private func restore(_ args:[String]) async throws {
        let raw = try await CLI.call(args)
        let receipt = try JSONDecoder().decode(RestoreReceipt.self,from:raw)
        result = "已发布新主线版本 · " + short(receipt.snapshotId)
        previousRestore = nil
    }
    @MainActor private func retryRestore() async {
        guard !busy, let args = previousRestore else { return }
        busy = true
        defer { busy = false }
        do { try await restore(args) } catch { self.error = error.localizedDescription }
    }
    @MainActor private func previewPolicy() async {
        guard !busy, let value else { return }
        busy = true
        defer { busy = false }
        do {
            head = try await currentLibrary().head
            let raw = try await CLI.call(["timeline","preview-policy",action.library.id,"--mode",mode,"--value",String(value)])
            let report = try JSONDecoder().decode(RetentionPreview.self,from:raw)
            guard report.mode == mode && report.value == value else { throw LocalError(message:"服务端返回的策略与所选范围不同，请重新预览。") }
            preview = report
            error = ""
            showingConfirmation = true
        } catch { self.error = error.localizedDescription }
    }
    @MainActor private func submit() async {
        guard ready, !busy else { return }
        busy = true
        defer { busy = false }
        let lib = action.library.id
        do {
            let latest = try await currentLibrary()
            guard latest.head == head else { throw LocalError(message:"NAS 主线已变化，请重新读取并核对后提交。") }
            switch action.operation {
            case .policy:
                guard let p = preview else { return }
                var args = ["timeline","policy",lib,"--mode",p.mode,"--value",String(p.value)]
                if let head { args += ["--head",head] }
                _ = try await CLI.call(args)
                result = "已应用策略 · " + ruleText
            case .restore:
                guard let head else { return }
                let args = ["timeline","restore",lib,"--snapshot",target,"--head",head,"--request",request,"--name",label]
                previousRestore = args
                try await restore(args)
            case .delete:
                guard let head, let plan = deletePreview, let token = plan.planToken, !token.isEmpty,
                      plan.protectedReason?.isEmpty != false else { return }
                let raw = try await CLI.call(["timeline","delete",lib,"--snapshot",target,"--head",head,"--plan-token",token])
                let receipt = try JSONDecoder().decode(DeleteReceipt.self,from:raw)
                guard receipt.deleted, receipt.snapshotId == target else { throw LocalError(message:"删除回执与所选时间点不同，请重新读取。") }
                result = "已移除时间点 " + short(target) + "，其余时间点已重建 " + String(receipt.rebuiltPoints?.count ?? plan.changes.count) + " 个。"
            }
        } catch {
            self.error = error.localizedDescription
            ready = false
        }
    }
}
