import AppKit

// The bundled launch agent runs once per Aqua login. LaunchServices owns the
// GUI's lifetime, so stopping the agent does not terminate an open sync window.
@main
struct XinhuLogin {
    static func main() async throws {
        guard let executable = Bundle.main.executableURL else { throw CocoaError(.fileNoSuchFile) }
        let appURL = executable.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        guard let id = Bundle(url: appURL)?.bundleIdentifier else { throw CocoaError(.fileReadCorruptFile) }
        if !NSRunningApplication.runningApplications(withBundleIdentifier: id).isEmpty { return }
        let config = NSWorkspace.OpenConfiguration()
        config.activates = false
        config.addsToRecentItems = false
        config.arguments = ["--login-item"]
        _ = try await NSWorkspace.shared.openApplication(at: appURL, configuration: config)
    }
}
