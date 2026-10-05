import Foundation
import Darwin

/// Compiled alongside the production Bridge*.swift files, then invoked by
/// TestNativeClientInterop while the real Go server owns its private session.
@main
struct FSBridgeSmoke {
    static func main() async {
        do {
            guard CommandLine.arguments.count == 2 else { throw SmokeFailure("Pass the private connection descriptor") }
            try await run(descriptor: URL(fileURLWithPath: CommandLine.arguments[1]))
            print("PASS: native client and real Go filesystem bridge")
        } catch {
            fputs("FAIL: \(error.localizedDescription)\n", stderr)
            exit(1)
        }
    }

    static func run(descriptor: URL) async throws {
        let configuration = try FSBridgeConfiguration.load(from: descriptor)
        let client = FSBridgeClient(configuration: configuration)
        let root = try await client.request(FSBridgeRequest(op: "getattr", inode: 1))
        try expect(root.node?.inode == 1 && root.node?.attributes.type == .dir,
                   "root inode and directory attributes")
        try expect(root.node!.attributes.mode & UInt32(S_IFMT) == UInt32(S_IFDIR), "POSIX root mode")
        try emptyFields(root)

        let rootHandle = try requiredHandle(await client.request(FSBridgeRequest(op: "opendir", inode: 1)))
        let transport = FSBridgeTransport(configuration: configuration)
        let pageRequest = try JSONEncoder().encode(FSBridgeRequest(op: "readdir", inode: 1, handle: rootHandle, offset: 0))
        let framed = try await transport.send(method: "POST", path: "/v1/fs", body: pageRequest,
                                             contentType: "application/json", maximumResponse: 4 * 1_048_576, timeout: 5)
        try expect(framed.status == 200 && framed.body.count > 2048,
                   "real large metadata response crosses HTTP buffer boundary")
        try expect(framed.contentLength == framed.body.count || framed.headers["transfer-encoding"] == "chunked",
                   "real HTTP response has complete explicit framing")

        var offset: UInt64 = 0
        var names = Set<String>()
        var nonemptyPages = 0
        var reachedEOF = false
        for _ in 0..<10 {
            let page = try await client.request(FSBridgeRequest(op: "readdir", inode: 1, handle: rootHandle, offset: offset))
            guard let entries = page.entries, let eof = page.eof, let next = page.nextOffset else {
                throw SmokeFailure("directory wire fields must be present")
            }
            if eof {
                try expect(entries.isEmpty && next == offset, "EOF preserves the directory cookie")
                reachedEOF = true
                break
            }
            nonemptyPages += 1
            try expect(!entries.isEmpty && page.written == 0, "non-final page explicitly carries EOF=false and written=0")
            for entry in entries {
                try expect(entry.offset > offset && entry.node.inode != 0 && entry.node.attributes.type == .dir,
                           "directory entries carry increasing cookies and inode attributes")
                try expect(names.insert(entry.name).inserted, "directory pages contain no duplicates")
                offset = entry.offset
            }
            try expect(next == offset, "page continuation matches the last returned entry")
        }
        let expectedOwners = Set((0..<300).map { String(format: "owner-%03d-", $0) + String(repeating: "x", count: 200) } + ["interop"])
        try expect(reachedEOF && nonemptyPages >= 2 && names == expectedOwners,
                   "paged real catalogue returns every owner exactly once")
        _ = try await client.request(FSBridgeRequest(op: "releasedir", handle: rootHandle))

        let owner = try requiredNode(await client.request(FSBridgeRequest(op: "lookup", parent: 1, name: "interop")))
        let repo = try requiredNode(await client.request(FSBridgeRequest(op: "lookup", parent: owner.inode, name: "project")))
        let baseline = try requiredNode(await client.request(FSBridgeRequest(op: "lookup", parent: repo.inode, name: "README.md")))
        let baselineHandle = try requiredHandle(await client.request(FSBridgeRequest(op: "open", inode: baseline.inode)))
        let baselineBytes = try await client.read(inode: baseline.inode, handle: baselineHandle, offset: 0, size: 1024)
        try expect(baselineBytes == Data("interop/project".utf8) + Data([0, 255, 1, 254]),
                   "committed binary data passes through the engine's hydrator fixture")
        _ = try await client.request(FSBridgeRequest(op: "release", handle: baselineHandle))

        let created = try await client.request(FSBridgeRequest(op: "create", parent: repo.inode, name: "雪🧪.bin", mode: 0o640))
        let file = try requiredNode(created)
        let handle = try requiredHandle(created)
        let lookedUp = try requiredNode(await client.request(FSBridgeRequest(op: "lookup", parent: repo.inode, name: "雪🧪.bin")))
        try expect(lookedUp.inode == file.inode && lookedUp.attributes.type == .file, "UTF-8 creation and lookup identity")
        var binary = Data((0...255).map(UInt8.init)) + Data([0, 255, 254, 128])
        let count = try await client.write(inode: file.inode, handle: handle, offset: 0, data: binary)
        try expect(count == binary.count, "binary write reports the complete byte count")
        let patch = Data([255, 0, 128, 13, 10])
        try expect(try await client.write(inode: file.inode, handle: handle, offset: 13, data: patch) == patch.count,
                   "ranged binary write")
        binary.replaceSubrange(13..<18, with: patch)
        try expect(try await client.read(inode: file.inode, handle: handle, offset: 0, size: 1024) == binary,
                   "binary round trip preserves every byte")
        try expect(try await client.read(inode: file.inode, handle: handle, offset: 13, size: 5) == patch,
                   "ranged binary read")
        try expect(try await client.write(inode: file.inode, handle: handle, offset: 0, data: Data()) == 0,
                   "empty write reports zero")
        try expect(try await client.read(inode: file.inode, handle: handle, offset: 0, size: 0).isEmpty,
                   "empty binary read")
        _ = try await client.request(FSBridgeRequest(op: "fsync", inode: file.inode, handle: handle))

        let empty = try requiredNode(await client.request(FSBridgeRequest(op: "mkdir", parent: repo.inode, name: "empty", mode: 0o755)))
        let emptyHandle = try requiredHandle(await client.request(FSBridgeRequest(op: "opendir", inode: empty.inode)))
        let emptyPage = try await client.request(FSBridgeRequest(op: "readdir", inode: empty.inode, handle: emptyHandle, offset: 0))
        try expect(emptyPage.entries == [] && emptyPage.eof == true && emptyPage.nextOffset == 0 && emptyPage.written == 0,
                   "empty directory fields decode without omitted-zero ambiguity")
        _ = try await client.request(FSBridgeRequest(op: "releasedir", handle: emptyHandle))

        try await expectError(.filesystem(ENOENT)) {
            _ = try await client.request(FSBridgeRequest(op: "lookup", parent: repo.inode, name: "missing"))
        }
        try await expectError(.filesystem(EINVAL)) {
            _ = try await client.request(FSBridgeRequest(op: "lookup", parent: repo.inode, name: "../escape"))
        }
        try await expectError(.invalidRequest) {
            _ = try await client.read(inode: file.inode, handle: handle, offset: UInt64(Int64.max), size: 1)
        }
        try await expectError(.invalidRequest) {
            _ = try await client.read(inode: file.inode, handle: handle, offset: 0, size: FSBridgeClient.maximumChunkSize + 1)
        }
        for suffix in ["offset=-1&size=1", "offset=9223372036854775807&size=1", "offset=0&size=1048577", "offset=0&size=1&offset=1"] {
            let raw = try await transport.send(method: "GET", path: "/v1/fs/read?inode=\(file.inode)&handle=\(handle)&\(suffix)",
                                               body: Data(), contentType: "application/octet-stream", maximumResponse: 65_536, timeout: 5)
            let error = try JSONDecoder().decode(FSBridgeResponse.self, from: raw.body)
            try expect(raw.status != 200 && error.errno == EINVAL && raw.headers["x-reporeach-errno"] == String(EINVAL),
                       "real server rejects malformed or unbounded offsets")
        }
        let unauthenticated = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: configuration.socketPath,
                                                                                     token: String(repeating: "0", count: 64)))
        try await expectError(.filesystem(EACCES)) {
            _ = try await unauthenticated.request(FSBridgeRequest(op: "getattr", inode: 1))
        }
        await unauthenticated.close()
        _ = try await client.request(FSBridgeRequest(op: "release", handle: handle))
        try await expectError(.filesystem(EBADF)) {
            _ = try await client.read(inode: file.inode, handle: handle, offset: 0, size: 1)
        }
        await transport.close()
        await client.close()
    }

    static func requiredNode(_ response: FSBridgeResponse) throws -> FSBridgeNode {
        guard let node = response.node, node.inode != 0 else { throw SmokeFailure("response lacks inode metadata") }
        return node
    }

    static func requiredHandle(_ response: FSBridgeResponse) throws -> UInt64 {
        guard let handle = response.handle, handle != 0 else { throw SmokeFailure("response lacks an open handle") }
        return handle
    }

    static func emptyFields(_ response: FSBridgeResponse) throws {
        try expect(response.entries == [] && response.nextOffset == 0 && response.eof == false && response.written == 0,
                   "Go zero-valued fields decode explicitly in the actual Swift model")
    }

    static func expectError(_ expected: FSBridgeError, operation: () async throws -> Void) async throws {
        do { try await operation() }
        catch {
            try expect(error as? FSBridgeError == expected, "actual client propagates the expected filesystem error")
            return
        }
        throw SmokeFailure("expected filesystem operation to fail")
    }

    static func expect(_ condition: Bool, _ message: String) throws {
        guard condition else { throw SmokeFailure(message) }
    }

    struct SmokeFailure: LocalizedError {
        let message: String
        init(_ message: String) { self.message = message }
        var errorDescription: String? { message }
    }
}
