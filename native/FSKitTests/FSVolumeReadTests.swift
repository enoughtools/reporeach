import XCTest
import Foundation
import FSKit
import Darwin
#if canImport(NativeFilesystem)
@testable import NativeFilesystem
#endif

/// These tests invoke the production volume with real FSItem identities. The
/// peer is a small HTTP service, so handle ownership is verified at the bridge
/// boundary without constructing framework-owned mutable buffers or mounting.
@available(macOS 15.4, *)
final class FSVolumeReadTests: XCTestCase {
    func testReadBeforeOpenUsesAndReleasesTemporaryReadHandle() async throws {
        let bytes = Data([0, 255, 128, 13, 10, 0, 97])
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(bytes) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let received = ReadCapture()

        let result = try await read(volume, item: item, offset: 19, length: bytes.count, capture: received)

        XCTAssertNil(result.error)
        XCTAssertEqual(result.count, bytes.count)
        XCTAssertEqual(received.data, bytes)
        XCTAssertEqual(received.positions, [0])
        let calls = fixture.requests.filter { ["open", "read", "release"].contains($0.operation) }
        XCTAssertEqual(calls.map(\.operation), ["open", "read", "release"])
        let opened = try XCTUnwrap(calls.first { $0.operation == "open" })
        let read = try XCTUnwrap(calls.first { $0.operation == "read" })
        let released = try XCTUnwrap(calls.first { $0.operation == "release" })
        XCTAssertEqual(opened.access, 1)
        XCTAssertEqual(read.offset, 19)
        XCTAssertEqual(read.size, bytes.count)
        XCTAssertEqual(read.handle, 101)
        XCTAssertEqual(released.handle, 101)
        try await shutdown(volume)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.count, 1)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testReadReusesRetainedReadHandleUntilClose() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([0, 255, 42])) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let openError = try await open(volume, item: item, modes: .read)
        XCTAssertNil(openError)

        for offset: off_t in [0, 8] {
            let result = try await read(volume, item: item, offset: offset, length: 3)
            XCTAssertEqual(result.count, 3)
            XCTAssertNil(result.error)
        }

        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.count, 1)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "read" }.map(\.handle), [101, 101])
        XCTAssertTrue(fixture.requests.filter { $0.operation == "release" }.isEmpty)
        let closeError = try await close(volume, item: item, modes: [])
        XCTAssertNil(closeError)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101])
        try await shutdown(volume)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.count, 1)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testTemporaryReadDoesNotChangeRetainedOpenModes() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([7])) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let initialRead = try await read(volume, item: item, length: 1)
        XCTAssertNil(initialRead.error)

        // Keeping a read mode requires a retained descriptor. A temporary read
        // must not grant one, and the later explicit open must reach the peer.
        assertPOSIX(try await close(volume, item: item, modes: .read), EBADF)
        let write = expectation(description: "Unopened write rejected")
        let writeResult = ReadReply()
        volume.write(contents: Data([1]), to: item, at: 0) { count, error in
            writeResult.store(count: count, error: error)
            write.fulfill()
        }
        await fulfillment(of: [write], timeout: 3)
        assertPOSIX(try XCTUnwrap(writeResult.value).error, EBADF)
        let openError = try await open(volume, item: item, modes: .read)
        XCTAssertNil(openError)
        let closeError = try await close(volume, item: item, modes: [])
        XCTAssertNil(closeError)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.map(\.access), [1, 1])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101, 102])
        XCTAssertTrue(fixture.requests.filter { $0.operation == "write" }.isEmpty)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testWriteOnlyOpenKeepsWriterWhileTemporaryReadUsesSeparateHandle() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([0, 255])) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let openError = try await open(volume, item: item, modes: .write)
        XCTAssertNil(openError)

        let result = try await read(volume, item: item, length: 2)

        XCTAssertEqual(result.count, 2)
        XCTAssertNil(result.error)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.map(\.access), [2, 1])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "read" }.map(\.handle), [102])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [102])
        let keepWriterError = try await close(volume, item: item, modes: .write)
        XCTAssertNil(keepWriterError)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [102])
        XCTAssertTrue(fixture.requests.filter { $0.operation == "flush" }.isEmpty)
        let finalCloseError = try await close(volume, item: item, modes: [])
        XCTAssertNil(finalCloseError)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "flush" }.map(\.handle), [101])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [102, 101])
        try await shutdown(volume)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.count, 2)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testPartialReadErrorPreservesCopiedCountAndReleasesHandle() async throws {
        let chunk = Data(repeating: 0x80, count: FSBridgeClient.maximumChunkSize)
        let fixture = try VolumeReadFixture(read: { request in
            request.offset == 0 ? VolumeReadFixture.binary(chunk) : VolumeReadFixture.error(EACCES)
        }, release: { attempt in attempt == 1 ? EBUSY : 0 })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let received = ReadCapture()

        let result = try await read(volume, item: item, length: chunk.count + 9, capture: received)

        XCTAssertEqual(result.count, chunk.count)
        assertPOSIX(result.error, EACCES)
        XCTAssertEqual(received.data, chunk)
        XCTAssertEqual(received.positions, [0])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "read" }.map(\.offset), [0, UInt64(chunk.count)])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "read" }.map(\.size), [chunk.count, 9])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101])
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testCancelledReadAwaitsTemporaryHandleReleaseBeforeReplyAndDrain() async throws {
        let waitingRead = expectation(description: "Read is awaiting peer bytes")
        let releaseStarted = expectation(description: "Cancelled read started independent release")
        let allowRelease = DispatchSemaphore(value: 0)
        let releaseAcknowledged = CompletionFlag()
        let drainCompleted = CompletionFlag()
        let fixture = try VolumeReadFixture(read: { _ in
            waitingRead.fulfill()
            return nil
        }, release: { _ in
            releaseStarted.fulfill()
            guard allowRelease.wait(timeout: .now() + 5) == .success else { return ETIMEDOUT }
            releaseAcknowledged.complete()
            return 0
        })
        defer { allowRelease.signal(); fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let readFinished = expectation(description: "Read callback completed after release")
        let shutdownFinished = expectation(description: "Shutdown drained cancelled read")
        let reply = ReadReply()
        volume.read(from: item, at: 0, length: 7, capacity: 7, consume: { _, _ in
            XCTFail("A stalled read must not produce bytes")
        }) { count, error in
            XCTAssertTrue(releaseAcknowledged.value, "Read replied before its temporary release completed")
            reply.store(count: count, error: error)
            readFinished.fulfill()
        }
        await fulfillment(of: [waitingRead], timeout: 3)
        Task {
            await volume.shutdown()
            XCTAssertTrue(releaseAcknowledged.value, "Drain finished before the temporary release completed")
            drainCompleted.complete()
            shutdownFinished.fulfill()
        }
        await fulfillment(of: [releaseStarted], timeout: 3)

        // The release response is deliberately withheld. Neither the kernel
        // reply nor session drain may finish while the handle is still owned.
        XCTAssertNil(reply.value)
        XCTAssertFalse(drainCompleted.value)
        allowRelease.signal()
        await fulfillment(of: [readFinished, shutdownFinished], timeout: 3)
        let result = try XCTUnwrap(reply.value)
        XCTAssertEqual(result.count, 0)
        assertPOSIX(result.error, EINTR)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101])
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testCancellationDuringTemporaryOpenWaitsForHandleAndReleasesIt() async throws {
        let allocated = expectation(description: "Peer allocated handle before acknowledging OPEN")
        let allowOpenResponse = DispatchSemaphore(value: 0)
        let openAcknowledged = CompletionFlag()
        let releaseAcknowledged = CompletionFlag()
        let drainCompleted = CompletionFlag()
        let fixture = try VolumeReadFixture(read: { _ in
            XCTFail("Cancellation during OPEN must prevent binary reads")
            return VolumeReadFixture.binary(Data())
        }, release: { _ in
            releaseAcknowledged.complete()
            return 0
        }, onOpen: { handle in
            XCTAssertEqual(handle, 101)
            allocated.fulfill()
            guard allowOpenResponse.wait(timeout: .now() + 5) == .success else {
                XCTFail("OPEN response barrier timed out")
                return
            }
            openAcknowledged.complete()
        })
        defer { allowOpenResponse.signal(); fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let readFinished = expectation(description: "Cancelled read replied after learning and releasing handle")
        let shutdownFinished = expectation(description: "Shutdown drained the pending OPEN")
        let reply = ReadReply()
        volume.read(from: item, at: 0, length: 7, capacity: 7, consume: { _, _ in
            XCTFail("Cancellation during OPEN must not deliver file bytes")
        }) { count, error in
            XCTAssertTrue(openAcknowledged.value, "Read replied before OPEN returned its allocated handle")
            XCTAssertTrue(releaseAcknowledged.value, "Read replied before releasing the newly learned handle")
            reply.store(count: count, error: error)
            readFinished.fulfill()
        }
        await fulfillment(of: [allocated], timeout: 3)
        Task {
            await volume.shutdown()
            XCTAssertTrue(openAcknowledged.value, "Drain finished while an allocated OPEN was unacknowledged")
            XCTAssertTrue(releaseAcknowledged.value, "Drain finished before releasing the allocated handle")
            drainCompleted.complete()
            shutdownFinished.fulfill()
        }

        // An exclusive close cannot pass the admitted read's permit. Its ENXIO
        // reply proves shutdown has closed admission and cancelled the owner,
        // without relying on a sleep or observing private volume state.
        let rejectedClose = try await close(volume, item: item, modes: .read)
        assertPOSIX(rejectedClose, ENXIO)
        XCTAssertNil(reply.value)
        XCTAssertFalse(drainCompleted.value)
        XCTAssertTrue(fixture.requests.filter { ["read", "release"].contains($0.operation) }.isEmpty)
        allowOpenResponse.signal()
        await fulfillment(of: [readFinished, shutdownFinished], timeout: 3)

        let result = try XCTUnwrap(reply.value)
        XCTAssertEqual(result.count, 0)
        assertPOSIX(result.error, EINTR)
        XCTAssertTrue(drainCompleted.value)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.map(\.access), [1])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101])
        XCTAssertTrue(fixture.requests.filter { $0.operation == "read" }.isEmpty)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testFailedTemporaryReleaseRemainsOwnedForDrainRetry() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([0, 255])) },
                                            release: { attempt in attempt == 1 ? EIO : 0 })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)

        let result = try await read(volume, item: item, length: 2)

        XCTAssertEqual(result.count, 2)
        assertPOSIX(result.error, EIO)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101])
        try await shutdown(volume)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101, 101])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "forget" }.map(\.inode), [7])
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testInvalidAndZeroLengthReadsDoNotOpenHandles() async throws {
        let fixture = try VolumeReadFixture(read: { _ in
            XCTFail("Bounds must be checked before any bridge read")
            return VolumeReadFixture.binary(Data())
        })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let invalid: [(off_t, Int, Int)] = [(-1, 1, 1), (0, -1, 1), (off_t.max, 1, 1), (0, 2, 1), (0, 0, -1)]
        for (offset, length, capacity) in invalid {
            let result = try await read(volume, item: item, offset: offset, length: length, capacity: capacity)
            XCTAssertEqual(result.count, 0)
            assertPOSIX(result.error, EINVAL)
        }
        let empty = try await read(volume, item: item, offset: 99, length: 0, capacity: 0)
        XCTAssertEqual(empty.count, 0)
        XCTAssertNil(empty.error)
        XCTAssertTrue(fixture.requests.filter { ["open", "read", "release"].contains($0.operation) }.isEmpty)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    private func prepare(_ fixture: VolumeReadFixture) async throws -> (RepoReachVolume, FSItem) {
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: VolumeReadFixture.token))
        let volume = RepoReachVolume(client: client, identifier: UUID(), readOnly: false)
        let activated = expectation(description: "Production volume activated")
        let rootReply = ItemReply()
        volume.activate { item, error in rootReply.store(item: item, error: error); activated.fulfill() }
        await fulfillment(of: [activated], timeout: 3)
        let root = try XCTUnwrap(rootReply.value)
        if let error = root.error { throw error }
        let rootItem = try XCTUnwrap(root.item)
        let lookedUp = expectation(description: "Production file item looked up")
        let itemReply = ItemReply()
        volume.lookupItem(named: FSFileName(string: "binary"), inDirectory: rootItem) { item, _, error in
            itemReply.store(item: item, error: error)
            lookedUp.fulfill()
        }
        await fulfillment(of: [lookedUp], timeout: 3)
        let file = try XCTUnwrap(itemReply.value)
        if let error = file.error { throw error }
        return (volume, try XCTUnwrap(file.item))
    }

    private func read(_ volume: RepoReachVolume, item: FSItem, offset: off_t = 0, length: Int,
                      capacity: Int? = nil, capture: ReadCapture = ReadCapture()) async throws -> ReadReply.Value {
        let finished = expectation(description: "Production read callback")
        let reply = ReadReply()
        volume.read(from: item, at: offset, length: length, capacity: capacity ?? length,
                    consume: { capture.append($0, at: $1) }) { count, error in
            reply.store(count: count, error: error)
            finished.fulfill()
        }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value)
    }

    private func open(_ volume: RepoReachVolume, item: FSItem, modes: FSVolume.OpenModes) async throws -> Error? {
        let finished = expectation(description: "Production open callback")
        let reply = ReadReply()
        volume.openItem(item, modes: modes) { error in reply.store(count: 0, error: error); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value).error
    }

    private func close(_ volume: RepoReachVolume, item: FSItem, modes: FSVolume.OpenModes) async throws -> Error? {
        let finished = expectation(description: "Production close callback")
        let reply = ReadReply()
        volume.closeItem(item, modes: modes) { error in reply.store(count: 0, error: error); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value).error
    }

    private func shutdown(_ volume: RepoReachVolume) async throws {
        let finished = expectation(description: "Production volume drained")
        let completed = CompletionFlag()
        Task { await volume.shutdown(); completed.complete(); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        guard completed.value else { throw FSBridgeError.timedOut }
    }

    private func assertPOSIX(_ error: Error?, _ code: Int32, file: StaticString = #filePath, line: UInt = #line) {
        let value = error as NSError?
        XCTAssertEqual(value?.domain, NSPOSIXErrorDomain, file: file, line: line)
        XCTAssertEqual(value?.code, Int(code), file: file, line: line)
    }
}

private final class ReadReply: @unchecked Sendable {
    struct Value { let count: Int; let error: Error? }
    private let lock = NSLock()
    private var stored: Value?
    var value: Value? { lock.lock(); defer { lock.unlock() }; return stored }
    func store(count: Int, error: Error?) { lock.lock(); defer { lock.unlock() }; stored = Value(count: count, error: error) }
}

private final class CompletionFlag: @unchecked Sendable {
    private let lock = NSLock()
    private var completed = false
    var value: Bool { lock.lock(); defer { lock.unlock() }; return completed }
    func complete() { lock.lock(); defer { lock.unlock() }; completed = true }
}

private final class ItemReply: @unchecked Sendable {
    struct Value { let item: FSItem?; let error: Error? }
    private let lock = NSLock()
    private var stored: Value?
    var value: Value? { lock.lock(); defer { lock.unlock() }; return stored }
    func store(item: FSItem?, error: Error?) { lock.lock(); defer { lock.unlock() }; stored = Value(item: item, error: error) }
}

private final class ReadCapture: @unchecked Sendable {
    private let lock = NSLock()
    private var bytes = Data()
    private var offsets: [Int] = []
    var data: Data { lock.lock(); defer { lock.unlock() }; return bytes }
    var positions: [Int] { lock.lock(); defer { lock.unlock() }; return offsets }
    func append(_ data: Data, at position: Int) { lock.lock(); defer { lock.unlock() }; bytes.append(data); offsets.append(position) }
}

/// The production transport owns a socket per request. This peer accepts those
/// independently, including cancellation of a stalled read while release runs.
private final class VolumeReadFixture: @unchecked Sendable {
    struct Request {
        let operation: String
        let inode: UInt64?
        let handle: UInt64?
        let access: UInt32?
        let offset: UInt64?
        let size: Int?
    }
    static let token = String(repeating: "a", count: 64)
    let socketPath: String
    private let directory: URL
    private let listener: Int32
    private let lock = NSLock()
    private let workers = DispatchGroup()
    private let accepted = DispatchSemaphore(value: 0)
    private var connections: Set<Int32> = []
    private var recorded: [Request] = []
    private var recordedFailures: [String] = []
    private var stopped = false
    private var handle: UInt64 = 100
    private var releases = 0
    private let readResponse: (Request) -> Data?
    private let releaseResponse: (Int) -> Int32
    private let onOpen: (UInt64) -> Void
    var requests: [Request] { locked { recorded } }
    var failures: [String] { locked { recordedFailures } }

    init(read: @escaping (Request) -> Data?, release: @escaping (Int) -> Int32 = { _ in 0 },
         onOpen: @escaping (UInt64) -> Void = { _ in }) throws {
        directory = URL(fileURLWithPath: "/tmp/rr-fsv-" + UUID().uuidString.prefix(8), isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        socketPath = directory.appendingPathComponent("socket").path
        readResponse = read
        releaseResponse = release
        self.onOpen = onOpen
        listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard listener >= 0 else {
            let code = Darwin.errno
            try? FileManager.default.removeItem(at: directory)
            throw FSBridgeError.unavailable(code)
        }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(socketPath.utf8) + [0]) }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0, chmod(socketPath, 0o600) == 0, Darwin.listen(listener, 16) == 0 else {
            let code = Darwin.errno
            Darwin.close(listener)
            try? FileManager.default.removeItem(at: directory)
            throw FSBridgeError.unavailable(code)
        }
        DispatchQueue.global(qos: .userInitiated).async { self.acceptLoop() }
    }

    func stop() {
        let shouldStop = locked { () -> Bool in
            guard !stopped else { return false }
            stopped = true
            for fd in connections { Darwin.shutdown(fd, SHUT_RDWR) }
            Darwin.shutdown(listener, SHUT_RDWR)
            Darwin.close(listener)
            return true
        }
        guard shouldStop else { return }
        _ = accepted.wait(timeout: .now() + 2)
        _ = workers.wait(timeout: .now() + 2)
        try? FileManager.default.removeItem(at: directory)
    }

    private func acceptLoop() {
        defer { accepted.signal() }
        while true {
            let fd = Darwin.accept(listener, nil, nil)
            guard fd >= 0 else { return }
            let admitted = locked { () -> Bool in
                guard !stopped else { Darwin.close(fd); return false }
                connections.insert(fd)
                workers.enter()
                return true
            }
            guard admitted else { return }
            DispatchQueue.global(qos: .userInitiated).async {
                defer {
                    self.locked { self.connections.remove(fd); Darwin.close(fd) }
                    self.workers.leave()
                }
                self.serve(fd)
            }
        }
    }

    private func serve(_ fd: Int32) {
        var noSigPipe: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        do {
            let request = try Self.request(fd)
            locked { recorded.append(request) }
            let response: Data?
            switch request.operation {
            case "read": response = readResponse(request)
            case "getattr": response = try Self.metadata(["node": Self.node(inode: 1, type: "dir")])
            case "lookup": response = try Self.metadata(["node": Self.node(inode: 7, type: "file")])
            case "statfs": response = try Self.metadata([:])
            case "open":
                let value = locked { () -> UInt64 in handle += 1; return handle }
                onOpen(value)
                response = try Self.metadata(["handle": value])
            case "release":
                let attempt = locked { () -> Int in releases += 1; return releases }
                let code = releaseResponse(attempt)
                response = code == 0 ? try Self.metadata([:]) : Self.error(code)
            case "forget", "flush", "fsync": response = try Self.metadata([:])
            default: response = Self.error(EOPNOTSUPP)
            }
            if let response { Self.send(response, to: fd) }
            else {
                var byte: UInt8 = 0
                while Darwin.recv(fd, &byte, 1, 0) > 0 {}
            }
        } catch {
            locked { if !stopped { recordedFailures.append(String(describing: error)) } }
        }
    }

    static func binary(_ bytes: Data) -> Data { http(body: bytes, contentType: "application/octet-stream", errno: 0, status: 200) }
    static func error(_ code: Int32) -> Data {
        http(body: Data("{\"version\":1,\"errno\":\(code)}".utf8), contentType: "application/json", errno: code, status: 500)
    }
    private static func metadata(_ fields: [String: Any]) throws -> Data {
        var body = fields
        body["version"] = 1
        body["errno"] = 0
        return http(body: try JSONSerialization.data(withJSONObject: body), contentType: "application/json", errno: 0, status: 200)
    }
    private static func node(inode: UInt64, type: String) -> [String: Any] {
        ["inode": inode, "generation": 1, "attributes": ["size": 3_145_728, "nlink": 1,
          "mode": type == "dir" ? 0o40755 : 0o100644, "type": type, "uid": getuid(), "gid": getgid(),
          "atime_ns": 0, "mtime_ns": 0, "ctime_ns": 0, "birthtime_ns": 0]]
    }
    private static func http(body: Data, contentType: String, errno: Int32, status: Int) -> Data {
        var response = Data("HTTP/1.1 \(status) Response\r\nContent-Type: \(contentType)\r\nContent-Length: \(body.count)\r\nX-RepoReach-Errno: \(errno)\r\nConnection: close\r\n\r\n".utf8)
        response.append(body)
        return response
    }
    private static func send(_ bytes: Data, to fd: Int32) {
        var sent = 0
        while sent < bytes.count {
            let count = bytes.withUnsafeBytes { Darwin.send(fd, $0.baseAddress!.advanced(by: sent), bytes.count - sent, 0) }
            if count < 0 && Darwin.errno == EINTR { continue }
            guard count > 0 else { return }
            sent += count
        }
    }
    private static func request(_ fd: Int32) throws -> Request {
        let separator = Data([13, 10, 13, 10])
        var bytes = Data()
        while bytes.range(of: separator) == nil {
            guard bytes.count <= 16_384 else { throw FSBridgeError.responseTooLarge }
            try receive(fd, into: &bytes)
        }
        let ending = bytes.range(of: separator)!
        guard let header = String(data: bytes[..<ending.lowerBound], encoding: .ascii) else { throw FSBridgeError.malformedResponse }
        let lines = header.components(separatedBy: "\r\n")
        let first = lines[0].split(separator: " ")
        guard first.count == 3 else { throw FSBridgeError.malformedResponse }
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":") else { throw FSBridgeError.malformedResponse }
            headers[line[..<colon].lowercased()] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        guard headers["authorization"] == "Bearer " + token, let rawLength = headers["content-length"],
              let length = Int(rawLength), length >= 0, length <= 65_536 else { throw FSBridgeError.invalidRequest }
        var body = Data(bytes[ending.upperBound...])
        while body.count < length { try receive(fd, into: &body) }
        guard body.count == length else { throw FSBridgeError.invalidRequest }
        if first[0] == "GET", let url = URLComponents(string: String(first[1])), url.path == "/v1/fs/read" {
            let fields = Dictionary(uniqueKeysWithValues: (url.queryItems ?? []).map { ($0.name, $0.value ?? "") })
            return Request(operation: "read", inode: fields["inode"].flatMap(UInt64.init), handle: fields["handle"].flatMap(UInt64.init),
                           access: nil, offset: fields["offset"].flatMap(UInt64.init), size: fields["size"].flatMap(Int.init))
        }
        guard first[0] == "POST", first[1] == "/v1/fs",
              let fields = try JSONSerialization.jsonObject(with: body) as? [String: Any],
              let op = fields["op"] as? String else { throw FSBridgeError.invalidRequest }
        return Request(operation: op, inode: (fields["inode"] as? NSNumber)?.uint64Value,
                       handle: (fields["handle"] as? NSNumber)?.uint64Value, access: (fields["access"] as? NSNumber)?.uint32Value,
                       offset: nil, size: nil)
    }
    private static func receive(_ fd: Int32, into bytes: inout Data) throws {
        var buffer = [UInt8](repeating: 0, count: 8192)
        let count = Darwin.recv(fd, &buffer, buffer.count, 0)
        guard count > 0 else { throw FSBridgeError.unavailable(Darwin.errno) }
        bytes.append(contentsOf: buffer.prefix(count))
    }
    private func locked<T>(_ body: () -> T) -> T { lock.lock(); defer { lock.unlock() }; return body() }
}
