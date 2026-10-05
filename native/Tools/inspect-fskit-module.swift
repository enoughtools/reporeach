import Darwin
import Foundation
import FSKit

/// Read-only acceptance preflight. The system mount tool selects a filesystem
/// by short name, so checking one app's plist cannot identify its actual module.
private struct ModuleCandidate: Encodable, Sendable {
    let moduleID: String
    let modulePath: String
}

private struct InstalledModule: Encodable, Sendable {
    let moduleID: String
    let modulePath: String
    let enabled: Bool
}

private struct ObservedModule: Sendable {
    let moduleID: String
    let url: URL
    let enabled: Bool
}

private struct UnknownModule: Encodable, Sendable {
    let moduleID: String
    let modulePath: String
    let reason: String
}

private struct Inspection: Encodable, Sendable {
    var ok = false
    var expectedModuleID = ""
    var expectedModulePath = ""
    let shortName = "reporeach"
    var installedCount = 0
    var installedModules: [InstalledModule] = []
    var candidateCount = 0
    var candidates: [ModuleCandidate] = []
    var unknowns: [UnknownModule] = []
    var message = "inspection did not complete"
    var outputTruncated = false
}

/// A framework callback or filesystem read must not retain this helper forever.
/// Only one result is written, including when the timeout races the callback.
private final class Reporter: @unchecked Sendable {
    private let lock = NSLock()
    private var finished = false

    func finish(_ inspection: Inspection) {
        lock.lock()
        guard !finished else { lock.unlock(); return }
        finished = true
        lock.unlock()
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        encoder.outputFormatting = [.sortedKeys]
        var result = inspection
        var data = (try? encoder.encode(result)) ?? Data("{\"ok\":false,\"message\":\"JSON encoding failed\"}".utf8)
        if data.count > 64 * 1024 {
            result.ok = false
            result.message = "inspection output exceeded its bound"
            result.outputTruncated = true
            result.installedModules = Array(result.installedModules.prefix(8)).map {
                InstalledModule(moduleID: bounded($0.moduleID, bytes: 255), modulePath: bounded($0.modulePath, bytes: 1024), enabled: $0.enabled)
            }
            result.candidates = Array(result.candidates.prefix(8)).map {
                ModuleCandidate(moduleID: bounded($0.moduleID, bytes: 255), modulePath: bounded($0.modulePath, bytes: 1024))
            }
            result.unknowns = Array(result.unknowns.prefix(8)).map {
                UnknownModule(moduleID: bounded($0.moduleID, bytes: 255), modulePath: bounded($0.modulePath, bytes: 1024), reason: $0.reason)
            }
            data = (try? encoder.encode(result)) ?? Data("{\"ok\":false,\"message\":\"JSON encoding failed\"}".utf8)
        }
        data.append(0x0a)
        FileHandle.standardOutput.write(data)
        exit(result.ok ? 0 : 1)
    }
}

private func bounded(_ text: String, bytes: Int) -> String {
    String(decoding: text.utf8.prefix(bytes), as: UTF8.self)
}

private enum MetadataFailure: String, Error {
    case invalidURL = "module URL is not an absolute file URL"
    case unavailable = "module metadata is unavailable"
    case oversized = "module metadata exceeds its bound"
    case invalid = "module metadata is invalid"
    case identity = "module metadata identifier differs from FSClient"
    case extensionPoint = "module extension point is unknown"
    case shortName = "module filesystem short name is unknown"
}

@main
private struct InspectFSKitModule {
    static func main() {
        let reporter = Reporter()
        var initial = Inspection()
        if CommandLine.arguments == [CommandLine.arguments[0], "--self-test"] {
            do {
                let count = try selfTest()
                initial.ok = true
                initial.message = "\(count) pure inventory and metadata validation scenarios passed; no FSClient query or mount occurred"
            } catch {
                initial.message = "pure inventory validation failed: \(error)"
            }
            reporter.finish(initial)
            return
        }
        guard CommandLine.arguments.count == 3 else {
            initial.message = "pass the expected module identifier and its absolute module path"
            reporter.finish(initial)
            return
        }
        let identifier = CommandLine.arguments[1]
        let path = CommandLine.arguments[2]
        guard ["com.enoughtools.reporeach.fskit", "com.enoughtools.reporeach.validation.fskit"].contains(identifier),
              path.utf8.count <= 4096, path.hasPrefix("/"), !path.contains("\0") else {
            initial.message = "expected module identifier or path is invalid"
            reporter.finish(initial)
            return
        }
        initial.expectedModuleID = identifier
        initial.expectedModulePath = URL(fileURLWithPath: path).standardizedFileURL.resolvingSymlinksInPath().path
        guard #available(macOS 15.4, *) else {
            initial.message = "public FSKit discovery requires macOS 15.4 or later"
            reporter.finish(initial)
            return
        }
        let expected = initial
        DispatchQueue.global().asyncAfter(deadline: .now() + 15) {
            var timeout = expected
            timeout.message = "public FSKit discovery timed out"
            reporter.finish(timeout)
        }
        FSClient.shared.fetchInstalledExtensions { modules, error in
            guard error == nil, let modules else {
                var failure = expected
                failure.message = "public FSKit discovery failed"
                reporter.finish(failure)
                return
            }
            let observed = modules.map {
                ObservedModule(moduleID: $0.bundleIdentifier, url: $0.url, enabled: $0.isEnabled)
            }
            reporter.finish(inspect(modules: observed, expected: expected))
        }
        RunLoop.main.run()
    }

    private static func inspect(modules: [ObservedModule], expected: Inspection,
                                metadataReader: (URL) throws -> [String: Any] = readMetadata) -> Inspection {
        var result = expected
        result.installedCount = modules.count
        guard modules.count <= 64 else {
            result.message = "public FSKit module inventory exceeds its bound"
            return result
        }
        result.installedModules = modules.map {
            InstalledModule(moduleID: bounded($0.moduleID, bytes: 255), modulePath: bounded($0.url.path, bytes: 4096), enabled: $0.enabled)
        }
        for module in modules where module.enabled {
            let identifier = module.moduleID
            let url = module.url
            let displayedPath = bounded(url.path, bytes: 4096)
            do {
                guard identifier.utf8.count <= 255, !identifier.isEmpty,
                      url.isFileURL, url.path.hasPrefix("/"), url.path.utf8.count <= 4096,
                      url.host == nil || url.host == "" || url.host == "localhost" else {
                    throw MetadataFailure.invalidURL
                }
                let canonical = url.standardizedFileURL.resolvingSymlinksInPath()
                let metadata = try metadataReader(canonical.appendingPathComponent("Contents/Info.plist"))
                guard metadata["CFBundleIdentifier"] as? String == identifier else { throw MetadataFailure.identity }
                guard let attributes = metadata["EXAppExtensionAttributes"] as? [String: Any],
                      attributes["EXExtensionPointIdentifier"] as? String == "com.apple.fskit.fsmodule" else {
                    throw MetadataFailure.extensionPoint
                }
                guard let shortName = attributes["FSShortName"] as? String,
                      !shortName.isEmpty, shortName.utf8.count <= 255 else { throw MetadataFailure.shortName }
                if shortName == result.shortName {
                    result.candidates.append(ModuleCandidate(moduleID: identifier, modulePath: canonical.path))
                }
            } catch {
                let reason = (error as? MetadataFailure)?.rawValue ?? "module metadata could not be inspected"
                result.unknowns.append(UnknownModule(moduleID: bounded(identifier, bytes: 255), modulePath: displayedPath, reason: reason))
            }
        }
        result.candidates.sort { ($0.moduleID, $0.modulePath) < ($1.moduleID, $1.modulePath) }
        result.candidateCount = result.candidates.count
        guard result.unknowns.isEmpty else {
            result.message = "enabled modules have unreadable or unknown metadata; selection cannot be established"
            return result
        }
        guard result.candidates.count == 1 else {
            result.message = "reporeach must have exactly one enabled installed filesystem module"
            return result
        }
        guard result.candidates[0].moduleID == result.expectedModuleID,
              result.candidates[0].modulePath == result.expectedModulePath else {
            result.message = "the enabled reporeach module differs from the selected identifier or exact module path"
            return result
        }
        result.ok = true
        result.message = "the selected module is the sole enabled reporeach filesystem"
        return result
    }

    /// Fixtures exercise the actual selection function. This branch never asks
    /// FSClient about the host or opens a bundle, and cannot satisfy normal CLI
    /// inspection because it has no selected module identifier or path.
    private static func selfTest() throws -> Int {
        let identifier = "com.enoughtools.reporeach.validation.fskit"
        let url = URL(fileURLWithPath: "/fixture/RepoReach.app/Contents/Extensions/RepoReachFSKit.appex")
        var expected = Inspection()
        expected.expectedModuleID = identifier
        expected.expectedModulePath = url.path
        let selected = ObservedModule(moduleID: identifier, url: url, enabled: true)
        let other = ObservedModule(moduleID: "com.enoughtools.reporeach.fskit", url: URL(fileURLWithPath: "/fixture/Other.appex"), enabled: true)
        func metadata(id: String = identifier, name: String = "reporeach") -> [String: Any] {
            ["CFBundleIdentifier": id, "EXAppExtensionAttributes": [
                "EXExtensionPointIdentifier": "com.apple.fskit.fsmodule", "FSShortName": name,
            ]]
        }
        struct Case {
            let name: String
            let modules: [ObservedModule]
            let read: (URL) throws -> [String: Any]
            let ok: Bool
            let candidates: Int
            let unknowns: Int
        }
        let good: (URL) throws -> [String: Any] = { _ in metadata() }
        let both: (URL) throws -> [String: Any] = { file in metadata(id: file.path.hasPrefix(other.url.path + "/") ? other.moduleID : identifier) }
        let cases: [Case] = [
            Case(name: "matching module", modules: [selected], read: good, ok: true, candidates: 1, unknowns: 0),
            Case(name: "module absent", modules: [], read: good, ok: false, candidates: 0, unknowns: 0),
            Case(name: "module disabled", modules: [ObservedModule(moduleID: identifier, url: url, enabled: false)], read: good, ok: false, candidates: 0, unknowns: 0),
            Case(name: "same short name different identities", modules: [selected, other], read: both, ok: false, candidates: 2, unknowns: 0),
            Case(name: "duplicate registration", modules: [selected, selected], read: good, ok: false, candidates: 2, unknowns: 0),
            Case(name: "same identity different path", modules: [ObservedModule(moduleID: identifier, url: other.url, enabled: true)], read: good, ok: false, candidates: 1, unknowns: 0),
            Case(name: "sole module wrong identity", modules: [other], read: both, ok: false, candidates: 1, unknowns: 0),
            Case(name: "unrelated known filesystem", modules: [selected, other], read: { file in metadata(id: file.path.hasPrefix(other.url.path + "/") ? other.moduleID : identifier, name: file.path.hasPrefix(other.url.path + "/") ? "otherfs" : "reporeach") }, ok: true, candidates: 1, unknowns: 0),
            Case(name: "unreadable metadata", modules: [selected], read: { _ in throw MetadataFailure.unavailable }, ok: false, candidates: 0, unknowns: 1),
            Case(name: "mismatched plist identity", modules: [selected], read: { _ in metadata(id: other.moduleID) }, ok: false, candidates: 0, unknowns: 1),
            Case(name: "unknown extension metadata", modules: [selected], read: { _ in ["CFBundleIdentifier": identifier] }, ok: false, candidates: 0, unknowns: 1),
            Case(name: "missing short name", modules: [selected], read: { _ in ["CFBundleIdentifier": identifier, "EXAppExtensionAttributes": ["EXExtensionPointIdentifier": "com.apple.fskit.fsmodule"]] }, ok: false, candidates: 0, unknowns: 1),
            Case(name: "empty short name", modules: [selected], read: { _ in metadata(name: "") }, ok: false, candidates: 0, unknowns: 1),
            Case(name: "unknown competing module", modules: [selected, other], read: { file in if file.path.hasPrefix(other.url.path + "/") { throw MetadataFailure.unavailable }; return metadata() }, ok: false, candidates: 1, unknowns: 1),
            Case(name: "non-file URL", modules: [ObservedModule(moduleID: identifier, url: URL(string: "https://example.invalid/module")!, enabled: true)], read: good, ok: false, candidates: 0, unknowns: 1),
            Case(name: "bounded inventory", modules: Array(repeating: selected, count: 65), read: good, ok: false, candidates: 0, unknowns: 0),
        ]
        for test in cases {
            let result = inspect(modules: test.modules, expected: expected, metadataReader: test.read)
            guard result.ok == test.ok, result.candidateCount == test.candidates, result.unknowns.count == test.unknowns else {
                throw NSError(domain: "RepoReachInspectorSelfTest", code: 1, userInfo: [NSLocalizedDescriptionKey: test.name])
            }
        }
        return cases.count
    }

    private static func readMetadata(_ url: URL) throws -> [String: Any] {
        let descriptor = open(url.path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK)
        guard descriptor >= 0 else { throw MetadataFailure.unavailable }
        let file = FileHandle(fileDescriptor: descriptor, closeOnDealloc: true)
        defer { try? file.close() }
        var info = stat()
        guard fstat(descriptor, &info) == 0, info.st_mode & S_IFMT == S_IFREG else { throw MetadataFailure.unavailable }
        let limit = 1024 * 1024
        guard info.st_size >= 0, info.st_size <= limit else { throw MetadataFailure.oversized }
        let data = try file.read(upToCount: limit + 1) ?? Data()
        guard data.count <= limit else { throw MetadataFailure.oversized }
        guard let metadata = try PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any] else {
            throw MetadataFailure.invalid
        }
        return metadata
    }
}
