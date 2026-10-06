import Foundation
import Darwin

/// Each operation owns a connection. Cancelling an operation shuts down that
/// connection, which cancels the Go request context without replaying writes.
final class FSBridgeClient: @unchecked Sendable {
    static let maximumChunkSize = 1_048_576
    static let maximumXattrSize = 1_048_576
    static let maximumXattrNameSize = 127
    static let maximumXattrsPerItem = 128
    private static let maximumMetadataSize = 4 * 1_048_576
    private enum ConfigurationSource: Sendable {
        case file(URL, FSBridgeContainer?)
        case explicit(FSBridgeConfiguration)
    }
    private let source: ConfigurationSource
    private let transport: FSBridgeTransport

    init(configURL: URL, container: FSBridgeContainer? = nil) throws {
        source = .file(configURL, container)
        transport = FSBridgeTransport(configuration: try FSBridgeConfiguration.load(from: configURL, container: container))
    }

    init(configuration: FSBridgeConfiguration) {
        source = .explicit(configuration)
        transport = FSBridgeTransport(configuration: configuration)
    }

    /// A remount may follow a service restart and capability rotation. Keep the
    /// old client closed while constructing an independent connection from the
    /// current descriptor. The caller continues holding the resource scope.
    func reconnected() throws -> FSBridgeClient {
        switch source {
        case .file(let url, let container): return try FSBridgeClient(configURL: url, container: container)
        case .explicit(let configuration): return FSBridgeClient(configuration: configuration)
        }
    }

    func request(_ request: FSBridgeRequest, timeout: TimeInterval = 120) async throws -> FSBridgeResponse {
        try await self.request(request, maximumResponse: Self.maximumMetadataSize, timeout: timeout)
    }

    private func request(_ request: FSBridgeRequest, maximumResponse: Int,
                         timeout: TimeInterval) async throws -> FSBridgeResponse {
        let body: Data
        do { body = try JSONEncoder().encode(request) }
        catch { throw FSBridgeError.invalidRequest }
        guard body.count <= 65_536 else { throw FSBridgeError.invalidRequest }
        let response = try await transport.send(method: "POST", path: "/v1/fs", body: body,
                                                contentType: "application/json",
                                                maximumResponse: maximumResponse, timeout: timeout)
        return try Self.decode(response, xattr: request.op == "listxattr" || request.op == "removexattr")
    }

    func read(inode: UInt64, handle: UInt64, offset: UInt64, size: Int,
              timeout: TimeInterval = 120) async throws -> Data {
        try Self.validateChunk(offset: offset, size: size)
        let path = "/v1/fs/read?inode=\(inode)&handle=\(handle)&offset=\(offset)&size=\(size)"
        let response = try await transport.send(method: "GET", path: path, body: Data(),
                                                contentType: "application/octet-stream",
                                                maximumResponse: max(size, 65_536), timeout: timeout)
        guard response.status == 200 else { _ = try Self.decode(response); throw FSBridgeError.malformedResponse }
        guard response.headers["x-reporeach-errno"] == "0",
              response.headers["content-type"]?.lowercased().split(separator: ";").first == "application/octet-stream",
              response.contentLength != nil, response.body.count <= size else {
            throw FSBridgeError.malformedResponse
        }
        return response.body
    }

    func write(inode: UInt64, handle: UInt64, offset: UInt64, data: Data,
               timeout: TimeInterval = 120) async throws -> Int {
        try Self.validateChunk(offset: offset, size: data.count)
        let path = "/v1/fs/write?inode=\(inode)&handle=\(handle)&offset=\(offset)"
        let response = try await transport.send(method: "PUT", path: path, body: data,
                                                contentType: "application/octet-stream",
                                                maximumResponse: 65_536, timeout: timeout)
        let decoded = try Self.decode(response)
        // Go omits a zero written count. A successful empty write is still
        // validated by the service, including inode and handle permissions.
        let written = decoded.written ?? (data.isEmpty ? 0 : -1)
        guard written >= 0, written <= data.count else {
            throw FSBridgeError.malformedResponse
        }
        return written
    }

    func getXattr(inode: UInt64, name: String, timeout: TimeInterval = 120) async throws -> Data {
        let path = try Self.xattrPath(inode: inode, name: name)
        let response = try await transport.send(method: "GET", path: path, body: Data(),
                                                contentType: "application/octet-stream",
                                                maximumResponse: Self.maximumXattrSize, timeout: timeout)
        guard response.status == 200 else {
            _ = try Self.decode(response, xattr: true)
            throw FSBridgeError.malformedResponse
        }
        guard response.headers["x-reporeach-errno"] == "0",
              response.headers["content-type"]?.lowercased().split(separator: ";").first == "application/octet-stream",
              response.contentLength != nil, response.body.count <= Self.maximumXattrSize else {
            throw FSBridgeError.malformedResponse
        }
        return response.body
    }

    func setXattr(inode: UInt64, name: String, value: Data, policy: FSBridgeXattrPolicy,
                  timeout: TimeInterval = 120) async throws {
        guard value.count <= Self.maximumXattrSize else { throw FSBridgeError.invalidRequest }
        let path = try Self.xattrPath(inode: inode, name: name) + "&policy=" + policy.rawValue
        let response = try await transport.send(method: "PUT", path: path, body: value,
                                                contentType: "application/octet-stream",
                                                maximumResponse: 65_536, timeout: timeout)
        let decoded = try Self.decode(response, xattr: true)
        guard decoded.written == value.count else {
            throw FSBridgeError.malformedResponse
        }
    }

    func listXattrs(inode: UInt64, timeout: TimeInterval = 120) async throws -> [String] {
        guard inode > 0 else { throw FSBridgeError.invalidRequest }
        let response = try await request(FSBridgeRequest(op: "listxattr", inode: inode),
                                         maximumResponse: Self.maximumMetadataSize, timeout: timeout)
        guard let names = response.xattrNames, names.count <= Self.maximumXattrsPerItem,
              Set(names.map { Data($0.utf8) }).count == names.count,
              names.allSatisfy(Self.validXattrName) else { throw FSBridgeError.malformedResponse }
        return names
    }

    func removeXattr(inode: UInt64, name: String, timeout: TimeInterval = 120) async throws {
        guard inode > 0, Self.validXattrName(name) else { throw FSBridgeError.invalidRequest }
        _ = try await request(FSBridgeRequest(op: "removexattr", inode: inode, name: name),
                              maximumResponse: 65_536, timeout: timeout)
    }

    static func validXattrName(_ name: String) -> Bool {
        !name.isEmpty && name.utf8.count <= maximumXattrNameSize && !name.utf8.contains(0)
    }

    private static func xattrPath(inode: UInt64, name: String) throws -> String {
        guard inode > 0, validXattrName(name) else { throw FSBridgeError.invalidRequest }
        // Attribute names are not paths: slash, controls and query delimiters
        // remain legal names, and must be encoded rather than normalized.
        let encoded = name.utf8.map { byte -> String in
            if (65...90).contains(byte) || (97...122).contains(byte) || (48...57).contains(byte) ||
                [45, 46, 95, 126].contains(byte) { return String(UnicodeScalar(byte)) }
            return String(format: "%%%02X", byte)
        }.joined()
        return "/v1/fs/xattr?inode=\(inode)&name=\(encoded)"
    }

    /// Call only after preventing new volume operations. This permanently
    /// closes this client and waits for cancellation of its outstanding calls.
    func close() async { await transport.close() }

    private static func validateChunk(offset: UInt64, size: Int) throws {
        guard size >= 0, size <= maximumChunkSize,
              offset <= UInt64(Int64.max), UInt64(size) <= UInt64(Int64.max) - offset else {
            throw FSBridgeError.invalidRequest
        }
    }

    private static func decode(_ response: FSBridgeHTTPResponse, xattr: Bool = false) throws -> FSBridgeResponse {
        guard let decoded = try? JSONDecoder().decode(FSBridgeResponse.self, from: response.body),
              decoded.version == 1, decoded.errno >= 0 else { throw FSBridgeError.malformedResponse }
        if let header = response.headers["x-reporeach-errno"], header != String(decoded.errno) {
            throw FSBridgeError.malformedResponse
        }
        if decoded.errno != 0 {
            // Linux ENODATA and Darwin ENOATTR have different numeric values.
            // The authenticated operation-specific tag supplies that meaning.
            throw FSBridgeError.filesystem(xattr && decoded.xattrMissing == true ? ENOATTR : decoded.errno)
        }
        guard decoded.xattrMissing != true else { throw FSBridgeError.malformedResponse }
        guard response.status == 200 else { throw FSBridgeError.malformedResponse }
        return decoded
    }
}
