import AppKit
import SwiftUI

struct ReadingRoom: View {
    @ObservedObject var model: LibraryModel
    @ObservedObject var scheduler: Schedule
    @ObservedObject var shell: DesktopShell
    @AppStorage("compactSidebar") var compactSidebar = false
    @State var importing = false
    @State var projectSearch = ""
    @State var editingIgnored: DesktopProject?
    @State var checkoutProject: DesktopProject?
    @State var checkingGit: String?
    @State var gitSyncProject: DesktopProject?
    @State var deletionTarget: LibraryRemoval?
    @State var pairing = false
    @State var scheduleSettings = false
    @State var address = ""
    @State var code = ""
    @State var ca: URL?
    @State var name = ""
    @State var draftUnit: Library?
    @State var checkpoint = false
    @State var bindRequest = false
    @State var stagedSource: URL?
    @State var bindProfile = "project"
    @State var bindBase: String?
    @State var browse: VersionTarget?
    @State var mergeTarget: VersionTarget?
    @State var timelineAction: TimelineAction?
    @State var search = ""
    @State var pendingOnly = false

    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Rectangle().fill(DeskColor.line).frame(width: 1)
            VStack(alignment: .leading, spacing: 0) {
                HStack {
                    Text(shell.page.rawValue).font(.system(size: 26, weight: .bold))
                    Spacer()
                    if model.busy { ProgressView().controlSize(.small) }
                    Button { model.run { try await model.refresh() } } label: {
                        Label("刷新", systemImage: "arrow.clockwise")
                    }.buttonStyle(DeskAction()).disabled(model.busy).keyboardShortcut("r", modifiers: .command)
                }.padding(.horizontal, 29).padding(.top, 21).padding(.bottom, 18)
                ScrollView {
                    VStack(alignment: .leading, spacing: 18) {
                        switch shell.page {
                        case .overview: overview
                        case .projects: projectPage
                        case .libraries: libraryPage
                        case .queue: queuePage
                        case .cache: cachePage
                        case .settings: settingsPage
                        }
                    }.padding(.horizontal, 29).padding(.bottom, 24)
                      .frame(maxWidth: .infinity, alignment: .leading)
                }
                if !model.message.isEmpty || !scheduler.error.isEmpty {
                    let msg = model.message.isEmpty ? scheduler.error : model.message
                    HStack(spacing: 8) {
                        Image(systemName: "info.circle")
                        Text(msg).font(.callout).lineLimit(3).textSelection(.enabled)
                        Spacer()
                        if !model.message.isEmpty {
                            Button { model.message = "" } label: { Image(systemName: "xmark") }
                                .buttonStyle(.plain).accessibilityLabel("关闭消息")
                        }
                    }.foregroundStyle(DeskColor.ink).padding(10).background(DeskColor.selection)
                }
            }.frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
             .background(DeskColor.background)
        }.foregroundStyle(DeskColor.ink).tint(DeskColor.accent)
         .frame(minWidth: 780, minHeight: 535)
         .sheet(isPresented: $importing, onDismiss: { model.run { try await model.refresh() } }) { ImportProjects(model:model) }
         .sheet(item: $editingIgnored) { project in
             if let path = project.path { ProjectIgnoredEditor(unit:project.unitId,path:path) }
         }
         .sheet(item: $gitSyncProject) { project in
             if let path = project.path { GitSyncSheet(model: model, name: project.name, path: path) }
         }
         .sheet(item: $deletionTarget) { target in
             LibraryRemovalSheet(model: model, target: target)
         }
         .sheet(item: $checkoutProject) { project in checkoutSheet(project) }
         .sheet(isPresented: $scheduleSettings) { ScheduleSettings(scheduler) }
         .sheet(isPresented: $pairing) { pairSheet }
         .sheet(item: $mergeTarget, onDismiss: { model.run { try await model.refresh() } }) { target in
             if let version = target.version { MergeBranches(model:model,unit:target.unit,branch:version,title:target.title) }
         }
         .sheet(item: $timelineAction, onDismiss: { model.run { try await model.refresh() } }) {
             TimelineControls(action:$0)
         }
         .sheet(item: $browse, onDismiss: { model.run { try await model.refreshCache() } }) { target in Browse(model: model, target: target).tint(DeskColor.accent) }
         .alert("保存快照", isPresented: $checkpoint) {
             TextField("版本说明（可选）", text: $name)
             Button("取消", role: .cancel) {}
             Button("保存到本机队列") {
                 guard let lib = draftUnit else { return }
                 model.run {
                     _ = try await CLI.call(["snapshot", lib.id, "--name", name])
                     model.jobs = try await CLI.rows(["queue"], Job.self)
                     pendingOnly = true; shell.page = .queue
                 }
             }
         } message: { Text("为「" + (draftUnit?.name ?? "") + "」保存当前版本。") }
         .alert("绑定本机来源", isPresented: $bindRequest) {
             Button("取消", role: .cancel) {}
             Button("绑定") {
                 guard let source = stagedSource, let lib = draftUnit else { return }
                 model.run {
                     var args = ["unit", "bind", lib.id, "--path", source.path, "--profile", bindProfile]
                     if let base = bindBase { args += ["--base", base] }
                     _ = try await CLI.call(args)
                     model.bindings = try await CLI.rows(["bindings"], LocalBinding.self)
                     model.message = "已绑定 " + source.path
                 }
             }
         } message: {
             Text((stagedSource?.path ?? "") + (bindBase.map { "\n接续 NAS 版本 " + $0 } ?? ""))
         }
    }
    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 7) {
            HStack(spacing: 9) {
                if let img = Bundle.main.image(forResource: "Avatar") {
                    Image(nsImage: img).resizable().interpolation(.high).frame(width: 35,height: 35)
                        .clipShape(RoundedRectangle(cornerRadius: 10))
                }
                if !compactSidebar { Text("心狐同步").font(.system(size: 18, weight: .bold)) }
            }.padding(.leading, 7).padding(.top, 18).padding(.bottom, 24)
            ForEach([DesktopPage.overview, .projects, .libraries, .queue, .cache], id: \.self) { nav($0) }
            Spacer()
            nav(.settings)
            Button { compactSidebar.toggle() } label: {
                HStack(spacing: 12) {
                    Image(systemName: "sidebar.left").frame(width: 18)
                    if !compactSidebar { Text("收起侧栏") }
                    Spacer(minLength: 0)
                }.font(.system(size: 12)).foregroundStyle(DeskColor.muted)
                 .padding(.horizontal, 14).frame(height: 33).contentShape(Rectangle())
            }.buttonStyle(.plain).help(compactSidebar ? "展开侧栏" : "收起侧栏")
            Divider().padding(.vertical, 6)
            HStack(spacing: 10) {
                Image(systemName: "laptopcomputer").font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(DeskColor.muted).frame(width: 31,height: 31)
                    .background(DeskColor.surface, in: RoundedRectangle(cornerRadius: 8))
                if !compactSidebar {
                    VStack(alignment: .leading,spacing: 2) {
                        Text("此设备").font(.system(size: 10)).foregroundStyle(DeskColor.muted)
                        Text(Host.current().localizedName ?? "Mac").lineLimit(1).font(.system(size: 12, weight: .medium))
                    }
                }
            }.padding(.leading, 6).padding(.bottom, 13)
        }.padding(.horizontal, 10).frame(width: compactSidebar ? 70 : 184)
         .frame(maxHeight: .infinity).background(DeskColor.sidebar)
    }
    private func nav(_ item: DesktopPage) -> some View {
        let chosen = shell.page == item
        return Button { shell.page = item } label: {
            HStack(spacing: 12) {
                Image(systemName: item.symbol).font(.system(size: 15,weight: .medium)).frame(width: 18)
                if !compactSidebar {
                    Text(item.rawValue).font(.system(size: 13,weight: chosen ? .semibold : .medium))
                    Spacer(minLength: 0)
                    if item == .queue && model.pendingCount > 0 { Text("\(model.pendingCount)").font(.caption.monospacedDigit()) }
                }
            }.padding(.horizontal, 14).frame(maxWidth: .infinity, alignment: .leading).frame(height: 39)
                .background(chosen ? DeskColor.selection : .clear, in: RoundedRectangle(cornerRadius: 8))
                .foregroundStyle(chosen ? DeskColor.accent : DeskColor.ink).contentShape(Rectangle())
        }.buttonStyle(.plain).help(item.rawValue).accessibilityLabel(item.rawValue)
    }

    private var overview: some View {
        VStack(alignment: .leading, spacing: 19) {
            HStack {
                VStack(alignment: .leading, spacing: 8) {
                    HStack(spacing: 6) {
                        Circle().fill(model.reachable ? Color.mint : Color(red:0.96,green:0.71,blue:0.73)).frame(width: 7,height: 7)
                        Text(model.connectionLabel).font(.system(size: 11,weight: .medium))
                    }.foregroundStyle(.white.opacity(0.86))
                    Text("梦枢天罗").font(.system(size: 27, weight: .bold)).foregroundStyle(.white)
                    Text(model.connection.serverUrl ?? "使用 NAS 签发的配对码连接")
                        .font(.system(size: 12)).foregroundStyle(.white.opacity(0.85)).lineLimit(1)
                    Button {
                        if !model.connection.paired { pairing = true }
                        else if model.reachable { shell.page = .libraries }
                        else { model.run { try await model.refresh() } }
                    } label: {
                        Label(model.connection.paired ? (model.reachable ? "打开资料库" : "重新连接") : "配对 NAS",
                              systemImage: model.connection.paired ? "arrow.right" : "link")
                          .font(.system(size: 12, weight: .semibold))
                          .padding(.horizontal, 13).frame(height: 29)
                          .foregroundStyle(Color(red:0.28, green:0.15, blue:0.21))
                          .background(Color(white:0.98), in: RoundedRectangle(cornerRadius: 6))
                    }.buttonStyle(.plain).disabled(model.busy).padding(.top, 4)
                }
                Spacer(minLength: 8)
                VaultIllustration(online: model.reachable).frame(width: 207,height: 136)
            }.padding(.horizontal, 24).padding(.vertical, 14)
             .background(LinearGradient(colors: [Color(red:0.19, green:0.15, blue:0.20), Color(red:0.34, green:0.23, blue:0.30)],startPoint:.leading,endPoint:.trailing),
                         in: RoundedRectangle(cornerRadius: 15))
            HStack(spacing: 10) {
                stat("资料库", model.connection.paired && !model.reachable && model.libraries.isEmpty ? "—" : "\(model.libraries.count)", "square.stack.3d.up", .libraries)
                stat("待上传", "\(model.pendingCount)", "arrow.up.to.line", .queue)
                stat("自动同步", scheduler.running ? "检查中" : scheduler.active ? "已开启" : "已暂停", "arrow.triangle.2.circlepath", .settings)
            }
            if let transfer = model.transfers.first {
                Button { shell.page = .queue } label: {
                    TransferDetails(transfer: transfer, project: model.name(for: transfer.unitId))
                        .padding(14).deskCard().contentShape(Rectangle())
                }.buttonStyle(.plain).help("查看全部传输")
            }
            HStack {
                Text("最近活动的项目").font(.system(size: 16,weight: .bold))
                Spacer()
                Button("查看全部") { shell.focusProject = nil; shell.page = .projects }.buttonStyle(.link)
            }
            if model.projects.isEmpty {
                PanelEmpty(icon:"square.stack.3d.up", title:model.checkingProjects ? "正在读取项目状态" : "还没有项目",
                           detail:"导入一个包含多个项目的父目录，或在资料库绑定现有项目。")
                if !model.checkingProjects && model.connection.paired {
                    Button("导入项目目录…") { importing = true }.buttonStyle(DeskAction(primary:true))
                }
            } else {
                VStack(spacing:0) {
                    ForEach(Array(model.projects.prefix(4))) { project in
                        projectSummaryRow(project, detail:false)
                    }
                }.deskCard()
            }
        }
    }
    private func stat(_ label: String, _ value: String, _ icon: String, _ destination: DesktopPage) -> some View {
        Button {
            if destination == .queue { pendingOnly = true }
            shell.page = destination
        } label: {
            VStack(alignment: .leading,spacing: 11) {
                HStack { Image(systemName: icon).foregroundStyle(DeskColor.accent); Text(label).foregroundStyle(DeskColor.muted); Spacer() }
                    .font(.system(size: 12, weight: .medium))
                Text(value).font(.system(size: 20,weight: .semibold)).monospacedDigit()
            }.padding(.horizontal,15).padding(.vertical,13).frame(maxWidth: .infinity,alignment:.leading).deskCard().contentShape(Rectangle())
        }.buttonStyle(.plain)
    }
}
