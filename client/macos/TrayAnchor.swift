import AppKit
import SwiftUI
import Combine

private final class TrayPosition: ObservableObject {
    @Published var tip: CGFloat = 155
}
private struct TraySurface<Content: View>: View {
    @ObservedObject var position: TrayPosition
    let content: Content
    var body: some View {
        VStack(spacing: 0) {
            Canvas { c, _ in
                var p = Path()
                p.move(to: CGPoint(x:position.tip-8,y:7))
                p.addLine(to:CGPoint(x:position.tip,y:0))
                p.addLine(to:CGPoint(x:position.tip+8,y:7))
                c.fill(p,with:.color(DeskColor.surface))
            }.frame(height:7)
            content.background(DeskColor.surface, in: RoundedRectangle(cornerRadius:13))
                .overlay(RoundedRectangle(cornerRadius:13).strokeBorder(DeskColor.line,lineWidth:0.7))
        }.fixedSize()
    }
}

// Coordinates are measured in AppKit screen points, including on scaled displays.
@MainActor
final class TrayAnchor {
    private let panel = NSPanel(contentRect: .zero,styleMask:[.borderless,.nonactivatingPanel],backing:.buffered,defer:true)
    private let position = TrayPosition()
    private weak var button: NSStatusBarButton?
    private var observers = Set<AnyCancellable>()
    private var mouseMonitor: Any?
    private var localMonitor: Any?
    private var clickedOn: NSPoint?
    private var anchorRect = NSRect.zero
    var isShown: Bool { panel.isVisible }

    init() {
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.level = .popUpMenu
        panel.hidesOnDeactivate = false
        panel.isReleasedWhenClosed = false
        panel.collectionBehavior = [.transient,.moveToActiveSpace,.fullScreenAuxiliary]
        panel.isMovable = false
        panel.identifier = NSUserInterfaceItemIdentifier("xinhu.tray")
    }
    func configure<V: View>(_ content: V, model: LibraryModel, schedule: Schedule) {
        let host = NSHostingView(rootView: TraySurface(position:position,content:content))
        panel.contentView = host
        for publisher in [model.objectWillChange,schedule.objectWillChange] {
            publisher.sink { [weak self] _ in
                DispatchQueue.main.async { self?.layout() }
            }.store(in:&observers)
        }
        NotificationCenter.default.publisher(for:NSApplication.didChangeScreenParametersNotification)
            .sink { [weak self] _ in self?.close() }.store(in:&observers)
        NSWorkspace.shared.notificationCenter.publisher(for:NSWorkspace.activeSpaceDidChangeNotification)
            .sink { [weak self] _ in self?.close() }.store(in:&observers)
    }
    func show(_ button: NSStatusBarButton) {
        self.button = button
        if let event=NSApp.currentEvent, [.leftMouseUp,.rightMouseUp].contains(event.type) {
            clickedOn=NSEvent.mouseLocation
        } else { clickedOn=nil }
        layout(opening:true)
        panel.orderFrontRegardless()
        button.highlight(true)
        mouseMonitor=NSEvent.addGlobalMonitorForEvents(matching:[.leftMouseDown,.rightMouseDown,.otherMouseDown]) { [weak self] _ in
            Task { @MainActor in self?.outsideClick() }
        }
        localMonitor=NSEvent.addLocalMonitorForEvents(matching:[.leftMouseDown,.rightMouseDown,.otherMouseDown,.keyDown]) { [weak self] event in
            guard let self else {return event}
            if event.type == .keyDown {
                if event.keyCode == 53 { self.close(); return nil }
            } else { self.outsideClick() }
            return event
        }
    }
    private func outsideClick() {
        let point=NSEvent.mouseLocation
        if !panel.frame.contains(point) && !anchorRect.contains(point) {close()}
    }
    private func layout(opening: Bool = false) {
        guard opening || isShown,let button,let window=button.window,let host=panel.contentView else{return}
        let measured=window.convertToScreen(button.convert(button.bounds,to:nil))
        let reference=clickedOn ?? NSPoint(x:measured.midX,y:measured.midY)
        guard let screen=NSScreen.screens.first(where:{$0.frame.contains(reference)}) ?? window.screen else{return}
        let bounds=screen.frame
        let thickness=max(screen.safeAreaInsets.top,NSStatusBar.system.thickness)
        let physicalBand=NSRect(x:bounds.minX,y:bounds.maxY-thickness-4,width:bounds.width,height:thickness+8)
        if physicalBand.intersects(measured) && (clickedOn.map { measured.insetBy(dx:-3,dy:-3).contains($0) } ?? true) {
            anchorRect=measured
        } else {
            anchorRect=NSRect(x:reference.x-NSStatusBar.system.thickness/2,y:bounds.maxY-thickness,width:NSStatusBar.system.thickness,height:thickness)
        }
        let fit=host.fittingSize
        let width=ceil(fit.width),height=ceil(fit.height)
        let x=min(max(anchorRect.midX-width/2,bounds.minX+5),bounds.maxX-width-5)
        let y=max(bounds.minY+4,anchorRect.minY-3-height)
        let tip=min(max(anchorRect.midX-x,14),width-14)
        if abs(position.tip-tip)>0.1 {position.tip=tip}
        let frame=NSRect(x:round(x),y:round(y),width:width,height:height)
        if panel.frame != frame { panel.setFrame(frame,display:true) }
    }
    func close() {
        panel.orderOut(nil)
        button?.highlight(false)
        if let mouseMonitor {NSEvent.removeMonitor(mouseMonitor)}
        if let localMonitor {NSEvent.removeMonitor(localMonitor)}
        mouseMonitor=nil;localMonitor=nil;clickedOn=nil
    }
}
