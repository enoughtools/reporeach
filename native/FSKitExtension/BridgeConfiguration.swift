import Foundation
import Darwin

/// The caller holds the resource folder's security scope for the volume's
/// lifetime. The shared secret is read from that folder, never from arguments
/// or the environment, and is used only in the HTTP Authorization header.
struct FSBridgeConfiguration: CustomDebugStringConvertible, Sendable {
    let socketPath: String
    fileprivate let token: String

    var debugDescription: String { "FSBridgeConfiguration(socket: \(socketPath), token: REDACTED)" }

    init(socketPath: String, token: String) throws {
        guard socketPath.hasPrefix("/"), !socketPath.utf8.contains(0),
              socketPath.utf8.count < MemoryLayout.size(ofValue: sockaddr_un().sun_path),
              token.utf8.count == 64,
              token.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }) else {
            throw FSBridgeError.invalidConfiguration
        }
        self.socketPath = socketPath
        self.token = token
    }

    static func load(from url: URL, container: FSBridgeContainer? = nil) throws -> FSBridgeConfiguration {
        guard url.isFileURL else { throw FSBridgeError.invalidConfiguration }
        let fd = Darwin.open(url.path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK)
        guard fd >= 0 else { throw FSBridgeError.invalidConfiguration }
        defer { Darwin.close(fd) }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_uid == getuid(),
              info.st_mode & mode_t(S_IFMT) == mode_t(S_IFREG),
              info.st_mode & 0o777 == 0o600,
              info.st_size > 0, info.st_size <= 16_384 else {
            throw FSBridgeError.invalidConfiguration
        }
        var bytes = Data(count: Int(info.st_size))
        let expectedSize = bytes.count
        var consumed = 0
        while consumed < expectedSize {
            let count = bytes.withUnsafeMutableBytes { buffer in
                Darwin.read(fd, buffer.baseAddress!.advanced(by: consumed), expectedSize - consumed)
            }
            if count < 0 && Darwin.errno == EINTR { continue }
            guard count > 0 else { throw FSBridgeError.invalidConfiguration }
            consumed += count
        }
        // Reject files that grew while being read rather than accepting a
        // truncated JSON document containing the original credentials.
        var extra: UInt8 = 0
        var extraCount: Int
        repeat { extraCount = Darwin.read(fd, &extra, 1) } while extraCount < 0 && Darwin.errno == EINTR
        guard extraCount == 0 else { throw FSBridgeError.invalidConfiguration }
        struct Document: Decodable { let version: Int; let socket: String; let token: String }
        guard let document = try? JSONDecoder().decode(Document.self, from: bytes), document.version == 1 else {
            throw FSBridgeError.invalidConfiguration
        }
        let configuration = try FSBridgeConfiguration(socketPath: document.socket, token: document.token)
        if let container {
            do { try container.validateSocket(path: configuration.socketPath) }
            catch { throw FSBridgeError.invalidConfiguration }
        }
        return configuration
    }

    var authorizationHeader: String { "Bearer \(token)" }
}
