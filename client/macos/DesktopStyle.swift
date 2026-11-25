import SwiftUI
import AppKit

enum DeskColor {
    static func adaptive(_ light: UInt32, _ dark: UInt32) -> Color {
        Color(nsColor: NSColor(name: nil) { appearance in
            let code = appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? dark : light
            return NSColor(srgbRed: Double((code >> 16) & 255) / 255, green: Double((code >> 8) & 255) / 255, blue: Double(code & 255) / 255, alpha: 1)
        })
    }
    static let background = adaptive(0xFAF9F8, 0x201F22)
    static let sidebar = adaptive(0xF5F2F2, 0x262327)
    static let surface = adaptive(0xFFFFFF, 0x29272B)
    static let line = adaptive(0xE7E2E2, 0x403A40)
    static let ink = adaptive(0x302A30, 0xF5EDF1)
    static let muted = adaptive(0x70676D, 0xB9AEB6)
    static let accent = adaptive(0x983E57, 0xE89AAE)
    static let selection = adaptive(0xEFDFE3, 0x49323C)
}
extension View {
    func deskCard() -> some View {
        background(RoundedRectangle(cornerRadius: 13).fill(DeskColor.surface))
            .overlay(RoundedRectangle(cornerRadius: 13).strokeBorder(DeskColor.line, lineWidth: 1))
    }
}
struct DeskAction: ButtonStyle {
    var primary = false
    func makeBody(configuration: Configuration) -> some View {
        configuration.label.font(.system(size: 12, weight: .semibold))
            .padding(.horizontal, 12).frame(minHeight: 31)
            .foregroundStyle(primary ? Color.white : DeskColor.ink)
            .background(RoundedRectangle(cornerRadius: 7).fill(primary ? Color(red: 0.55, green: 0.22, blue: 0.33) : DeskColor.surface))
            .overlay(RoundedRectangle(cornerRadius: 7).strokeBorder(primary ? Color.clear : DeskColor.line))
            .opacity(configuration.isPressed ? 0.73 : 1)
    }
}
struct PanelEmpty: View {
    var icon: String, title: String, detail: String
    var body: some View {
        HStack(spacing: 15) {
            Image(systemName: icon).font(.system(size: 20, weight: .regular))
                .foregroundStyle(DeskColor.muted).frame(width: 44, height: 44)
                .background(DeskColor.sidebar, in: RoundedRectangle(cornerRadius: 12))
            VStack(alignment: .leading, spacing: 6) {
                Text(title).font(.system(size: 14, weight: .semibold)).foregroundStyle(DeskColor.ink)
                Text(detail).font(.system(size: 12)).foregroundStyle(DeskColor.muted).fixedSize(horizontal: false, vertical: true)
            }
            Spacer()
        }.padding(20).deskCard()
    }
}

// A three-bay NAS, drawn in the interface accent colours. Remains crisp at Retina scaling.
struct VaultIllustration: View {
    var online: Bool
    var body: some View {
        Canvas { ctx, size in
            let scale = min(size.width / 240, size.height / 145)
            ctx.translateBy(x: (size.width - 240 * scale) / 2, y: (size.height - 145 * scale) / 2)
            ctx.scaleBy(x: scale, y: scale)
            func pts(_ points: [CGPoint], close: Bool = true) -> Path {
                Path { p in
                    guard let first = points.first else { return }
                    p.move(to: first)
                    for pt in points.dropFirst() { p.addLine(to: pt) }
                    if close { p.closeSubpath() }
                }
            }
            func polygon(_ xy: [(Int,Int)], _ color: Color, _ line: Color) {
                let p = pts(xy.map { CGPoint(x: $0.0, y: $0.1) })
                ctx.fill(p, with: .color(color))
                ctx.stroke(p, with: .color(line), style: StrokeStyle(lineWidth: 1.2, lineJoin: .round))
            }
            let wire = Color(red: 0.88, green: 0.68, blue: 0.73)
            let shell = Color(red: 0.34, green: 0.25, blue: 0.32)
            let side = Color(red: 0.25, green: 0.18, blue: 0.24)
            for i in 0..<5 {
                let x = 20 + i * 36
                let trace = pts([CGPoint(x:x,y:105),CGPoint(x:x+57,y:136),CGPoint(x:x+99,y:113)],close:false)
                ctx.stroke(trace, with: .color(wire.opacity(0.18)), style: StrokeStyle(lineWidth: 0.8, dash: [3,4]))
            }
            // The front and right faces share a single baseline.
            polygon([(40,39),(99,11),(197,37),(138,67)], Color(red: 0.44,green: 0.33,blue: 0.40),wire)
            polygon([(138,67),(197,37),(197,101),(138,133)],side,wire)
            polygon([(40,39),(138,67),(138,133),(40,103)],shell,wire)
            for x in [49,72,95] {
                let rise = Int(Double(x-40)*0.29)
                polygon([(x,46+rise),(x+17,51+rise),(x+17,99+rise),(x,94+rise)],side,wire.opacity(0.72))
                let seam = pts([CGPoint(x:x+5,y:56+rise),CGPoint(x:x+5,y:85+rise)],close:false)
                ctx.stroke(seam, with: .color(wire.opacity(0.70)), lineWidth:1.6)
            }
            for i in 0..<3 {
                let y = 90 + i * 7
                ctx.fill(Path(ellipseIn: CGRect(x:122,y:Double(y),width:3.6,height:3.6)), with:.color(online ? Color.mint : wire))
            }
            for i in 0..<4 {
                let x = 151 + i * 6
                let line = pts([CGPoint(x:x,y:83-i*3),CGPoint(x:x,y:106-i*3)], close:false)
                ctx.stroke(line, with:.color(wire.opacity(0.42)),lineWidth:1.3)
            }
        }.allowsHitTesting(false).accessibilityHidden(true)
    }
}

func desktopTime(_ raw: String?) -> String {
    guard let raw else { return "" }
    let precise = ISO8601DateFormatter()
    precise.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    guard let date = precise.date(from: raw) ?? ISO8601DateFormatter().date(from: raw) else { return raw }
    return date.formatted(.dateTime.month().day().hour().minute().locale(Locale(identifier: "zh_CN")))
}
