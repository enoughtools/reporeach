import Foundation
#if REPOREACH_NATIVE_FSKIT
import Darwin
import FSKit
#endif

enum FilesystemExtensionDiscovery {
    /// Read-only public inventory query. A delayed system callback must not
    /// leave setup controls permanently busy or resume the continuation twice.
    static func check(bundle: Bundle = .main) async -> FilesystemExtensionAvailability {
        #if REPOREACH_NATIVE_FSKIT
        guard #available(macOS 26, *) else { return .unsupported }
        let expected = bundle.bundleURL.appendingPathComponent("Contents/Extensions/RepoReachFSKit.appex", isDirectory: true)
        return await withCheckedContinuation { continuation in
            let result = ResultOnce(continuation)
            DispatchQueue.global().asyncAfter(deadline: .now() + 5) { result.finish(.unavailable) }
            FSClient.shared.fetchInstalledExtensions { modules, error in
                guard error == nil, let modules, modules.count <= 64 else { result.finish(.unavailable); return }
                let installed = modules.map {
                    FilesystemExtensionAvailability.InstalledModule(identifier: $0.bundleIdentifier, url: $0.url, enabled: $0.isEnabled,
                                                                    filesystemShortName: nil)
                }
                DispatchQueue.global(qos: .userInitiated).async {
                    let observed = installed.map {
                        FilesystemExtensionAvailability.InstalledModule(identifier: $0.identifier, url: $0.url, enabled: $0.enabled,
                            filesystemShortName: $0.enabled ? filesystemShortName(at: $0.url, identifier: $0.identifier) : nil)
                    }
                    result.finish(.assess(observed, expectedURL: expected))
                }
            }
        }
        #else
        return .unsupported
        #endif
    }

    #if REPOREACH_NATIVE_FSKIT
    /// Read installed extension metadata only, with the same bounded regular
    /// file discipline used by the release inspector. Unknown enabled modules
    /// cannot establish unambiguous filesystem selection.
    private static func filesystemShortName(at module: URL, identifier: String) -> String? {
        guard module.isFileURL, module.path.hasPrefix("/"), module.path.utf8.count <= 4096,
              module.host == nil || module.host == "" || module.host == "localhost",
              module.query == nil, module.fragment == nil else { return nil }
        let url = module.standardizedFileURL.resolvingSymlinksInPath().appendingPathComponent("Contents/Info.plist")
        let descriptor = open(url.path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK)
        guard descriptor >= 0 else { return nil }
        let handle = FileHandle(fileDescriptor: descriptor, closeOnDealloc: true)
        defer { try? handle.close() }
        var info = stat()
        let limit = 1024 * 1024
        guard fstat(descriptor, &info) == 0, info.st_mode & S_IFMT == S_IFREG,
              info.st_size >= 0, info.st_size <= limit,
              let data = try? handle.read(upToCount: limit + 1), data.count <= limit,
              let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
              plist["CFBundleIdentifier"] as? String == identifier,
              let attributes = plist["EXAppExtensionAttributes"] as? [String: Any],
              attributes["EXExtensionPointIdentifier"] as? String == "com.apple.fskit.fsmodule",
              let shortName = attributes["FSShortName"] as? String,
              !shortName.isEmpty, shortName.utf8.count <= 255 else { return nil }
        return shortName
    }
    #endif

    private final class ResultOnce: @unchecked Sendable {
        private let lock = NSLock()
        private var continuation: CheckedContinuation<FilesystemExtensionAvailability, Never>?

        init(_ continuation: CheckedContinuation<FilesystemExtensionAvailability, Never>) {
            self.continuation = continuation
        }

        func finish(_ value: FilesystemExtensionAvailability) {
            lock.lock()
            let pending = continuation
            continuation = nil
            lock.unlock()
            pending?.resume(returning: value)
        }
    }
}
