import AppKit
import SwiftUI

private struct LocalProject: Decodable {
    let path, name, kind, sourceKey, origin: String
}
private struct FoundProjects: Decodable { let items: [LocalProject]; let warnings: [String] }
private func encodedPaths(_ files: Set<String>) throws -> String {
    String(decoding: try JSONEncoder().encode(files.sorted()), as: UTF8.self)
}
private struct SharedTarget: Decodable, Identifiable {
    let unitId, key, name: String
    let head, sourceKey: String?
    var id: String { unitId }
    var label: String { name + " · " + String(unitId.suffix(7)) }
    var originKey: String {
        if let sourceKey, !sourceKey.isEmpty { return sourceKey }
        return key.hasPrefix("git:") ? key : ""
    }
}
private struct ProjectImportRow: Identifiable {
    let project: LocalProject
    var id: String { project.path }
    var selected = true
    let request = UUID().uuidString.lowercased()
    var destination = ""
    var rebindFrom: String?
    var acceptedBase: String?
    var ignoredPolicy: IgnoredPolicy?
    var saved = false
    var error = ""
    init(_ p: LocalProject) { project = p }
}

struct ImportProjects: View {
    @ObservedObject var model: LibraryModel
    @Environment(\.dismiss) private var dismiss
    @State private var scope = ""
    @State private var targets = [SharedTarget]()
    @State private var parent = ""
    @State private var rows = [ProjectImportRow]()
    @State private var selectedPath: String?
    @State private var busy = false
    @State private var ready = false
    @State private var notice = ""
    @State private var warnings = [String]()
    @State private var ignoredProject: String?
    @State private var confirming = false

    private var scopes: [Library] { model.libraries.filter { $0.kind == "managed" && $0.role == "sync" && $0.scopeId == nil } }
    private var chosen: [ProjectImportRow] { rows.filter { $0.selected && !$0.saved } }
    private func target(_ row: ProjectImportRow) -> SharedTarget? { targets.first { $0.id == row.destination } }
    private func targetLabel(_ row: ProjectImportRow) -> String {
        if let t = target(row) { return t.label }
        return row.destination == "@new" ? row.project.name + "（新建）" : "待选择"
    }
    private func binding(_ p: String) -> LocalBinding? { model.bindings.first { $0.path == p } }
    private func currentSource(_ row: ProjectImportRow) -> LocalBinding? {
        guard let old = binding(row.project.path), old.unitId != target(row)?.unitId else { return nil }
        return old
    }
    private func suggested(_ p: LocalProject) -> [SharedTarget] {
        guard !p.sourceKey.isEmpty else { return [] }
        return targets.filter { $0.originKey == p.sourceKey }
    }
    private func initialRow(_ p: LocalProject) -> ProjectImportRow {
        var row = ProjectImportRow(p)
        if let old = binding(p.path), targets.contains(where: { $0.id == old.unitId }) { row.destination = old.unitId }
        else {
            let matches = suggested(p)
            if matches.count == 1 { row.destination = matches[0].id }
            else if matches.isEmpty { row.destination = "@new" }
        }
        return row
    }
    private func needsBase(_ row: ProjectImportRow) -> Bool {
        guard let t = target(row), t.head != nil else { return false }
        return binding(row.project.path)?.unitId != t.unitId
    }
    private func problem(_ row: ProjectImportRow) -> String? {
        if row.destination.isEmpty || (row.destination != "@new" && target(row) == nil) { return "请选择要关联的 NAS 项目，或新建独立项目。" }
        if row.destination != "@new" && chosen.contains(where: { $0.id != row.id && $0.destination == row.destination }) {
            return "两个已选目录都指向同一个 NAS 项目。本机请选择一个工作区。"
        }
        if let old = currentSource(row) {
            if model.jobs.contains(where: { $0.unitId == old.unitId && $0.pending }) { return "原项目还有待传版本，完成上传后才能重新关联。" }
            if row.rebindFrom != old.unitId { return "请确认重新关联这个工作区。" }
        }
        if let t = target(row), let b = model.bindings.first(where: { $0.unitId == t.unitId && $0.path != row.project.path }) {
            return "此共享项目已绑定本机目录 " + b.path
        }
        if needsBase(row) && row.acceptedBase != target(row)?.head { return "请核对并确认 NAS 的当前基线。" }
        return nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 15) {
            HStack {
                Text("导入项目").font(.title2.bold())
                Spacer()
                if busy { ProgressView().controlSize(.small) }
                Button("完成") { dismiss() }.disabled(busy)
            }
            HStack {
                Picker("目标资料库", selection: Binding(get: { scope }, set: {
                    if $0 != scope { ready = false; targets = []; scope = $0 }
                })) { ForEach(scopes) { Text($0.name).tag($0.id) } }
                    .frame(maxWidth: 280)
                Spacer()
                if !ready && !notice.isEmpty { Button("重新连接") { Task { await loadTargets() } } }
                Button("选择父目录…", action: pickParent).disabled(scope.isEmpty || !ready)
            }.disabled(busy)
            if !parent.isEmpty { Text(parent).font(.caption).foregroundStyle(.secondary).textSelection(.enabled) }
            if rows.isEmpty {
                PanelEmpty(icon: "folder.badge.plus", title: scopes.isEmpty ? "没有可导入的资料库" : "选择项目所在的父目录",
                           detail: scopes.isEmpty ? "请先配对并取得受管资料库的同步权限。" : "心狐同步会识别 Git 工作区和常见项目目录。")
                Spacer()
            } else {
                HStack(alignment: .top, spacing: 14) {
                    VStack(alignment: .leading, spacing: 7) {
                        HStack {
                            Text("项目（\(rows.count)）").font(.headline)
                            Spacer()
                            Button("全选") { for i in rows.indices where !rows[i].saved { rows[i].selected = true } }
                            Button("清除") { for i in rows.indices { rows[i].selected = false } }
                        }.buttonStyle(.borderless)
                        List(selection: $selectedPath) {
                            ForEach($rows) { $row in
                                HStack {
                                    Toggle(row.project.name, isOn: $row.selected).toggleStyle(.checkbox)
                                        .disabled(row.saved).lineLimit(1)
                                    Spacer()
                                    if row.saved { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
                                    else if !row.error.isEmpty { Image(systemName: "exclamationmark.triangle").foregroundStyle(.orange) }
                                }.tag(row.id).contentShape(Rectangle())
                            }
                        }.border(Color.secondary.opacity(0.18))
                    }.frame(width: 235)
                    if let i = rows.firstIndex(where: { $0.id == selectedPath }) {
                        ScrollView { rowDetail(i).frame(maxWidth: .infinity, alignment: .leading) }
                    } else { Spacer() }
                }.disabled(busy)
            }
            if !warnings.isEmpty {
                DisclosureGroup("有 \(warnings.count) 条扫描提示") {
                    ScrollView { Text(warnings.joined(separator: "\n")).font(.caption).textSelection(.enabled) }.frame(maxHeight: 60)
                }
            }
            if !notice.isEmpty { Text(notice).font(.callout).foregroundStyle(.secondary).textSelection(.enabled) }
            Divider()
            HStack {
                Text("已选 \(chosen.count) 项 · 已导入 \(rows.filter(\.saved).count) 项").font(.caption)
                Spacer()
                Button("核对并导入…") { confirming = true }
                    .buttonStyle(DeskAction(primary: true)).disabled(busy || !ready || chosen.isEmpty || chosen.contains { problem($0) != nil })
            }
        }.padding(22).frame(minWidth: 710, idealWidth: 760, minHeight: 465, idealHeight: 540)
        .interactiveDismissDisabled(busy)
        .onAppear { if scope.isEmpty { scope = scopes.first?.id ?? "" } }
        .task(id: scope) { await loadTargets() }
        .sheet(isPresented: $confirming) {
            VStack(alignment: .leading, spacing: 14) {
                Text("确认项目关联").font(.title2.bold())
                Text("本机项目 → NAS 项目").font(.headline)
                List(chosen) { row in
                    VStack(alignment: .leading, spacing: 4) {
                        HStack {
                            Text(row.project.name).fontWeight(.medium)
                            Image(systemName:"arrow.right").foregroundStyle(.secondary)
                            Text(targetLabel(row))
                        }
                        Text(row.project.path).font(.caption).foregroundStyle(.secondary)
                        if currentSource(row) != nil { Text("重新关联 · 保留旧项目历史").font(.caption).foregroundStyle(.orange) }
                    }
                }
                Text("导入会绑定目录。保存快照时，如果 NAS 基线已变化，服务端会保留分叉版本。").font(.caption)
                HStack { Spacer(); Button("返回修改") { confirming = false }
                    Button("确认导入") { confirming = false; Task { await performImport() } }
                        .buttonStyle(DeskAction(primary: true))
                }
            }.padding(20).frame(width: 540, height: 340)
        }
        .sheet(isPresented: Binding(get: { ignoredProject != nil }, set: { if !$0 { ignoredProject = nil } })) {
            if let i = rows.firstIndex(where: { $0.id == ignoredProject }) {
                IgnorePicker(path: rows[i].project.path, unit: binding(rows[i].project.path)?.unitId, initial: rows[i].ignoredPolicy) { policy in
                    rows[i].ignoredPolicy = policy
                }
            }
        }
    }

    @ViewBuilder private func rowDetail(_ i: Int) -> some View {
        let row = rows[i]
        VStack(alignment: .leading, spacing: 12) {
            Text(row.project.name).font(.title3.bold())
            Text(row.project.path).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            if !row.project.origin.isEmpty {
                Label(row.project.origin, systemImage: "point.3.connected.trianglepath.dotted").font(.callout).textSelection(.enabled)
            }
            Text("项目归属").font(.headline)
            Picker("关联已有 NAS 项目或新建独立项目",selection: Binding(get: { rows[i].destination }, set: { destination in
                rows[i].destination = destination
                rows[i].acceptedBase = nil; rows[i].rebindFrom = nil; rows[i].error = ""
            })) {
                if row.destination.isEmpty { Text("选择 NAS 项目…").tag("") }
                Text("新建独立项目").tag("@new")
                if !targets.isEmpty {
                    Section("关联已有 NAS 项目") {
                        ForEach(targets) { t in
                            Text(optionTitle(t, project: row.project)).tag(t.id)
                        }
                    }
                }
            }.labelsHidden().disabled(row.saved)
            if let old = currentSource(row), !row.saved {
                Text("原关联：" + model.name(for: old.unitId)).font(.caption)
                Toggle("重新关联，保留旧项目历史", isOn: Binding(get: { rows[i].rebindFrom == old.unitId }, set: { rows[i].rebindFrom = $0 ? old.unitId : nil })).toggleStyle(.checkbox)
            }
            if let t = target(row) {
                Label("关联至 " + t.label, systemImage: "link").font(.subheadline)
                if let head = t.head, needsBase(row), !row.saved {
                    Text("NAS 当前版本 " + head).font(.caption.monospaced()).textSelection(.enabled)
                    Toggle("我已核对本地来源，基于此版本导入", isOn: Binding(get: { rows[i].acceptedBase == head }, set: { rows[i].acceptedBase = $0 ? head : nil }))
                        .toggleStyle(.checkbox)
                }
            } else if row.destination == "@new" {
                Label("新建 " + row.project.name, systemImage: "plus.circle").font(.subheadline)
            }
            if row.project.kind == "git" {
                Button("选择 Git 忽略文件…") { ignoredProject = row.id }.disabled(row.saved)
                Text(row.ignoredPolicy?.summary ?? (binding(row.project.path) == nil ? IgnoredPolicy().summary : "沿用此项目的备份范围")).font(.caption).foregroundStyle(.secondary)
            }
            if !row.error.isEmpty { Text(row.error).foregroundStyle(.red).textSelection(.enabled) }
            else if !row.saved, let why = problem(row) { Text(why).font(.caption).foregroundStyle(.secondary) }
        }
    }

    private func optionTitle(_ target: SharedTarget, project: LocalProject) -> String {
        let matches = !project.sourceKey.isEmpty && target.originKey == project.sourceKey
        return target.label + (matches ? " · 同源" : "")
    }

    @MainActor private func loadTargets() async {
        guard !scope.isEmpty else { return }
        ready = false
        let id = scope
        do {
            let values = try await CLI.rows(["project", "list", id], SharedTarget.self)
            guard scope == id, !Task.isCancelled else { return }
            targets = values; ready = true; notice = ""
            rows = rows.map { initialRow($0.project) }
        } catch { if scope == id { notice = error.localizedDescription } }
    }
    private func pickParent() {
        let p = NSOpenPanel()
        p.canChooseDirectories = true; p.canChooseFiles = false; p.allowsMultipleSelection = false
        guard p.runModal() == .OK, let dir = p.url else { return }
        busy = true
        Task { @MainActor in
            defer { busy = false }
            do {
                let raw = try await CLI.call(["project", "discover", "--path", dir.path])
                let result = try JSONDecoder().decode(FoundProjects.self, from: raw)
                rows = result.items.map(initialRow); selectedPath = rows.first?.id
                warnings = result.warnings; parent = dir.path
                notice = rows.isEmpty ? "此目录下未发现可导入的项目。" : ""
            } catch { notice = error.localizedDescription }
        }
    }
    @MainActor private func performImport() async {
        guard !busy, ready else { return }; busy = true
        defer { busy = false }
        for i in rows.indices where rows[i].selected && !rows[i].saved && problem(rows[i]) == nil {
            let row = rows[i]
            do {
                var args = ["project", "import", scope, "--path", row.project.path, "--name", row.project.name]
                if row.destination == "@new" { args += ["--new","--request",row.request] }
                else { args += ["--existing",row.destination] }
                if let old = currentSource(row), row.rebindFrom == old.unitId { args += ["--rebind-from",old.unitId] }
                var payload: String?
                if let base = row.acceptedBase { args += ["--base", base] }
                if let policy = row.ignoredPolicy {
                    args += ["--ignore-mode", policy.mode.rawValue, "--includes-stdin"]
                    payload = try encodedPaths(policy.items)
                }
                _ = try await CLI.call(args,input:payload); rows[i].saved = true; rows[i].error = ""
            } catch { rows[i].error = error.localizedDescription }
        }
        do {
            targets = try await CLI.rows(["project", "list", scope], SharedTarget.self)
            try await model.refresh()
            notice = rows.contains(where: { !$0.error.isEmpty }) ? "部分项目尚未导入。查看项目提示，修正后可重试。" : "所选项目已绑定，可在资料库中保存快照。"
        } catch { notice = error.localizedDescription }
    }
}

private enum IgnoredMode: String, Decodable {
    case all, selected
}
private struct IgnoredPolicy: Decodable {
    var mode: IgnoredMode = .all
    var items = Set<String>()

    init() {}
    private enum CodingKeys: String, CodingKey { case mode, items }
    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        mode = try values.decode(IgnoredMode.self, forKey: .mode)
        items = try values.decodeIfPresent(Set<String>.self, forKey: .items) ?? []
    }
    func includes(_ path: String) -> Bool {
        mode == .all ? !items.contains(path) : items.contains(path)
    }
    mutating func set(_ path: String, included: Bool) {
        if included == (mode == .selected) { items.insert(path) }
        else { items.remove(path) }
    }
    var summary: String {
        if mode == .selected { return "仅备份勾选的 \(items.count) 个 Git 忽略文件" }
        return items.isEmpty ? "备份全部 Git 忽略文件，包含今后新增的文件" : "备份全部 Git 忽略文件，已排除 \(items.count) 项"
    }
}
private struct IgnoredChoice: Decodable, Identifiable {
    let path: String
    let size: Int64?
    var id: String { path }
}
private struct IgnorePicker: View {
    let path: String
    let unit: String?
    let initial: IgnoredPolicy?
    let apply: (IgnoredPolicy) async throws -> Void
    @Environment(\.dismiss) private var dismiss
    @State private var policy = IgnoredPolicy()
    @State private var files = [IgnoredChoice]()
    @State private var loading = true
    @State private var ready = false
    @State private var message = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 13) {
            Text("Git 忽略文件").font(.title2.bold())
            Text(path).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            HStack {
                Text(policy.summary).font(.callout)
                Spacer()
                Button("全选") { policy = IgnoredPolicy() }
                Button("清空选择") { policy = IgnoredPolicy(); policy.mode = .selected }
            }.disabled(loading || !ready)
            if loading { ProgressView().controlSize(.small) }
            List(files) { file in
                HStack {
                    Toggle(isOn: Binding(get: { policy.includes(file.path) }, set: { policy.set(file.path, included: $0) })) {
                        Text(file.path).lineLimit(1).truncationMode(.middle).help(file.path)
                    }.toggleStyle(.checkbox)
                    Spacer()
                    Text(file.size.map { ByteCountFormatter.string(fromByteCount:$0,countStyle:.file) } ?? "已保存的规则")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }.disabled(loading || !ready)
            if ready && files.isEmpty { Text("当前没有 Git 忽略文件。").font(.caption) }
            if !message.isEmpty { Text(message).font(.callout).foregroundStyle(.secondary).textSelection(.enabled) }
            HStack {
                if !loading && !ready { Button("重试") { Task { await load() } } }
                Spacer(); Button("取消"){ dismiss() }
                Button("保存选择"){
                    loading = true
                    Task { @MainActor in
                        defer { loading = false }
                        do { try await apply(policy); dismiss() } catch { message = error.localizedDescription }
                    }
                }.buttonStyle(DeskAction(primary:true)).disabled(loading || !ready)
            }
        }.padding(20).frame(minWidth:580, minHeight:385)
        .task { await load() }
    }
    @MainActor private func load() async {
        loading = true
        defer { loading = false }
        do {
            var saved = initial ?? IgnoredPolicy()
            if initial == nil, let unit {
                saved = try JSONDecoder().decode(IgnoredPolicy.self, from: await CLI.call(["project","includes",unit]))
            }
            let found = try await CLI.rows(["project","ignored","--path",path,"--min-bytes","0"],IgnoredChoice.self)
            let missing = saved.items.subtracting(found.map(\.path)).sorted()
            files = found + missing.map { IgnoredChoice(path: $0, size: nil) }
            policy = saved; ready = true; message = ""
        } catch { ready = false; message = error.localizedDescription }
    }
}

struct ProjectIgnoredEditor: View {
    let unit: String
    let path: String
    var body: some View {
        IgnorePicker(path: path, unit: unit, initial: nil) { policy in
            _ = try await CLI.call(["project","includes",unit,"--set","--ignore-mode",policy.mode.rawValue,"--includes-stdin"], input: try encodedPaths(policy.items))
        }
    }
}
