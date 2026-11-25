import SwiftUI

private struct GitSyncPlan: Decodable {
    let state, branch, upstream, head, upstreamHead: String
    let ahead, behind: Int
    let dirty: Bool
    let planToken: String?

    var action: String? {
        if state == "ahead" { return "推送 \(ahead) 个提交" }
        if state == "behind" && !dirty { return "快进 \(behind) 个提交" }
        return nil
    }
    var detail: String {
        switch state {
        case "equal": return "本地分支与远端已一致。"
        case "ahead": return "本地领先远端 \(ahead) 个提交。确认后会将这些提交推送到上游分支。"
        case "behind": return dirty ? "本地落后 \(behind) 个提交。请先提交或储藏工作区修改，再重新检查。" : "本地落后远端 \(behind) 个提交。确认后会快进本地分支并更新工作区文件。"
        case "diverged": return "本地领先 \(ahead) 个提交，同时落后 \(behind) 个提交。请在 Git 中合并或变基后重新检查。"
        case "no_upstream", "upstream_missing": return "请在 Git 中为当前分支设置有效的上游分支。"
        case "detached": return "请先检出本地分支。"
        case "unborn": return "请先在仓库中创建初始提交。"
        default: return "未能核对远端状态（" + state + "）。"
        }
    }
}

struct GitSyncSheet: View {
    @ObservedObject var model: LibraryModel
    let name, path: String
    @Environment(\.dismiss) private var dismiss
    @State private var plan: GitSyncPlan?
    @State private var busy = true
    @State private var message = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack { Text("同步远端 Git").font(.title2.bold()); Spacer(); if busy { ProgressView().controlSize(.small) } }
            Text(name).font(.headline)
            Text(path).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            if let plan {
                if !plan.branch.isEmpty { LabeledContent("本地分支", value: plan.branch) }
                if !plan.upstream.isEmpty { LabeledContent("远端上游", value: plan.upstream) }
                Text(plan.detail)
                if plan.dirty && plan.state != "behind" { Text("工作区有未提交的修改。").font(.caption).foregroundStyle(.secondary) }
            } else if busy { Text("正在核对远端分支…").foregroundStyle(.secondary) }
            if !message.isEmpty { Text(message).foregroundStyle(.secondary).textSelection(.enabled) }
            HStack {
                Button("重新检查") { Task { await run() } }
                Spacer()
                Button("关闭") { dismiss() }
                if let plan, let title = plan.action, plan.planToken != nil {
                    Button(title) { Task { await run(applying: plan) } }.buttonStyle(DeskAction(primary: true))
                }
            }.disabled(busy)
        }.padding(22).frame(width:475)
        .interactiveDismissDisabled(busy)
        .task { await run() }
    }
    @MainActor private func run(applying old: GitSyncPlan? = nil) async {
        busy = true
        message = ""
        defer { busy = false; model.refreshProjects() }
        var args = ["project", "git-sync", "--path", path]
        if let old, let token = old.planToken {
            args += ["--apply", "--expected-head", old.head, "--expected-upstream", old.upstreamHead, "--plan-token", token]
        }
        do {
            plan = try JSONDecoder().decode(GitSyncPlan.self, from: await CLI.call(args))
            if old != nil { message = "Git 同步已完成。" }
        } catch { plan = nil; message = error.localizedDescription }
    }
}
