import AppKit
import SwiftUI

struct GitReport: Decodable {
    let state, branch, upstream: String
    let ahead, behind: Int
    let fetched, dirty: Bool
    var label: String {
        let text: String
        switch state {
        case "equal": text = fetched ? "Git 远端一致" : "Git 引用一致"
        case "ahead": text = "Git 领先 \(ahead)"
        case "behind": text = "Git 落后 \(behind)"
        case "diverged": text = "Git 分叉 ↑\(ahead) ↓\(behind)"
        case "remote_changed": text = "Git 远端有变化"
        case "offline": text = "Git 远端未核实"
        case "no_upstream": text = "Git 未设上游"
        case "upstream_missing": text = "Git 上游不存在"
        case "detached": text = "Git 分离头指针"
        case "unborn": text = "Git 尚无提交"
        case "cached": text = "Git 仅本地引用"
        default: text = "Git 状态待核实"
        }
        return text + (dirty ? " · 工作区有未提交改动" : "")
    }
}
struct DesktopProject: Decodable, Identifiable {
    let unitId, name, kind, nasState, lastActivity: String
    let path, base, head, gitCheckedAt, gitError: String?
    let pending: Int
    let git: GitReport?
    var id: String { unitId }
    var gitLabel: String? { if gitError != nil { return "Git 检查失败" }; return git?.label }
    var nasLabel: String {
        switch nasState {
        case "localFilesDeleted": return "本机同步已暂停"
        case "server_only": return head == nil ? "NAS 空项目" : "仅在 NAS"
        case "current": return "快照已对齐"
        case "pending", "local_only": return "待上传" + (pending > 0 ? " · \(pending)" : "")
        case "empty": return "尚无快照"
        case "remote_ahead": return "NAS 有新版本"
        case "fork": return "已保留分叉"
        case "protected": return "主线受保护"
        case "diverged": return "两端有变更"
        case "baseline": return "已绑定基线"
        default: return pending > 0 ? "待上传 · \(pending)" : "NAS 未核实"
        }
    }
    var needsAttention: Bool { ["pending","fork","remote_ahead","diverged","unknown","protected"].contains(nasState) }
}
struct DesktopProjectReport: Decodable { let items: [DesktopProject]; let warning: String? }

extension ReadingRoom {
    var projectPage: some View {
        VStack(alignment:.leading,spacing:14) {
            HStack {
                Image(systemName:"magnifyingglass").foregroundStyle(DeskColor.muted)
                TextField("搜索项目",text:$projectSearch).textFieldStyle(.plain)
                Spacer()
                if model.checkingProjects { ProgressView().controlSize(.small) }
                Button("刷新状态") { model.refreshProjects(remoteGit:true) }.buttonStyle(DeskAction()).disabled(model.checkingProjects)
                Button("导入项目目录…") { importing=true }.buttonStyle(DeskAction(primary:true)).disabled(!model.connection.paired)
            }.padding(10).deskCard()
            if !model.projectWarning.isEmpty { Text(model.projectWarning).font(.caption).foregroundStyle(DeskColor.muted) }
            if let focused=shell.focusProject {
                HStack {
                    Text("当前项目").font(.caption)
                    Text(model.projects.first(where:{$0.id==focused})?.name ?? String(focused.suffix(8))).font(.subheadline.bold())
                    Spacer()
                    Button("显示全部项目") { shell.focusProject=nil }.buttonStyle(.link)
                }
            }
            let filtered=model.projects.filter { (shell.focusProject == nil || $0.unitId == shell.focusProject)
                && (projectSearch.isEmpty || $0.name.localizedCaseInsensitiveContains(projectSearch)) }
            if filtered.isEmpty { PanelEmpty(icon:"folder",title:"暂无匹配项目",detail:model.connection.paired ? "可导入项目父目录，或查看其他已授权资料库。" : "配对梦枢天罗后，可导入本机的项目目录。") }
            ForEach(filtered) { p in
                VStack(alignment:.leading,spacing:0) {
                    projectSummaryRow(p,detail:true)
                    Divider()
                    HStack {
                        Button("版本历史") { shell.page = .libraries; model.setLibrary(p.id) }.disabled(model.busy)
                        if let path=p.path, p.kind=="git" {
                            Button("同步远端 Git…") { gitSyncProject = p }
                                .disabled(model.busy || checkingGit != nil)
                            Button("深查 Git") {
                                checkingGit=p.id
                                Task { @MainActor in
                                    defer { checkingGit=nil }
                                    do {
                                        _ = try await CLI.call(["project","git","--path",path,"--fetch"])
                                        model.refreshProjects()
                                    } catch { model.message=error.localizedDescription }
                                }
                            }.disabled(checkingGit != nil).help("按需取回 Git 对象，核对领先和落后提交数。")
                            Button("忽略文件…") { editingIgnored=p }
                        }
                        if p.head != nil && model.libraries.contains(where:{$0.id==p.id && $0.role=="sync"}) {
                            Button("接收 NAS 版本…") { checkoutProject=p }
                        }
                        Spacer()
                        deletionMenu(unit: p.id, name: p.name,
                                     local: model.hasLocalRecords(p.id),
                                     nas: model.libraries.contains { $0.id == p.id && $0.role == "sync" })
                    }.font(.caption).padding(.horizontal,13).padding(.vertical,9)
                }.deskCard()
            }
        }
    }
    func projectSummaryRow(_ p: DesktopProject,detail:Bool) -> some View {
        HStack(spacing:11) {
            Image(systemName:p.kind=="git" ? "point.3.connected.trianglepath.dotted" : "folder")
                .foregroundStyle(DeskColor.accent).frame(width:31,height:31).background(DeskColor.selection,in:RoundedRectangle(cornerRadius:8))
            VStack(alignment:.leading,spacing:4) {
                if detail { Text(p.name).font(.system(size:13,weight:.semibold)) }
                else {
                    Button(p.name) { shell.focusProject=p.id;shell.page = .projects }.buttonStyle(.plain).font(.system(size:13,weight:.semibold))
                }
                if let git=p.gitLabel {
                    Text(git + (detail && p.gitCheckedAt != nil ? " · " + desktopTime(p.gitCheckedAt) : ""))
                        .font(.caption).foregroundStyle(DeskColor.muted).lineLimit(2)
                } else {
                    Text(p.path == nil ? "共享资料库" : "项目目录").font(.caption).foregroundStyle(DeskColor.muted)
                }
                if detail && p.nasState == "localFilesDeleted" {
                    Text("可以从版本历史取回文件，或用「接收 NAS 版本…」恢复到新目录并继续同步。")
                        .font(.caption2).foregroundStyle(DeskColor.muted)
                }
                if detail && p.nasState=="protected" {
                    Text("本机内容与共享基线不同；从这里上传会保留为分叉。")
                        .font(.caption2).foregroundStyle(DeskColor.accent)
                }
                if detail, let path=p.path { Text(path).font(.caption2).foregroundStyle(DeskColor.muted).textSelection(.enabled) }
            }
            Spacer()
            VStack(alignment:.trailing,spacing:4) {
                Text(p.nasLabel).font(.system(size:12,weight:.medium)).foregroundStyle(p.needsAttention ? DeskColor.accent : DeskColor.ink)
                Text(desktopTime(p.lastActivity)).font(.caption2).foregroundStyle(DeskColor.muted)
            }
        }.padding(13)
    }
    func checkoutSheet(_ p: DesktopProject) -> some View {
        VStack(alignment:.leading,spacing:14) {
            Text("接收「" + p.name + "」").font(.title3.bold())
            Text("将 NAS 主线版本恢复到一个新目录，并将本机绑定切换到新目录。原目录、其中的未提交修改和 .git 都会保留。新的工作树包含快照内容。")
                .font(.callout).fixedSize(horizontal:false,vertical:true)
            HStack {
                Spacer()
                Button("取消") {checkoutProject=nil}
                Button("选择新目录…") {
                    let picker=NSSavePanel();picker.nameFieldStringValue=p.name+"-NAS"
                    guard picker.runModal() == .OK,let path=picker.url?.path else{return}
                    checkoutProject=nil
                    model.run {
                        _ = try await CLI.call(["project","checkout",p.unitId,"--to",path])
                        try await model.refresh();model.message="已接收 NAS 主线版本到 " + path
                    }
                }.keyboardShortcut(.defaultAction)
            }
        }.padding(22).frame(width:430)
    }
}
