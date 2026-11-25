import AppKit
import ServiceManagement
import SwiftUI

@MainActor
final class DesktopShell: NSObject, ObservableObject, NSApplicationDelegate, NSWindowDelegate {
    @Published var focusProject: String?
    @Published var workspaceRequest: WorkspaceRequest?
    private var projectsTimer: Timer?
    @Published var page: DesktopPage = .overview
    @Published private(set) var startAtLogin = false
    @Published private(set) var loginNeedsApproval = false
    @Published private(set) var loginMessage = ""

    let model = LibraryModel()
    let scheduler = Schedule()
    private let loginPlist = "local.kirius.xinhu.login.plist"
    private lazy var loginService = SMAppService.agent(plistName: loginPlist)
    private var helperInstalled: Bool {
        let contents = Bundle.main.bundleURL.appendingPathComponent("Contents")
        return FileManager.default.fileExists(atPath: contents.appendingPathComponent("Library/LaunchAgents/" + loginPlist).path)
            && FileManager.default.isExecutableFile(atPath: contents.appendingPathComponent("Helpers/XinhuLogin").path)
    }
    private let panel = TrayAnchor()
    private var mainWindow: NSWindow?
    private var trayItem: NSStatusItem?
    private var transferMonitor: Task<Void, Never>?
    private var animationTimer: Timer?
    private lazy var idleIcon = menuIcon("TrayFoxIdle")
    private lazy var runningIcons = (0..<8).compactMap { menuIcon("TrayFoxRun" + String($0)) }
    private var animationFrame = 0

    private func menuIcon(_ name: String) -> NSImage? {
        guard let url = Bundle.main.url(forResource: name, withExtension: "png"),
              let image = NSImage(contentsOf: url), image.size.width > 0, image.size.height > 0 else { return nil }
        let scale = min(27 / image.size.width, 18 / image.size.height)
        image.size = NSSize(width: image.size.width * scale, height: image.size.height * scale)
        image.isTemplate = false
        return image
    }
    private let silent = ProcessInfo.processInfo.arguments.contains("--login-item")

    private var inApplications: Bool {
        let app = Bundle.main.bundleURL.resolvingSymlinksInPath().standardizedFileURL.path
        let folders = FileManager.default.urls(for: .applicationDirectory, in: [.localDomainMask, .userDomainMask])
        return folders.contains { app.hasPrefix($0.resolvingSymlinksInPath().standardizedFileURL.path + "/") }
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        installMenu()
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        trayItem = item
        if let button = item.button {
            button.imagePosition = .imageOnly
            button.imageScaling = .scaleNone
            button.image = idleIcon ?? NSImage(systemSymbolName: "hare.fill", accessibilityDescription: "心狐同步")
            button.setAccessibilityLabel("心狐同步")
            button.toolTip = "心狐同步 · 打开同步面板"
            button.target = self
            button.action = #selector(togglePanel)
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        }
        panel.configure(QuickPanel(shell: self, model: model, schedule: scheduler), model: model, schedule: scheduler)
        scheduler.didCheck = { [weak self] in
            guard let self else { return }
            self.model.run { try await self.model.refreshJobs(); self.model.refreshProjects() }
        }
        startTransferMonitor()
        scheduler.start()
        model.refreshProjects()
        model.refreshProjects(remoteGit:true)
        projectsTimer = Timer.scheduledTimer(withTimeInterval: 60, repeats:true) { [weak self] _ in
            Task { @MainActor in self?.model.refreshProjects(remoteGit:true) }
        }
        refreshLoginState()
        if !silent && inApplications && !UserDefaults.standard.bool(forKey: "loginConfigured") {
            // A freshly installed ad-hoc signed bundle can report .notFound
            // until the first registration even when both embedded files exist.
            if (loginService.status == .notRegistered || loginService.status == .notFound) && helperInstalled {
                toggleLogin(true)
            } else {
                UserDefaults.standard.set(true, forKey: "loginConfigured")
            }
        }
        if silent { refreshModel() } else { openMain() }
    }

    private func startTransferMonitor() {
        let env = ProcessInfo.processInfo.environment
        let defaultRoot = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("XinhuSync")
        let root = env["KDRIVE_HOME"].flatMap { $0.isEmpty ? nil : URL(fileURLWithPath: $0) } ?? defaultRoot
        let dir = root.appendingPathComponent("transfers", isDirectory: true)
        transferMonitor = Task {
            while !Task.isCancelled {
                do {
                    let names = (try? FileManager.default.contentsOfDirectory(atPath: dir.path)) ?? []
                    let records = names.contains { $0.hasSuffix(".json") }
                    let items = records ? try await CLI.rows(["transfers"], LiveTransfer.self) : []
                    if items != model.transfers {
                        let active = Set(items.filter { $0.direction == "upload" }.map(\.activityID))
                        let finished = model.transfers.contains { $0.direction == "upload" && !active.contains($0.activityID) }
                        model.transfers = items
                        if finished { try? await model.refreshJobs() }
                    }
                    if !model.transferError.isEmpty { model.transferError = "" }
                    animateFox(items.contains { $0.phase == "transferring" })
                } catch {
                    if !model.transfers.isEmpty { model.transfers = [] }
                    if model.transferError != error.localizedDescription { model.transferError = error.localizedDescription }
                    animateFox(false)
                }
                try? await Task.sleep(for: .milliseconds(500))
            }
        }
    }
    private func animateFox(_ running: Bool) {
        if !running {
            guard animationTimer != nil else { return }
            animationTimer?.invalidate()
            animationTimer = nil
            trayItem?.button?.image = idleIcon
            trayItem?.button?.toolTip = "心狐同步 · 打开同步面板"
            return
        }
        guard animationTimer == nil, !runningIcons.isEmpty else { return }
        animationFrame = 0
        trayItem?.button?.image = runningIcons[0]
        trayItem?.button?.toolTip = "心狐同步 · 正在传输"
        let timer = Timer(timeInterval: 0.05, repeats: true) { [weak self] _ in
            Task { @MainActor in
                guard let self, self.animationTimer != nil else { return }
                self.animationFrame = (self.animationFrame + 1) % self.runningIcons.count
                self.trayItem?.button?.image = self.runningIcons[self.animationFrame]
            }
        }
        RunLoop.main.add(timer, forMode: .common)
        animationTimer = timer
    }
    func applicationDidBecomeActive(_ notification: Notification) { refreshLoginState() }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        openMain(page)
        return false
    }

    func refreshLoginState() {
        let state = loginService.status
        startAtLogin = state == .enabled
        loginNeedsApproval = state == .requiresApproval
        if loginNeedsApproval {
            loginMessage = "请在系统设置的「登录项与扩展」中允许心狐同步在后台运行。"
        } else if !inApplications {
            loginMessage = "将心狐同步移到「应用程序」后，从那里打开即可设置登录自启。"
        } else if !helperInstalled {
            loginMessage = "安装包缺少登录启动组件，请重新安装。"
        } else {
            loginMessage = ""
        }
    }

    func toggleLogin(_ enabled: Bool) {
        refreshLoginState()
        if enabled && !inApplications { return }
        UserDefaults.standard.set(true, forKey: "loginConfigured")
        let state = loginService.status
        if enabled && state == .requiresApproval { openLoginSettings(); return }
        do {
            if enabled && state != .enabled { try loginService.register() }
            if !enabled && (state == .enabled || state == .requiresApproval) { try loginService.unregister() }
            refreshLoginState()
        } catch {
            refreshLoginState()
            if !loginNeedsApproval { loginMessage = "登录启动设置失败：" + error.localizedDescription }
        }
    }

    func openLoginSettings() { SMAppService.openSystemSettingsLoginItems() }

    private func refreshModel() {
        model.run { try await self.model.refresh() }
    }

    @objc private func togglePanel() {
        if panel.isShown { panel.close(); return }
        guard let button = trayItem?.button else { return }
        refreshLoginState()
        refreshModel()
        panel.show(button)
    }

    func openMain(_ target: DesktopPage = .overview) {
        page = target
        if panel.isShown { panel.close() }
        NSApp.setActivationPolicy(.regular)
        if mainWindow == nil {
            let view = ReadingRoom(model: model, scheduler: scheduler, shell: self)
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1020, height: 670),
                                  styleMask: [.titled, .closable, .miniaturizable, .resizable],
                                  backing: .buffered, defer: false)
            window.title = "心狐同步 · Moon Fox Sync"
            window.identifier = NSUserInterfaceItemIdentifier("xinhu.main")
            window.titlebarSeparatorStyle = .none
            window.isReleasedWhenClosed = false
            window.isRestorable = false
            window.contentView = NSHostingView(rootView: view)
            window.minSize = NSSize(width: 860, height: 570)
            if !window.setFrameUsingName("XinhuDesktop") { window.center() }
            window.setFrameAutosaveName("XinhuDesktop")
            window.delegate = self
            mainWindow = window
        }
        if mainWindow?.isMiniaturized == true { mainWindow?.deminiaturize(nil) }
        mainWindow?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        refreshModel()
    }

    func windowWillClose(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
    }

    func application(_ application: NSApplication, open urls: [URL]) {
        guard let path = urls.compactMap({ FinderDomain.hydrationPath(from: $0) }).first else { return }
        openMain(.cache)
        workspaceRequest=WorkspaceRequest(source:path)
    }
    func applicationWillTerminate(_ notification: Notification) { transferMonitor?.cancel(); animationTimer?.invalidate(); projectsTimer?.invalidate(); panel.close(); scheduler.stop() }

    @objc private func showOverview() { openMain() }
    @objc private func showSettings() { openMain(.settings) }
    @objc func quit() { NSApp.terminate(nil) }

    private func installMenu() {
        let bar = NSMenu()
        let app = NSMenuItem()
        bar.addItem(app)
        let appMenu = NSMenu(title: "心狐同步")
        app.submenu = appMenu
        appMenu.addItem(withTitle: "打开心狐同步", action: #selector(showOverview), keyEquivalent: "1").target = self
        appMenu.addItem(withTitle: "设置…", action: #selector(showSettings), keyEquivalent: ",").target = self
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "隐藏心狐同步", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: "退出心狐同步", action: #selector(quit), keyEquivalent: "q").target = self
        let edit = NSMenuItem(title: "编辑", action: nil, keyEquivalent: "")
        bar.addItem(edit)
        edit.submenu = NSMenu(title: "编辑")
        for (title, action, key) in [("撤销", "undo:", "z"), ("剪切", "cut:", "x"), ("复制", "copy:", "c"),
                                     ("粘贴", "paste:", "v"), ("全选", "selectAll:", "a")] {
            edit.submenu?.addItem(withTitle: title, action: Selector(action), keyEquivalent: key)
        }
        let window = NSMenuItem(title: "窗口", action: nil, keyEquivalent: "")
        bar.addItem(window)
        window.submenu = NSMenu(title: "窗口")
        window.submenu?.addItem(withTitle: "最小化", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        window.submenu?.addItem(withTitle: "关闭", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        NSApp.mainMenu = bar
    }
}

private struct QuickPanel: View {
    @ObservedObject var shell: DesktopShell
    @ObservedObject var model: LibraryModel
    @ObservedObject var schedule: Schedule
    private let accent = Color(red: 0.69, green: 0.23, blue: 0.39)

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 10) {
                if let img = Bundle.main.image(forResource: "Avatar") {
                    Image(nsImage: img).resizable().scaledToFit().frame(width: 42, height: 42)
                        .clipShape(RoundedRectangle(cornerRadius: 10))
                }
                VStack(alignment: .leading, spacing: 3) {
                    Text("心狐同步").font(.headline)
                    Text("梦枢天罗 · " + model.connectionLabel).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Button { shell.openMain(.settings) } label: { Image(systemName: "slider.horizontal.3") }
                    .buttonStyle(.borderless).help("打开设置").accessibilityLabel("打开设置")
            }
            VStack(alignment: .leading, spacing: 8) {
                HStack(spacing: 10) {
                    Button { shell.openMain(.queue) } label: {
                        HStack(spacing: 7) {
                            Image(systemName: "arrow.up.arrow.down").foregroundStyle(accent)
                            Text("待传输").font(.subheadline)
                            Spacer(minLength: 0)
                            Text(String(model.pendingCount)).font(.system(size: 19, weight: .semibold)).monospacedDigit()
                            Image(systemName: "chevron.right").font(.caption).foregroundStyle(.secondary)
                        }.contentShape(Rectangle())
                    }.buttonStyle(.plain).accessibilityLabel("查看传输队列").help("查看传输队列")
                    PendingSyncButton(model: model, schedule: schedule).controlSize(.small)
                }
                let operations = ProjectTransfer.group(model.transfers)
                if let operation = operations.first {
                    Button { shell.openMain(.queue) } label: {
                        ProjectTransferView(operation: operation, name: model.name(for: operation.summary.unitId), compact: true)
                    }.buttonStyle(.plain).help("查看项目传输及文件明细")
                    if operations.count > 1 {
                        Text("共 \(operations.count) 项传输").font(.caption2).foregroundStyle(.secondary)
                    }
                }
                if !model.pendingSyncStatus.isEmpty {
                    Text(model.pendingSyncStatus).font(.caption).foregroundStyle(.secondary)
                }
                if let error = model.pendingSyncErrors.first {
                    Text(error).font(.caption2).foregroundStyle(.secondary).lineLimit(2).help(error)
                }
            }.padding(10).background(accent.opacity(0.09), in: RoundedRectangle(cornerRadius: 9))
            VStack(alignment:.leading,spacing:7) {
                HStack {
                    Text("最近项目").font(.caption).foregroundStyle(.secondary)
                    Spacer()
                    Button("全部") { shell.focusProject=nil; shell.openMain(.projects) }
                        .buttonStyle(.borderless).controlSize(.small)
                }
                if model.projects.isEmpty {
                    Text(model.checkingProjects ? "正在读取项目…" : "暂无已绑定或共享的项目").font(.caption).foregroundStyle(.secondary)
                } else {
                    ForEach(Array(model.projects.prefix(3))) { project in
                        Button {
                            shell.focusProject=project.unitId
                            shell.openMain(.projects)
                        } label: {
                            HStack(spacing:8) {
                                Image(systemName:project.kind=="git" ? "point.3.connected.trianglepath.dotted" : "folder")
                                    .foregroundStyle(DeskColor.accent)
                                VStack(alignment:.leading,spacing:2) {
                                    Text(project.name).lineLimit(1).font(.system(size:12,weight:.semibold))
                                    if let git = project.gitLabel { Text(git).font(.caption2).foregroundStyle(.secondary).lineLimit(1) }
                                }
                                Spacer()
                                Text(project.nasLabel).font(.caption).foregroundStyle(project.needsAttention ? DeskColor.accent : .secondary)
                            }.contentShape(Rectangle())
                        }.buttonStyle(.plain)
                    }
                }
            }
            HStack(spacing: 8) {
                Circle().fill(schedule.running ? .blue : schedule.active ? .green : .secondary).frame(width: 7, height: 7)
                Text(schedule.running ? "正在检查同步" : schedule.active ? "自动同步已开启" : "自动同步已暂停")
                    .font(.subheadline)
                Spacer()
                Button(schedule.active ? "暂停" : "启用") { schedule.toggle() }.controlSize(.small)
            }
            if !schedule.error.isEmpty {
                Text(schedule.error).lineLimit(2).font(.caption).foregroundStyle(.red)
            } else if let date = schedule.lastChecked {
                Text("最近检查 \(date.formatted(date: .omitted, time: .shortened))").font(.caption).foregroundStyle(.secondary)
            }
            Divider()
            HStack {
                Button("打开主窗口") { shell.openMain() }.buttonStyle(.borderedProminent).tint(accent)
                Button("立即检查") { schedule.checkNow() }.disabled(schedule.running)
                Spacer()
                Button { shell.quit() } label: { Image(systemName: "power") }
                    .buttonStyle(.borderless).help("退出心狐同步").accessibilityLabel("退出心狐同步")
            }.controlSize(.small)
        }
        .padding(16).frame(width: 304)
    }
}

@main
struct XinhuDesktop {
    @MainActor static func main() {
        let app = NSApplication.shared
        let shell = DesktopShell()
        app.delegate = shell
        withExtendedLifetime(shell) { app.run() }
    }
}
