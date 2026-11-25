import SwiftUI

private struct PathConflict: Decodable, Identifiable {
    let path, mainKind, branchKind, baseKind: String
    let bothPath: String?
    let textAllowed: Bool?
    var id: String { path }
}
private struct BranchPlan: Decodable {
    let unitId, mainHead, branchSnapshotId: String
    let baseSnapshotId: String?
    let baseAmbiguous: Bool?
    let baseHidden: Bool?
    let alreadyMerged: Bool
    private let conflictItems: [PathConflict]?
    private let automaticItems: [String]?
    var conflicts: [PathConflict] { conflictItems ?? [] }
    var autoPaths: [String] { automaticItems ?? [] }
    private enum CodingKeys: String, CodingKey {
        case unitId, mainHead, branchSnapshotId, baseSnapshotId, baseAmbiguous, baseHidden, alreadyMerged
        case conflictItems = "conflicts", automaticItems = "autoPaths"
    }
}
private struct MergeContent: Decodable {
    let kind: String
    let text, target: String?
    let truncated: Bool?
    let size: Int64?
}
private struct MergeDecision: Codable, Equatable {
    let path: String
    var choice: String
    var text: String?
}
private struct MergeReceipt: Decodable { let snapshotId, outcome: String }
private enum MergeSide: String, CaseIterable {
    case base = "共同祖先", main = "当前主线", branch = "分叉版本"
}
struct MergeBranches: View {
    @ObservedObject var model: LibraryModel
    let unit, branch, title: String
    @Environment(\.dismiss) private var dismiss
    @State private var plan: BranchPlan?
    @State private var decisions = [String: MergeDecision]()
    @State private var selected = ""
    @State private var side = MergeSide.main
    @State private var contents = [MergeSide: MergeContent]()
    @State private var confirmation = false
    @State private var busy = false
    @State private var loadingBytes = false
    @State private var readError = ""
    @State private var error = ""
    @State private var label = "手动合并分叉"
    @State private var request = UUID().uuidString.lowercased()
    @State private var result: MergeReceipt?

    private var conflict: PathConflict? { plan?.conflicts.first { $0.path == selected } }
    private var previewID: String { (plan?.mainHead ?? "") + " " + selected }
    private func complete(_ c: PathConflict) -> Bool {
        guard let d = decisions[c.path] else { return false }
        return d.choice != "text" || (d.text?.utf8.count ?? Int.max) <= 131072
    }
    private var textCapable: Bool {
        guard conflict?.textAllowed == true, let main = contents[.main], let branch = contents[.branch],
              [main,branch].contains(where: { $0.kind == "text" }) else { return false }
        return [main,branch].allSatisfy { ($0.kind == "text" && $0.truncated != true) || $0.kind == "missing" }
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                VStack(alignment: .leading) {
                    Text("手动合并分叉").font(.title2.bold())
                    Text(title).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button("完成") { dismiss() }.disabled(busy)
            }
            if let result {
                Label(result.outcome == "published" ? "合并版本已发布到主线" : "合并结果 · " + result.outcome, systemImage: "checkmark.circle.fill").foregroundStyle(.green)
                Text(result.snapshotId).textSelection(.enabled)
            } else if let p = plan {
                HStack {
                    version(p.baseHidden == true ? "共同祖先 · 原时间点已移除" : "共同祖先",p.baseSnapshotId); Spacer(); version("当前主线",p.mainHead)
                    Spacer(); version("分叉版本",p.branchSnapshotId)
                }.padding(12).deskCard()
                if p.baseAmbiguous == true { Text("存在多个最近共同祖先。请逐项确认两份内容之间的差异。").font(.caption) }
                if p.alreadyMerged { Label("此分叉已合并",systemImage:"checkmark.circle") }
                else {
                    if !p.autoPaths.isEmpty {
                        DisclosureGroup("自动合并（\(p.autoPaths.count)）") {
                            ScrollView { Text(p.autoPaths.joined(separator:"\n")).font(.caption.monospaced()).frame(maxWidth:.infinity,alignment:.leading).textSelection(.enabled) }.frame(maxHeight:75)
                        }
                    }
                    if p.conflicts.isEmpty { Label("没有冲突，可以提交合并版本。",systemImage:"checkmark.circle") }
                    else {
                        HStack(alignment:.top,spacing:14) {
                            VStack(alignment:.leading,spacing:6) {
                                Text("冲突（\(p.conflicts.count)）").font(.headline)
                                List(selection:$selected) {
                                    ForEach(p.conflicts) { c in
                                        HStack { Text(c.path).lineLimit(2); Spacer(); if complete(c) {Image(systemName:"checkmark").foregroundStyle(.green)} }.tag(c.id)
                                    }
                                }
                            }.frame(width:210)
                            if let c = conflict { conflictDetail(c) }
                        }.disabled(busy)
                    }
                    HStack {
                        TextField("合并说明",text:$label).textFieldStyle(.roundedBorder).disabled(busy)
                        Button("核对并提交…") { confirmation = true }
                            .buttonStyle(DeskAction(primary:true)).disabled(busy || !p.conflicts.allSatisfy(complete))
                    }
                }
            }
            if !error.isEmpty {
                Text(error).font(.caption).foregroundStyle(.red).textSelection(.enabled)
                Button("重新预览当前主线") { Task { await reload() } }.disabled(busy)
            }
            Spacer(minLength:0)
        }.padding(20).frame(minWidth:730,minHeight:512)
        .interactiveDismissDisabled(busy)
        .task { await reload() }
        .task(id:previewID) { await readContents() }
        .onChange(of:label) { _,_ in request = UUID().uuidString.lowercased() }
        .alert("将合并结果保存为 NAS 新版本？",isPresented:$confirmation) {
            Button("取消",role:.cancel){}
            Button("提交合并") { Task { await commit() } }
        } message: { Text("自动处理 \(plan?.autoPaths.count ?? 0) 项，已选择 \(decisions.count) 项冲突处理方式。两个来源版本保留在历史中。") }
    }
    private func version(_ name:String,_ id:String?) -> some View {
        VStack(alignment:.leading,spacing:4) {Text(name).font(.caption).foregroundStyle(.secondary); Text(id.flatMap{ $0.isEmpty ? nil : String($0.suffix(12)) } ?? "无").font(.caption.monospaced()).help(id ?? "").textSelection(.enabled)}
    }
    @ViewBuilder private func conflictDetail(_ c:PathConflict) -> some View {
        VStack(alignment:.leading,spacing:8) {
            Text(c.path).font(.headline).textSelection(.enabled)
            Picker("内容来源",selection:$side) { ForEach(MergeSide.allCases,id:\.self){ Text($0.rawValue).tag($0) } }.pickerStyle(.segmented)
            if loadingBytes {ProgressView().controlSize(.small)}
            if !readError.isEmpty {Text(readError).font(.caption).foregroundStyle(.secondary)}
            if let doc = contents[side] {
                ScrollView {
                    Text(summary(doc)).font(.caption.monospaced()).textSelection(.enabled)
                        .frame(maxWidth:.infinity,alignment:.leading)
                }.frame(minHeight:54,maxHeight:120).padding(8).background(DeskColor.sidebar,in:RoundedRectangle(cornerRadius:6))
            }
            Picker("采用",selection:Binding(get: {decisions[c.path]?.choice ?? ""},set:{ choose(c.path,$0) })) {
                Text("请选择…").tag("")
                Text("主线 · " + kind(c.mainKind)).tag("main")
                Text("分叉 · " + kind(c.branchKind)).tag("branch")
                if let renamed = c.bothPath, !renamed.isEmpty {Text("保留两份").tag("both")}
                if textCapable || decisions[c.path]?.choice == "text" {Text("手动编辑文本").tag("text")}
            }
            if decisions[c.path]?.choice == "both" {Text("分叉副本：" + (c.bothPath ?? "")).font(.caption).textSelection(.enabled)}
            if decisions[c.path]?.choice == "text" {
                TextEditor(text:Binding(get:{decisions[c.path]?.text ?? ""},set:{ decisions[c.path]=MergeDecision(path:c.path,choice:"text",text:$0);request=UUID().uuidString.lowercased() }))
                    .font(.caption.monospaced()).border(DeskColor.line)
                Text("\(decisions[c.path]?.text?.utf8.count ?? 0) / 131072 字节").font(.caption)
            }
        }.frame(maxWidth:.infinity,alignment:.leading)
    }
    private func summary(_ c:MergeContent) -> String {
        switch c.kind {
        case "text": return (c.text ?? "") + (c.truncated == true ? "\n…（预览截断）" : "")
        case "symlink": return "链接目标：" + (c.target ?? "")
        case "binary": return "二进制文件" + (c.size.map { "（" + ByteCountFormatter.string(fromByteCount:$0,countStyle:.file) + "）" } ?? "")
        case "hidden": return "原时间点已移除"
        default: return kind(c.kind)
        }
    }
    private func kind(_ k:String) -> String { ["missing":"此版本没有这一路径","directory":"目录","file":"文件","symlink":"符号链接"][k] ?? k }
    private func choose(_ path:String,_ choice:String) {
        guard !choice.isEmpty else { decisions.removeValue(forKey:path);request=UUID().uuidString.lowercased();return }
        let txt = choice == "text" ? (contents[.main]?.text ?? contents[.branch]?.text ?? "") : nil
        decisions[path]=MergeDecision(path:path,choice:choice,text:txt)
        request=UUID().uuidString.lowercased()
    }
    @MainActor private func reload() async {
        guard !busy else{return};busy=true;defer{busy=false}
        do {
            let raw=try await CLI.call(["project","merge-plan",unit,"--branch",branch])
            let fresh=try JSONDecoder().decode(BranchPlan.self,from:raw)
            if fresh.mainHead != plan?.mainHead || fresh.baseSnapshotId != plan?.baseSnapshotId {
                decisions=[:]; request=UUID().uuidString.lowercased()
            }
            plan=fresh
            if !fresh.conflicts.contains(where:{$0.path==selected}) {selected=fresh.conflicts.first?.path ?? ""}
            error=""
        } catch {self.error=error.localizedDescription}
    }
    @MainActor private func readContents() async {
        contents=[:];readError="";loadingBytes=false
        guard let p=plan,let c=conflict else{return}
        let key=previewID;loadingBytes=true
        defer { if key==previewID && !Task.isCancelled {loadingBytes=false} }
        for (s,id,k) in [(MergeSide.main,Optional(p.mainHead),c.mainKind),(.branch,Optional(p.branchSnapshotId),c.branchKind),(.base,p.baseSnapshotId,c.baseKind)]{
            guard !Task.isCancelled && key==previewID else{return}
            if s == .base && p.baseHidden == true {contents[s]=MergeContent(kind:"hidden",text:nil,target:nil,truncated:nil,size:nil);continue}
            guard let id, !id.isEmpty, k != "missing" else {contents[s]=MergeContent(kind:"missing",text:nil,target:nil,truncated:nil,size:nil);continue}
            do {
                let raw=try await CLI.call(["project","merge-read",unit,"--snapshot",id,"--path",c.path])
                let doc=try JSONDecoder().decode(MergeContent.self,from:raw)
                guard !Task.isCancelled && key==previewID else{return};contents[s]=doc
            } catch {if key==previewID && !Task.isCancelled {readError=error.localizedDescription}}
        }
    }
    @MainActor private func commit() async {
        guard !busy,let p=plan,!p.alreadyMerged,p.conflicts.allSatisfy(complete) else{return}
        busy=true;defer{busy=false}
        do {
            let payload=try JSONEncoder().encode(p.conflicts.compactMap{decisions[$0.path]})
            let raw=try await CLI.call(["project","merge-commit",unit,"--main",p.mainHead,"--branch",branch,
                                        "--request",request,"--name",label],input:String(decoding:payload,as:UTF8.self))
            result=try JSONDecoder().decode(MergeReceipt.self,from:raw);error=""
            model.run {try await model.refresh()}
        } catch {self.error=error.localizedDescription+"\n如主线版本已变化，请重新预览并核对冲突。"}
    }
}
