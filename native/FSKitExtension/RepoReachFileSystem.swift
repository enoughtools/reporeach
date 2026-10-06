import CryptoKit
import Darwin
import Foundation
import FSKit
import OSLog

/// The resource is a private connection directory, never a repository checkout.
/// FSKit transports its security-scoped URL into this sandboxed extension.
@available(macOS 26.0, *)
final class RepoReachFileSystem: FSUnaryFileSystem, FSUnaryFileSystemOperations {
    private enum Lifecycle {
        case idle
        case loading(UUID)
        case loaded(FSPathURLResource, RepoReachVolume)
        case unloading
    }

    private let lifecycleLock = NSLock()
    private let logger = Logger(subsystem: "com.enoughtools.reporeach", category: "FSKitLifecycle")
    private var lifecycle: Lifecycle = .idle

    func probeResource(resource: FSResource, replyHandler: @escaping (FSProbeResult?, Error?) -> Void) {
        guard let pathResource = resource as? FSPathURLResource,
              pathResource.url.isFileURL else {
            replyHandler(nil, POSIXError(.ENODEV))
            return
        }
        guard pathResource.url.startAccessingSecurityScopedResource() else {
            replyHandler(nil, POSIXError(.EACCES))
            return
        }
        let client: FSBridgeClient
        do {
            client = try FSBridgeClient(configURL: pathResource.url.appendingPathComponent("connection.json"),
                                        container: FSBridgeContainer.resolve())
        } catch {
            pathResource.url.stopAccessingSecurityScopedResource()
            replyHandler(nil, Self.posixError(error))
            return
        }
        Task {
            let result: Result<FSProbeResult, Error>
            do {
                let response = try await client.request(FSBridgeRequest(op: "getattr", inode: 1))
                guard response.node?.inode == 1, response.node?.attributes.type == .dir else {
                    throw POSIXError(.ENODEV)
                }
                result = .success(FSProbeResult.usable(name: "RepoReach", containerID:
                    FSContainerIdentifier(uuid: Self.identifier(for: pathResource.url))))
            } catch {
                result = .failure(Self.posixError(error))
            }
            await client.close()
            pathResource.url.stopAccessingSecurityScopedResource()
            switch result {
            case .success(let probe): replyHandler(probe, nil)
            case .failure(let error): replyHandler(nil, error)
            }
        }
    }

    func loadResource(resource: FSResource, options: FSTaskOptions,
                      replyHandler: @escaping (FSVolume?, Error?) -> Void) {
        guard let pathResource = resource as? FSPathURLResource,
              pathResource.url.isFileURL else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        guard !options.taskOptions.contains(where: { $0 == "-f" || $0 == "--force" }) else {
            replyHandler(nil, POSIXError(.ENOTSUP))
            return
        }
        let reservation = UUID()
        guard reserveLoad(reservation) else {
            replyHandler(nil, POSIXError(.EBUSY))
            return
        }
        guard pathResource.url.startAccessingSecurityScopedResource() else {
            failLoad(reservation, error: POSIXError(.EACCES))
            replyHandler(nil, POSIXError(.EACCES))
            return
        }
        let client: FSBridgeClient
        do {
            client = try FSBridgeClient(configURL: pathResource.url.appendingPathComponent("connection.json"),
                                        container: FSBridgeContainer.resolve())
        } catch {
            pathResource.url.stopAccessingSecurityScopedResource()
            failLoad(reservation, error: Self.posixError(error))
            replyHandler(nil, Self.posixError(error))
            return
        }
        let explicitlyReadOnly = options.taskOptions.contains("--rdonly")
        let readOnly = !pathResource.isWritable || explicitlyReadOnly
        logger.notice("Load policy resourceWritable=\(pathResource.isWritable, privacy: .public) explicitlyReadOnly=\(explicitlyReadOnly, privacy: .public) selectedReadOnly=\(readOnly, privacy: .public)")
        Task {
            do {
                let response = try await client.request(FSBridgeRequest(op: "getattr", inode: 1))
                guard response.node?.inode == 1, response.node?.attributes.type == .dir else {
                    throw POSIXError(.ENODEV)
                }
                let volume = RepoReachVolume(client: client, identifier: Self.identifier(for: pathResource.url), readOnly: readOnly)
                finishLoad(reservation, resource: pathResource, volume: volume)
                replyHandler(volume, nil)
            } catch {
                await client.close()
                pathResource.url.stopAccessingSecurityScopedResource()
                failLoad(reservation, error: Self.posixError(error))
                replyHandler(nil, Self.posixError(error))
            }
        }
    }

    func unloadResource(resource: FSResource, options: FSTaskOptions,
                        replyHandler: @escaping (Error?) -> Void) {
        guard let pathResource = resource as? FSPathURLResource,
              let loaded = beginUnload(pathResource.url) else {
            replyHandler(POSIXError(.EINVAL))
            return
        }
        Task {
            // Keep sandbox access valid while outstanding operations are drained.
            await loaded.volume.shutdown()
            loaded.resource.url.stopAccessingSecurityScopedResource()
            finishUnload()
            replyHandler(nil)
        }
    }

    private func reserveLoad(_ reservation: UUID) -> Bool {
        lifecycleLock.withLock {
            guard case .idle = lifecycle else { return false }
            lifecycle = .loading(reservation)
            return true
        }
    }

    private func finishLoad(_ reservation: UUID, resource: FSPathURLResource, volume: RepoReachVolume) {
        lifecycleLock.withLock {
            guard case .loading(let current) = lifecycle, current == reservation else { return }
            lifecycle = .loaded(resource, volume)
            containerStatus = .ready
        }
    }

    private func failLoad(_ reservation: UUID, error: Error) {
        lifecycleLock.withLock {
            guard case .loading(let current) = lifecycle, current == reservation else { return }
            lifecycle = .idle
            containerStatus = .notReady(status: error)
        }
    }

    private func beginUnload(_ url: URL) -> (resource: FSPathURLResource, volume: RepoReachVolume)? {
        lifecycleLock.withLock {
            guard case .loaded(let resource, let volume) = lifecycle, resource.url == url else { return nil }
            lifecycle = .unloading
            return (resource, volume)
        }
    }

    private func finishUnload() {
        lifecycleLock.withLock {
            lifecycle = .idle
            containerStatus = .notReady(status: POSIXError(.ENODEV))
        }
    }

    private static func identifier(for url: URL) -> UUID {
        var bytes = Array(SHA256.hash(data: Data(url.path.utf8)).prefix(16))
        bytes[6] = (bytes[6] & 0x0f) | 0x80 // A deterministic, custom UUID (version 8).
        bytes[8] = (bytes[8] & 0x3f) | 0x80
        return UUID(uuid: (bytes[0], bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7],
                           bytes[8], bytes[9], bytes[10], bytes[11], bytes[12], bytes[13], bytes[14], bytes[15]))
    }

    private static func posixError(_ error: Error) -> POSIXError {
        if let error = error as? POSIXError { return error }
        if let error = error as? FSBridgeError { return POSIXError(POSIXError.Code(rawValue: error.errno) ?? .EIO) }
        if error is FSBridgeContainer.Failure { return POSIXError(.EACCES) }
        return POSIXError(.EIO)
    }
}
