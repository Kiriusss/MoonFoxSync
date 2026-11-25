import SwiftUI

private enum TransferPolicy: String, CaseIterable {
    case network, home, manual
    var caption: String {
        switch self { case .network:return "跟随网络策略"; case .home:return "仅家庭网络"; case .manual:return "仅手动" }
    }
}
private func intervalValue(_ text: String) -> Int? {
    guard let value = Int(text.trimmingCharacters(in:.whitespacesAndNewlines)), (1...1440).contains(value) else { return nil }
    return value
}
private struct PolicyFields: View {
    @Binding var policy: String
    @Binding var minutes: String
    var body: some View {
        HStack(spacing:12) {
            Picker("上传策略",selection:$policy) {
                ForEach(TransferPolicy.allCases,id:\.rawValue){ Text($0.caption).tag($0.rawValue) }
            }.frame(maxWidth:230)
            TextField("间隔",text:$minutes).textFieldStyle(.roundedBorder).frame(width:54)
                .accessibilityLabel("间隔分钟")
            Text("分钟").foregroundStyle(DeskColor.muted)
        }
    }
}
private struct CodexConfig: Decodable {
    var enabled: Bool
    var policy: String?
    var intervalMinutes: Int?
    var lastBackup, lastError, root, unitId, scopeId: String?
}
struct CodexBackupCard: View {
    @ObservedObject var model: LibraryModel
    @ObservedObject var scheduler: Schedule
    @State private var config: CodexConfig?
    @State private var policy = "network"
    @State private var minutes = "30"
    @State private var scope = ""
    @State private var busy = false
    @State private var message = ""
    private var scopes:[Library]{model.libraries.filter{$0.kind=="managed" && $0.role=="sync" && $0.scopeId==nil}}
    private var fixedScope: String? { if let s=config?.scopeId,!s.isEmpty{return s};return nil }
    private var valid: Bool { intervalValue(minutes) != nil && !scope.isEmpty }
    private var changed: Bool { config?.policy != policy || config?.intervalMinutes != intervalValue(minutes) }
    var body: some View {
        VStack(alignment:.leading,spacing:11) {
            HStack {
                Image(systemName:"text.bubble").foregroundStyle(DeskColor.accent)
                Text("Codex 对话历史").font(.headline)
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button { Task { await reload() } } label: {Image(systemName:"arrow.clockwise")}.help("刷新备份状态")
            }
            if config != nil {
                if let fixedScope {
                    Text("目标资料库 · " + model.name(for:fixedScope)).font(.callout).textSelection(.enabled)
                } else {
                    Picker("目标资料库",selection:$scope) {
                        Text("选择资料库…").tag("")
                        ForEach(scopes){Text($0.name).tag($0.id)}
                    }.frame(maxWidth:360)
                    Text("将保存对话及工具输出，可由目标资料库的授权设备读取。").font(.caption).foregroundStyle(DeskColor.muted)
                }
                PolicyFields(policy:$policy,minutes:$minutes)
                HStack {
                    Text(config?.enabled == true ? (scheduler.active ? "自动备份已启用" : "已配置 · 全局自动同步已暂停") : "自动备份已停止")
                        .font(.caption).foregroundStyle(DeskColor.muted)
                    Spacer()
                    if config?.enabled == true {
                        if changed {Button("保存策略"){save()}.disabled(!valid)}
                        Button("停止自动备份"){execute(["disable"])}
                    } else {Button("启用备份"){save()}.disabled(!valid)}
                    Button("立即备份"){execute(["run"])}.disabled(config?.unitId?.isEmpty != false)
                }
                if let last=config?.lastBackup,!last.isEmpty {Text("最近备份 · "+last).font(.caption).textSelection(.enabled)}
                if let root=config?.root,!root.isEmpty {Text(root).font(.caption).foregroundStyle(DeskColor.muted).textSelection(.enabled)}
                if let e=config?.lastError,!e.isEmpty {Text(e).font(.caption).foregroundStyle(.orange).textSelection(.enabled)}
                if fixedScope == nil && scopes.isEmpty {Text("请先配对并取得受管资料库的同步权限。").font(.caption)}
                if intervalValue(minutes) == nil {Text("间隔须为 1 至 1440 分钟。").font(.caption)}
            }
            if !message.isEmpty {Text(message).font(.caption).textSelection(.enabled)}
        }.disabled(busy).padding(15).deskCard().task { await reload() }
        .onChange(of:scopes.map(\.id)) { _,_ in if fixedScope==nil && scope.isEmpty && scopes.count==1 {scope=scopes[0].id} }
    }
    private func save(){
        guard let interval = intervalValue(minutes) else { return }
        execute(["enable","--policy",policy,"--interval",String(interval),"--scope",scope])
    }
    private func execute(_ args:[String]) {
        guard !busy else{return};busy=true
        Task { @MainActor in
            defer { busy=false }
            do {
                _=try await CLI.call(["preset","codex"]+args)
                try await getStatus()
                message = args.first=="run" ? "本次备份已完成。" : ""
            } catch { message=error.localizedDescription }
        }
    }
    @MainActor private func getStatus()async throws{
        let data=try await CLI.call(["preset","codex","status"])
        let s=try JSONDecoder().decode(CodexConfig.self,from:data)
        config=s; policy=s.policy.flatMap{TransferPolicy(rawValue:$0)}?.rawValue ?? "network"
        minutes=String(max(s.intervalMinutes ?? 30,1))
        if let id=s.scopeId,!id.isEmpty{scope=id}else if scope.isEmpty && scopes.count==1 {scope=scopes[0].id}
    }
    @MainActor private func reload()async{
        guard !busy else{return};busy=true;defer{busy=false}
        do{try await getStatus();message=""}catch{message=error.localizedDescription}
    }
}

struct SourceScheduleButton: View {
    @ObservedObject var model: LibraryModel
    @State private var shown = false
    var body: some View {
        Button("按来源设置…") { shown=true }.buttonStyle(DeskAction())
            .sheet(isPresented:$shown){ SourceSchedules(model:model) }
    }
}
private struct SourceConfig: Decodable { let unitId, policy: String; let intervalMinutes:Int }
private struct SourceSchedules: View {
    @ObservedObject var model: LibraryModel
    @Environment(\.dismiss) private var dismiss
    @State private var selected = ""
    @State private var policy = "network"
    @State private var minutes = "1"
    @State private var loaded = ""
    @State private var saving = false
    @State private var message = ""
    private var sources:[LocalBinding]{model.bindings.filter{["project","file"].contains($0.profile)}}
    var body: some View{
        VStack(alignment:.leading,spacing:14){
            HStack {Text("按来源设置").font(.title2.bold()); Spacer();Button("完成"){dismiss()}.disabled(saving)}
            if sources.isEmpty {PanelEmpty(icon:"folder",title:"还没有绑定来源",detail:"绑定一个文件或项目后，可在这里设置检查间隔和上传策略。")}
            else {
                Picker("文件或项目",selection:$selected){
                    ForEach(sources,id:\.unitId){b in Text(model.name(for:b.unitId)+" · "+URL(fileURLWithPath:b.path).lastPathComponent).tag(b.unitId)}
                }.disabled(saving)
                if let b=sources.first(where:{$0.unitId==selected}) {Text(b.path).font(.callout).foregroundStyle(DeskColor.muted).textSelection(.enabled)}
                PolicyFields(policy:$policy,minutes:$minutes).disabled(loaded != selected || saving)
                HStack {
                    Text("间隔范围 1 至 1440 分钟").font(.caption).foregroundStyle(DeskColor.muted)
                    Spacer()
                    Button("保存此来源"){save()}.buttonStyle(DeskAction(primary:true))
                        .disabled(saving || loaded != selected || intervalValue(minutes) == nil)
                }
            }
            if !message.isEmpty {Text(message).font(.callout).textSelection(.enabled)}
            Spacer()
        }.padding(20).frame(width:520,height:305).interactiveDismissDisabled(saving)
        .onAppear {selected=sources.first?.unitId ?? ""}.task(id:selected){await load()}
    }
    @MainActor private func load()async{
        let id=selected;loaded="";message=""
        guard !id.isEmpty else{return}
        do {
            let raw=try await CLI.call(["schedule","get",id])
            let s=try JSONDecoder().decode(SourceConfig.self,from:raw)
            guard id==selected,!Task.isCancelled else{return}
            policy=s.policy;minutes=String(s.intervalMinutes);loaded=s.unitId
        }catch{if !Task.isCancelled{message=error.localizedDescription}}
    }
    private func save(){
        guard !saving, selected==loaded, let interval = intervalValue(minutes) else{return};saving=true
        Task{@MainActor in
            defer{saving=false}
            do{
                let raw=try await CLI.call(["schedule","set",selected,"--policy",policy,"--interval",String(interval)])
                let s=try JSONDecoder().decode(SourceConfig.self,from:raw)
                policy=s.policy;minutes=String(s.intervalMinutes);message="已保存此来源的同步策略。"
            }catch{message=error.localizedDescription}
        }
    }
}
