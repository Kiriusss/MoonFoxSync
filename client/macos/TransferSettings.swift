import SwiftUI

private struct FileTransferLimits: Decodable {
    let uploads: Int
    let downloads: Int
}

struct TransferSettingsCard: View {
    @State private var saved: FileTransferLimits?
    @State private var uploads = ""
    @State private var downloads = ""
    @State private var busy = false
    @State private var message = ""

    private func limit(_ text: String) -> Int? {
        guard !text.isEmpty, text.utf8.allSatisfy({ (48...57).contains($0) }),
              let n = Int(text), (1...50).contains(n) else { return nil }
        return n
    }

    private var valid: Bool { limit(uploads) != nil && limit(downloads) != nil }
    private var changed: Bool { limit(uploads) != saved?.uploads || limit(downloads) != saved?.downloads }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("项目内并行传输").font(.headline)
            Text("每个项目可同时上传或下载 1 至 50 个文件。新设置从下一次传输开始生效。")
                .font(.caption).foregroundStyle(.secondary)
            HStack {
                field("同时上传", $uploads)
                field("同时下载", $downloads)
                Spacer()
                Button("保存并发数") { Task { await update(save: true) } }
                    .disabled(saved == nil || !valid || !changed)
            }
            .disabled(busy || saved == nil)
            if saved != nil && !valid {
                Text("请输入 1 至 50 的整数。").font(.caption).foregroundStyle(.orange)
            } else if !message.isEmpty {
                Text(message).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            }
            if saved == nil && !busy {
                Button("重新读取并发数") { Task { await update(save: false) } }
            }
        }.padding(15).deskCard().task { await update(save: false) }
    }

    private func field(_ title: String, _ value: Binding<String>) -> some View {
        HStack(spacing: 6) {
            Text(title)
            TextField(title, text: value).labelsHidden().frame(width: 52)
                .textFieldStyle(.roundedBorder).accessibilityLabel(title + "文件数")
        }
    }

    @MainActor private func update(save: Bool) async {
        guard !busy else { return }
        var args = ["settings", "transfers"]
        if save {
            guard let saved, let up = limit(uploads), let down = limit(downloads) else { return }
            if up != saved.uploads { args += ["--uploads", String(up)] }
            if down != saved.downloads { args += ["--downloads", String(down)] }
        }
        busy = true
        defer { busy = false }
        do {
            let data = try await CLI.call(args)
            let values = try JSONDecoder().decode(FileTransferLimits.self, from: data)
            guard (1...50).contains(values.uploads), (1...50).contains(values.downloads)
            else { throw LocalError(message: "读取的并发数超出范围。") }
            saved = values
            uploads = String(values.uploads)
            downloads = String(values.downloads)
            message = save ? "已保存" : ""
        } catch { message = error.localizedDescription }
    }
}
