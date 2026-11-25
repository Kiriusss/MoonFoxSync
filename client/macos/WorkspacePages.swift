import AppKit
import SwiftUI

extension ReadingRoom {
    var libraryPage: some View {
        VStack(alignment: .leading, spacing: 15) {
            if model.libraries.isEmpty {
                PanelEmpty(icon: "folder", title: model.connection.paired ? "暂无可浏览的资料库" : "连接 NAS 后浏览资料库",
                           detail: model.connection.paired ? "检查连接，或在梦枢天罗中为此设备授予资料库权限。" : "使用同步配对码可上传项目；只读配对码可浏览和下载。")
                if !model.connection.paired { Button("配对 NAS") { pairing = true }.buttonStyle(DeskAction(primary: true)) }
            } else {
                HStack {
                    Image(systemName: "magnifyingglass").foregroundStyle(DeskColor.muted)
                    TextField("查找资料库", text: $search).textFieldStyle(.plain)
                    if !search.isEmpty { Button { search = "" } label: { Image(systemName: "xmark.circle.fill") }.buttonStyle(.plain) }
                }.padding(10).deskCard()
                let filtered = model.libraries.filter { search.isEmpty || $0.name.localizedCaseInsensitiveContains(search) }
                if filtered.isEmpty { Text("没有匹配的资料库").foregroundStyle(DeskColor.muted) }
                ForEach(filtered) { lib in
                    VStack(alignment: .leading, spacing: 0) {
                        Button { model.setLibrary(lib.id) } label: {
                            HStack(spacing: 12) {
                                Image(systemName: lib.kind == "managed" ? "shippingbox" : "externaldrive")
                                    .font(.system(size: 17)).foregroundStyle(DeskColor.accent)
                                    .frame(width: 35,height: 35).background(DeskColor.selection, in: RoundedRectangle(cornerRadius: 9))
                                VStack(alignment: .leading, spacing: 4) {
                                    Text(lib.name).font(.system(size: 14, weight: .semibold))
                                    Text([lib.kind == "managed" ? "版本资料库" : "NAS 目录", lib.role == "sync" ? "可同步" : "只读"].joined(separator: " · "))
                                        .font(.caption).foregroundStyle(DeskColor.muted)
                                }
                                Spacer()
                                Image(systemName: model.selected == lib.id ? "chevron.down" : "chevron.right")
                                    .font(.caption).foregroundStyle(DeskColor.muted)
                            }.padding(14).contentShape(Rectangle())
                        }.buttonStyle(.plain).disabled(model.busy)
                        if model.selected == lib.id {
                            Divider()
                            libraryDetail(lib).padding(14)
                        }
                    }.deskCard()
                }
            }
        }
    }
    @ViewBuilder func libraryDetail(_ lib: Library) -> some View {
        if lib.kind == "external" {
            Button("浏览 NAS 目录") { browse = VersionTarget(unit:lib.id,version:nil,title:lib.name) }.buttonStyle(DeskAction())
        } else {
            VStack(alignment: .leading, spacing: 13) {
                if lib.role == "sync" {
                    HStack {
                        Menu("绑定本机来源") {
                            Button("项目目录…") { stageBinding(lib,"project") }
                            Button("单个文件…") { stageBinding(lib,"file") }
                        }.fixedSize()
                        if let binding = model.bindings.first(where: { $0.unitId == lib.id }) {
                            Text(binding.path).font(.caption).foregroundStyle(DeskColor.muted)
                                .lineLimit(1).truncationMode(.middle).help(binding.path)
                        }
                        Spacer()
                        Button("保存快照") { name = ""; draftUnit = lib; checkpoint = true }
                            .buttonStyle(DeskAction(primary:true)).disabled(!model.bindings.contains { $0.unitId == lib.id })
                    }.disabled(model.busy)
                }
                HStack {
                    Text("版本历史").font(.system(size: 12,weight:.semibold)).foregroundStyle(DeskColor.muted)
                    Spacer()
                    deletionMenu(unit: lib.id, name: lib.name, local: model.hasLocalRecords(lib.id), nas: lib.role == "sync")
                    if lib.role == "sync" {
                        Button("历史保存策略…") { timelineAction = TimelineAction(library:lib, operation:.policy) }
                            .buttonStyle(DeskAction())
                    }
                }
                if model.revisions.isEmpty {
                    Text(model.busy ? "正在读取…" : "尚无已发布的快照").font(.callout).foregroundStyle(DeskColor.muted)
                } else {
                    ForEach(model.revisions) { revision in
                        HStack {
                            VStack(alignment: .leading, spacing: 3) {
                                Text(revision.label.flatMap { $0.isEmpty ? nil : $0 } ?? String(revision.id.suffix(10)))
                                    .font(.system(size:13,weight:.medium))
                                Text(desktopTime(revision.committedAt) + (revision.outcome == "fork" ? " · 分叉版本" : ""))
                                    .font(.caption).foregroundStyle(DeskColor.muted)
                            }
                            Spacer()
                            if revision.outcome == "fork" && lib.role == "sync" {
                                if model.revisions.contains(where: { $0.mergeParentSnapshotId == revision.id }) {
                                    Text("已合并").font(.caption).foregroundStyle(DeskColor.muted)
                                } else {
                                    Button("合并到主线") { mergeTarget = VersionTarget(unit:lib.id,version:revision.id,title:lib.name) }
                                }
                            }
                            Button("浏览文件") { browse = VersionTarget(unit:lib.id,version:revision.id,title:lib.name) }
                            Button("恢复到…") { restore(lib, revision) }
                            if lib.role == "sync" {
                                Menu {
                                    Button("恢复为当前版本…") { timelineAction = TimelineAction(library:lib,operation:.restore,revision:revision) }
                                        .disabled(revision.id == lib.head)
                                    Button("删除并重建…",role:.destructive) { timelineAction = TimelineAction(library:lib,operation:.delete,revision:revision) }
                                } label: { Image(systemName:"ellipsis.circle") }
                                    .menuStyle(.borderlessButton).fixedSize()
                                    .accessibilityLabel("版本操作 · " + (revision.label.flatMap { $0.isEmpty ? nil : $0 } ?? String(revision.id.suffix(10))))
                                    .help("恢复为当前版本或删除并重建时间线")
                            }
                        }.buttonStyle(DeskAction()).disabled(model.busy)
                    }
                }
            }
        }
    }
    private func stageBinding(_ lib: Library, _ profile: String) {
        let picker = NSOpenPanel()
        picker.canChooseDirectories = profile == "project"; picker.canChooseFiles = profile == "file"
        guard picker.runModal() == .OK, let source = picker.url else { return }
        draftUnit = lib; stagedSource = source; bindProfile = profile
        model.run {
            let data = try await CLI.call(["unit","bind",lib.id,"--path",source.path,"--profile",profile,"--preview"])
            let binding = try JSONSerialization.jsonObject(with:data) as? [String:Any]
            bindBase = binding?["baseSnapshotId"] as? String
            bindRequest = true
        }
    }
    private func restore(_ lib: Library, _ revision: Revision) {
        let picker = NSSavePanel()
        picker.nameFieldStringValue = lib.name + "-" + String(revision.id.suffix(6))
        picker.prompt = "恢复到新目录"
        guard picker.runModal() == .OK, let dst = picker.url else { return }
        model.run {
            _ = try await CLI.call(["restore", lib.id, revision.id, "--to", dst.path])
            model.message = "已恢复到 " + dst.path
        }
    }
    var queuePage: some View {
        VStack(alignment:.leading, spacing:14) {
            HStack {
                Picker("任务范围", selection: $pendingOnly) {
                    Text("全部任务").tag(false)
                    Text("待上传 · \(model.pendingCount)").tag(true)
                }.pickerStyle(.segmented).labelsHidden().fixedSize()
                Spacer()
                PendingSyncButton(model: model, schedule: scheduler)
            }
            if !model.pendingSyncStatus.isEmpty {
                Text(model.pendingSyncStatus).font(.callout)
            }
            if !model.pendingSyncErrors.isEmpty {
                Text(model.pendingSyncErrors.joined(separator: "\n"))
                    .font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            }
            if !model.transferError.isEmpty {
                Text("读取传输进度失败：" + model.transferError).font(.caption).foregroundStyle(.secondary)
            }
            let activeFileCount = model.transfers.filter { $0.phase == "transferring" && !$0.file.isEmpty }.count
            if activeFileCount > 0 { Text("当前传输 · \(activeFileCount) 个文件").font(.headline) }
            ForEach(ProjectTransfer.group(model.transfers)) { operation in
                ProjectTransferView(operation: operation, name: model.name(for: operation.summary.unitId)).padding(14).deskCard()
            }
            let jobs = model.jobs.reversed().filter { !pendingOnly || $0.pending }
            if jobs.isEmpty && model.transfers.isEmpty {
                PanelEmpty(icon:"tray",title:pendingOnly ? "没有等待上传的快照" : "队列为空",detail:"可从资料库保存文件或项目快照。")
            } else if !jobs.isEmpty {
                VStack(spacing:0) { ForEach(jobs) { jobRow($0) } }.deskCard()
            }
        }
    }
    func jobRow(_ job: Job) -> some View {
        HStack(spacing: 11) {
            Image(systemName: job.pending ? "arrow.up.circle" : job.state == "fork" ? "arrow.triangle.branch" : "checkmark.circle")
                .font(.system(size: 18)).foregroundStyle(job.pending ? DeskColor.accent : DeskColor.muted)
            VStack(alignment: .leading,spacing: 4) {
                Text(job.label.flatMap { $0.isEmpty ? nil : $0 } ?? model.name(for:job.unitId)).font(.system(size: 13,weight:.medium))
                Text([model.name(for:job.unitId),desktopTime(job.capturedAt)].filter { !$0.isEmpty }.joined(separator:" · "))
                    .font(.caption).foregroundStyle(DeskColor.muted)
            }
            Spacer()
            Text(job.pending ? "待上传" : job.state == "published" ? "已发布" : job.state == "fork" ? "分叉版本" : job.state)
                .font(.caption).foregroundStyle(DeskColor.muted)
            if job.pending {
                Button("立即上传") { model.run {
                    _ = try await CLI.call(["sync",job.unitId,"--snapshot",job.id,"--now"])
                    try await model.refresh()
                } }.buttonStyle(DeskAction()).disabled(model.busy || model.syncingPending)
            }
        }.padding(.horizontal,14).padding(.vertical,11)
    }

    var settingsPage: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("启动与驻留").font(.system(size: 15,weight:.bold))
            VStack(alignment:.leading, spacing:0) {
                HStack {
                    settingText("开机自启","登录这台 Mac 后，在菜单栏启动心狐同步。")
                    Spacer()
                    Toggle("开机自启",isOn:Binding(get:{ shell.startAtLogin },set:{ shell.toggleLogin($0) }))
                        .labelsHidden().toggleStyle(.switch).controlSize(.small)
                }.padding(15)
                if !shell.loginMessage.isEmpty {
                    HStack {
                        Text(shell.loginMessage).font(.callout).foregroundStyle(DeskColor.muted).textSelection(.enabled)
                        if shell.loginNeedsApproval { Button("打开系统设置") { shell.openLoginSettings() } }
                    }.padding(.horizontal,15).padding(.bottom,12)
                }
                Divider()
                settingText("关闭主窗口后驻留","从菜单栏图标查看任务、暂停自动同步或退出应用。").padding(15)
            }.deskCard().onAppear { shell.refreshLoginState() }
            Text("同步策略").font(.system(size: 15,weight:.bold)).padding(.top,5)
            VStack(alignment: .leading,spacing:0) {
                HStack {
                    settingText("自动同步",scheduler.active ? "后台每分钟检查已绑定的来源。" : "已暂停，仍可手动保存和上传快照。")
                    Spacer()
                    SourceScheduleButton(model:model)
                    Button("配置策略…") { scheduleSettings = true }.buttonStyle(DeskAction())
                }.padding(15)
                Divider()
                settingText("家庭直连",scheduler.defaults.string(forKey:"homeURL").flatMap { $0.isEmpty ? nil : $0 } ?? "尚未设置家庭 NAS 地址与物理网卡。").padding(15)
                Divider()
                settingText("外出网络",scheduler.defaults.bool(forKey:"awaySmall") ? "自动传小任务，单次最多 10 MiB，24 小时累计最多 100 MiB。" : "仅在手动指定版本时上传。").padding(15)
            }.deskCard()
            TransferSettingsCard()
            CodexBackupCard(model:model,scheduler:scheduler)
            VStack(alignment: .leading, spacing: 10) {
                HStack {
                    Text("本机同步数据").font(.headline)
                    Spacer()
                    if !model.bindings.isEmpty {
                        Menu("移除本机项目…") {
                            ForEach(model.bindings, id: \.unitId) { binding in
                                Button(model.name(for: binding.unitId)) {
                                    deletionTarget = .forget(binding.unitId, model.name(for: binding.unitId))
                                }
                            }
                        }.fixedSize()
                    }
                    Button("清除本机同步数据…", role: .destructive) { deletionTarget = .reset }
                }
            }.padding(15).deskCard().disabled(model.busy)
            Text("NAS 连接").font(.system(size: 15,weight:.bold)).padding(.top,5)
            HStack {
                settingText("梦枢天罗",model.connection.serverUrl ?? "尚未配对")
                Spacer()
                if !model.connection.paired { Button("配对 NAS") { pairing = true }.buttonStyle(DeskAction()) }
                else {
                    Button("输入配对码…") { address = model.connection.serverUrl ?? address; pairing = true }.buttonStyle(DeskAction())
                    Button("检查连接") { model.run { try await model.refresh() } }.buttonStyle(DeskAction())
                }
            }.padding(15).deskCard()
        }
    }
    private func settingText(_ title: String,_ subtitle: String) -> some View {
        VStack(alignment:.leading,spacing:5) {
            Text(title).font(.system(size:13,weight:.semibold))
            Text(subtitle).font(.system(size:12)).foregroundStyle(DeskColor.muted).fixedSize(horizontal:false,vertical:true)
        }
    }
    var pairSheet: some View {
        VStack(alignment:.leading,spacing:16) {
            Text("配对梦枢天罗").font(.title2.bold())
            LabeledContent("服务地址") { TextField("https://NAS 地址:端口",text:$address) }
            LabeledContent("配对码") { SecureField("一次性配对码",text:$code) }
            Button(ca?.lastPathComponent ?? "选择自签 CA 证书（可选）…") {
                let picker = NSOpenPanel(); picker.canChooseDirectories = false
                if picker.runModal() == .OK { ca = picker.url }
            }
            HStack {
                Spacer()
                Button("取消") { pairing = false; code = "" }
                Button("配对") {
                    let secret = code; code = ""; pairing = false
                    model.run {
                        var args = ["pair","--server",address]
                        if let ca { args += ["--ca",ca.path] }
                        _ = try await CLI.call(args,input:secret)
                        try await model.refresh()
                    }
                }.disabled(code.isEmpty || model.busy).keyboardShortcut(.defaultAction)
            }
        }.textFieldStyle(.roundedBorder).padding(24).frame(width:440)
    }
}

extension ReadingRoom {
    var cachePage: some View {
        VStack(alignment: .leading, spacing: 14) {
            if model.cache.isEmpty {
                PanelEmpty(icon: "arrow.down.to.line", title: "尚未取回文件",
                           detail: "在资料库中打开一个快照，选择文件并取回本机，即可在这里管理下载缓存。")
            } else {
                let bytes = model.cache.filter { $0.localPath != nil }.reduce(Int64(0)) { $0 + $1.size }
                Text("本机已下载 " + ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file))
                    .font(.system(size: 14, weight: .semibold))
                ForEach(model.cache) { entry in
                    HStack(spacing: 10) {
                        Image(systemName: entry.localPath == nil ? "icloud" : "doc").foregroundStyle(DeskColor.accent)
                        VStack(alignment: .leading, spacing: 4) {
                            Text(entry.path).font(.system(size: 13, weight: .semibold)).lineLimit(2)
                            Text(model.name(for: entry.unitId) + " · " + String(entry.snapshotId.suffix(8)) + " · "
                                 + (entry.localPath == nil ? "仅 NAS" : "已下载"))
                                .font(.caption).foregroundStyle(DeskColor.muted)
                        }
                        Spacer()
                        if let path = entry.localPath {
                            Button("在 Finder 中显示") {
                                NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
                            }
                            Button("释放缓存") { updateCache(entry, action: "evict") }
                        } else { Button("取回本机") { updateCache(entry, action: "fetch") } }
                    }.buttonStyle(DeskAction()).padding(14).deskCard().disabled(model.busy)
                }
            }
        }
    }
    private func updateCache(_ entry: CacheMember, action: String) {
        model.run {
            _ = try await CLI.call(["cache", action, entry.unitId, entry.snapshotId, "--path", entry.path])
            try await model.refreshCache()
        }
    }
}
