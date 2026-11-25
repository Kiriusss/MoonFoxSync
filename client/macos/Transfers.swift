import AppKit
import SwiftUI

struct PendingSyncButton: View {
    @ObservedObject var model: LibraryModel
    let schedule: Schedule

    var body: some View {
        Button {
            model.syncPending(homeRouteArguments: schedule.homeRouteArguments)
        } label: {
            HStack(spacing: 5) {
                if model.syncingPending { ProgressView().controlSize(.mini).accessibilityHidden(true) }
                Text(model.syncingPending ? "同步中…" : "立即同步")
            }
        }
        .buttonStyle(.borderedProminent).tint(DeskColor.accent)
        .accessibilityLabel(model.syncingPending ? "同步中" : "立即同步")
        .disabled(model.syncingPending || model.pendingCount == 0)
        .help("立即上传当前队列中的所有待传输快照")
    }
}

struct LiveTransfer: Decodable, Equatable, Identifiable {
    let id, direction, unitId, phase: String
    let path: String?
    let fileBytes, fileTotal, bytesPerSecond: Int64?
    var file: String { path ?? "" }
    var done: Int64 { max(0, fileBytes ?? 0) }
    var total: Int64 { max(0, fileTotal ?? 0) }
    var rate: String {
        let speed = max(0, bytesPerSecond ?? 0)
        return speed == 0 ? "0 B/s" : Self.bytes(speed) + "/s"
    }
    var caption: String {
        switch (phase, direction) {
        case ("preparing", _): return "准备传输"
        case ("finalizing", _): return "正在完成"
        case (_, "upload"): return "正在上传"
        default: return "正在下载"
        }
    }
    static func bytes(_ value: Int64) -> String {
        value == 0 ? "0 B" : ByteCountFormatter.string(fromByteCount: value, countStyle: .file)
    }
}

struct TransferDetails: View {
    let transfer: LiveTransfer
    let project: String
    var compact = false

    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            HStack {
                Image(systemName: transfer.direction == "upload" ? "arrow.up" : "arrow.down").foregroundStyle(DeskColor.accent)
                Text(transfer.caption)
                if !compact { Text(project).foregroundStyle(.secondary).lineLimit(1) }
                Spacer()
                if transfer.phase == "transferring" {
                    Text(transfer.rate).monospacedDigit().fontWeight(.semibold)
                }
            }.font(.subheadline)
            if !transfer.file.isEmpty {
                Text(transfer.file).lineLimit(compact ? 1 : 2).truncationMode(.middle)
                    .font(.caption).help(transfer.file).textSelection(.enabled)
                if transfer.total > 0 {
                    let fraction = min(1, Double(transfer.done) / Double(transfer.total))
                    ProgressView(value: fraction).progressViewStyle(.linear)
                        .accessibilityLabel("当前文件进度")
                    HStack {
                        Text(LiveTransfer.bytes(transfer.done) + " / " + LiveTransfer.bytes(transfer.total))
                        Spacer()
                        Text(fraction.formatted(.percent.precision(.fractionLength(0))))
                    }.font(.caption2.monospacedDigit()).foregroundStyle(.secondary)
                } else { Text("0 B").font(.caption2).foregroundStyle(.secondary) }
            }
        }
    }
}
