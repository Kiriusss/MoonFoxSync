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
    let path, snapshotId, operationId: String?
    let projectBytes, projectTotal, projectRate, projectFilesDone, projectFilesTotal: Int64?
    var activityID: String {
        if let operationId, !operationId.isEmpty { return operationId }
        if let snapshotId, !snapshotId.isEmpty { return snapshotId }
        return id
    }
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
        case ("finalizing", _): return direction == "upload" ? "正在提交" : "正在完成"
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

struct ProjectTransfer: Identifiable {
    let id: String
    let summary: LiveTransfer
    let files: [LiveTransfer]

    static func group(_ transfers: [LiveTransfer]) -> [Self] {
        let groups = Dictionary(grouping: transfers, by: { $0.direction + "/" + $0.unitId + "/" + $0.activityID })
        return groups.keys.sorted().compactMap { key in
            guard let rows = groups[key], let first = rows.first else { return nil }
            let summary = rows.first(where: { $0.phase == "finalizing" })
                ?? rows.first(where: { $0.projectTotal != nil }) ?? first
            return Self(id: key, summary: summary, files: rows.filter { !$0.file.isEmpty })
        }
    }
}

struct ProjectTransferView: View {
    let operation: ProjectTransfer
    let name: String
    var compact = false
    @State private var expanded = true

    var body: some View {
        let row = operation.summary
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Image(systemName: row.direction == "upload" ? "arrow.up" : "arrow.down")
                Text(name).fontWeight(.semibold).lineLimit(1)
                Spacer()
                Text(row.caption).font(.caption)
            }
            if row.direction == "upload" {
                if let total = row.projectTotal, let done = row.projectBytes {
                    let fraction = total > 0 ? min(1, Double(max(0, done)) / Double(total))
                        : (row.phase == "finalizing" ? 1.0 : 0.0)
                    ProgressView(value: fraction).accessibilityLabel("项目总进度")
                    HStack {
                        Text(LiveTransfer.bytes(done) + " / " + LiveTransfer.bytes(total))
                        if total > 0 { Text(fraction.formatted(.percent.precision(.fractionLength(0)))) }
                        Spacer()
                        if let rate = row.projectRate { Text(LiveTransfer.bytes(rate) + "/s") }
                    }.font(.caption.monospacedDigit())
                } else if row.phase != "finalizing" {
                    Text(row.phase == "preparing" ? "正在统计项目总量…" : "项目总量尚未提供")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            if row.direction == "download" && !operation.files.isEmpty {
                let rate = operation.files.reduce(Int64(0)) { $0 + max(0, $1.bytesPerSecond ?? 0) }
                Text("\(operation.files.count) 个文件 · " + LiveTransfer.bytes(rate) + "/s")
                    .font(.caption.monospacedDigit())
            }
            if !compact && !operation.files.isEmpty {
                DisclosureGroup("文件明细 · " + String(operation.files.count), isExpanded: $expanded) {
                    ForEach(operation.files) { file in
                        TransferDetails(transfer: file, project: "").padding(.vertical, 3)
                    }
                }.font(.caption)
            }
        }
    }
}
