import AppKit

@MainActor
final class EngineService {
    let stateDirectory: URL
    let socket: URL
    let engine: URL
    let gh: URL
    let isIsolated: Bool
    private var process: Process?
    private var outputHandle: FileHandle?
    private let bundle: Bundle

    init(bundle: Bundle = .main) {
        self.bundle = bundle
        let environment = ProcessInfo.processInfo.environment
        let defaultDirectory = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("RepoReach", isDirectory: true)
        if let path = environment["REPOREACH_STATE_DIR"], path.hasPrefix("/") {
            stateDirectory = URL(fileURLWithPath: path, isDirectory: true)
            isIsolated = true
        } else {
            stateDirectory = defaultDirectory
            isIsolated = false
        }
        socket = stateDirectory.appendingPathComponent("engine.sock")
        let helpers = bundle.bundleURL.appendingPathComponent("Contents/Helpers", isDirectory: true)
        engine = URL(fileURLWithPath: environment["REPOREACH_ENGINE_PATH"] ?? helpers.appendingPathComponent("artifact-fs").path)
        gh = URL(fileURLWithPath: environment["REPOREACH_GH_PATH"] ?? helpers.appendingPathComponent("gh").path)
    }

    var client: EngineClient { EngineClient(executable: engine, socket: socket) }
    var isRunning: Bool { process?.isRunning == true }

    func start(mountRoot: String) throws {
        if process?.isRunning == true { return }
        guard FileManager.default.isExecutableFile(atPath: engine.path), FileManager.default.isExecutableFile(atPath: gh.path) else {
            throw EngineFailure(message: "The bundled repository tools are missing. Download the complete EnoughRepos app, or see the source build instructions.")
        }
        var arguments = ["desktop", "serve", "--state-dir", stateDirectory.path, "--mount-root", mountRoot, "--socket", socket.path, "--gh", gh.path]
        #if REPOREACH_NATIVE_FSKIT
        let bridgeContainer = try FSBridgeContainer.resolve(bundle: bundle)
        arguments += ["--fskit-socket-dir", bridgeContainer.directory.path]
        #endif
        try FileManager.default.createDirectory(at: stateDirectory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let log = stateDirectory.appendingPathComponent("service.log")
        if let size = (try? FileManager.default.attributesOfItem(atPath: log.path)[.size]) as? NSNumber, size.intValue > 1_048_576 {
            try? FileManager.default.removeItem(at: log)
        }
        if !FileManager.default.fileExists(atPath: log.path) { FileManager.default.createFile(atPath: log.path, contents: nil, attributes: [.posixPermissions: 0o600]) }
        outputHandle = try FileHandle(forWritingTo: log)
        try outputHandle?.seekToEnd()
        let service = Process()
        service.executableURL = engine
        service.arguments = arguments
        service.environment = ProcessInfo.processInfo.environment
        service.standardOutput = outputHandle; service.standardError = outputHandle
        try service.run()
        process = service
    }

    func stop() {
        if let process, process.isRunning { process.terminate() }
        process = nil
        try? outputHandle?.close()
        outputHandle = nil
    }
}
