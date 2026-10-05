import Foundation
import Darwin

/// Each operation owns a connection. Cancelling an operation shuts down that
/// connection, which cancels the Go request context without replaying writes.
final class FSBridgeClient: @unchecked Sendable {
    static let maximumChunkSize = 1_048_576
    private static let maximumMetadataSize = 4 * 1_048_576
    private enum ConfigurationSource: Sendable {
        case file(URL)
        case explicit(FSBridgeConfiguration)
    }
    private let source: ConfigurationSource
    private let transport: FSBridgeTransport

    init(configURL: URL) throws {
        source = .file(configURL)
        transport = FSBridgeTransport(configuration: try FSBridgeConfiguration.load(from: configURL))
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
        case .file(let url): return try FSBridgeClient(configURL: url)
        case .explicit(let configuration): return FSBridgeClient(configuration: configuration)
        }
    }

    func request(_ request: FSBridgeRequest, timeout: TimeInterval = 120) async throws -> FSBridgeResponse {
        let body: Data
        do { body = try JSONEncoder().encode(request) }
        catch { throw FSBridgeError.invalidRequest }
        guard body.count <= 65_536 else { throw FSBridgeError.invalidRequest }
        let response = try await transport.send(method: "POST", path: "/v1/fs", body: body,
                                                contentType: "application/json",
                                                maximumResponse: Self.maximumMetadataSize, timeout: timeout)
        return try Self.decode(response)
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

    /// Call only after preventing new volume operations. This permanently
    /// closes this client and waits for cancellation of its outstanding calls.
    func close() async { await transport.close() }

    private static func validateChunk(offset: UInt64, size: Int) throws {
        guard size >= 0, size <= maximumChunkSize,
              offset <= UInt64(Int64.max), UInt64(size) <= UInt64(Int64.max) - offset else {
            throw FSBridgeError.invalidRequest
        }
    }

    private static func decode(_ response: FSBridgeHTTPResponse) throws -> FSBridgeResponse {
        guard let decoded = try? JSONDecoder().decode(FSBridgeResponse.self, from: response.body),
              decoded.version == 1, decoded.errno >= 0 else { throw FSBridgeError.malformedResponse }
        if let header = response.headers["x-reporeach-errno"], header != String(decoded.errno) {
            throw FSBridgeError.malformedResponse
        }
        if decoded.errno != 0 { throw FSBridgeError.filesystem(decoded.errno) }
        guard response.status == 200 else { throw FSBridgeError.malformedResponse }
        return decoded
    }
}
