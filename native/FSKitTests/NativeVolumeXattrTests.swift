import XCTest
import Foundation
import FSKit
import Darwin
#if canImport(NativeFilesystem)
@testable import NativeFilesystem
#endif

/// These callbacks use real volume/item identities and the production client.
/// Attribute values remain opaque bytes; the independent peer implements only
/// the documented HTTP contract, rather than replacing the volume or client.
@available(macOS 15.4, *)
final class NativeVolumeXattrTests: XCTestCase {
    func testBinaryAndEmptyValuesRoundTripWithArbitraryAttributeNames() async throws {
        let fixture = try XattrPeer()
        defer { fixture.stop() }
        let (volume, _, item) = try await prepare(fixture)
        let name = "meta/path+?&=é#\n"
        let bytes = Data([0, 255, 128, 13, 10, 0, 97])
        XCTAssertEqual(volume.maximumXattrSize, 1_048_576)

        let setError = try await set(volume, item: item, name: FSFileName(string: name), value: bytes)
        XCTAssertNil(setError)
        let fetched = try await get(volume, item: item, name: FSFileName(string: name))
        XCTAssertNil(fetched.error)
        XCTAssertEqual(fetched.value, bytes)
        let emptyError = try await set(volume, item: item, name: FSFileName(string: ".."), value: Data())
        XCTAssertNil(emptyError)
        let empty = try await get(volume, item: item, name: FSFileName(string: ".."))
        XCTAssertNil(empty.error)
        XCTAssertEqual(empty.value, Data())
        let listed = try await list(volume, item: item)
        XCTAssertNil(listed.error)
        XCTAssertEqual(try XCTUnwrap(listed.value).map(\.data), [Data("..".utf8), Data(name.utf8)])
        let put = try XCTUnwrap(fixture.requests.first { $0.operation == "setxattr" })
        XCTAssertEqual(put.body, bytes)
        XCTAssertEqual(put.name, Data(name.utf8))
        XCTAssertEqual(put.policy, "always_set")
        XCTAssertTrue(put.target.contains("%2B"))
        XCTAssertTrue(put.target.contains("%2F"))
        XCTAssertFalse(put.target.contains("\n"))
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testMaximumSizeValueRoundTripsWithoutTextConversion() async throws {
        let fixture = try XattrPeer()
        defer { fixture.stop() }
        let (volume, _, item) = try await prepare(fixture)
        let bytes = Data((0..<1_048_576).map { UInt8(truncatingIfNeeded: $0) })
        let name = FSFileName(string: "com.apple.ResourceFork")
        let error = try await set(volume, item: item, name: name, value: bytes)
        XCTAssertNil(error)
        let result = try await get(volume, item: item, name: name)
        XCTAssertNil(result.error)
        XCTAssertEqual(result.value, bytes)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "setxattr" }.map(\.body), [bytes])
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testCreateReplaceAndDeletePoliciesPreserveMissingAndExistingErrors() async throws {
        let fixture = try XattrPeer()
        defer { fixture.stop() }
        let (volume, _, item) = try await prepare(fixture)
        let name = FSFileName(string: "user.binary")
        let missing = try await get(volume, item: item, name: name)
        assertPOSIX(missing.error, ENOATTR)
        assertPOSIX(try await set(volume, item: item, name: name, value: Data([1]), policy: .mustReplace), ENOATTR)
        let created = try await set(volume, item: item, name: name, value: Data([0, 255]), policy: .mustCreate)
        XCTAssertNil(created)
        assertPOSIX(try await set(volume, item: item, name: name, value: Data([9]), policy: .mustCreate), EEXIST)
        let replaced = try await set(volume, item: item, name: name, value: Data([128]), policy: .mustReplace)
        XCTAssertNil(replaced)
        let replacement = try await get(volume, item: item, name: name)
        XCTAssertEqual(replacement.value, Data([128]))
        let deleted = try await set(volume, item: item, name: name, value: nil, policy: .delete)
        XCTAssertNil(deleted)
        assertPOSIX(try await set(volume, item: item, name: name, value: nil, policy: .delete), ENOATTR)
        let recreated = try await set(volume, item: item, name: name, value: Data(), policy: .alwaysSet)
        XCTAssertNil(recreated)
        // FSKit permits a non-nil value with delete; the value is ignored and
        // must not accidentally become a PUT or enter the metadata request.
        let ignoredValue = Data([255, 0, 128])
        let deletedAgain = try await set(volume, item: item, name: name, value: ignoredValue, policy: .delete)
        XCTAssertNil(deletedAgain)
        let deletion = try XCTUnwrap(fixture.requests.last { $0.operation == "removexattr" })
        let deletionFields = try XCTUnwrap(try JSONSerialization.jsonObject(with: deletion.body) as? [String: Any])
        XCTAssertEqual(Set(deletionFields.keys), Set(["version", "op", "inode", "name"]))
        let listed = try await list(volume, item: item)
        XCTAssertNil(listed.error)
        XCTAssertEqual(listed.value?.count, 0)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testUnicodeNormalizationDistinctNamesHaveIndependentValuesAndListEntries() async throws {
        let composed = Data("é".utf8)
        let decomposed = Data("e\u{301}".utf8)
        XCTAssertNotEqual(composed, decomposed)
        let fixture = try XattrPeer()
        defer { fixture.stop() }
        let (volume, _, item) = try await prepare(fixture)
        let first = try await set(volume, item: item, name: FSFileName(data: composed), value: Data([1]))
        XCTAssertNil(first)
        let second = try await set(volume, item: item, name: FSFileName(data: decomposed), value: Data([2]))
        XCTAssertNil(second)
        let composedValue = try await get(volume, item: item, name: FSFileName(data: composed))
        let decomposedValue = try await get(volume, item: item, name: FSFileName(data: decomposed))
        XCTAssertEqual(composedValue.value, Data([1]))
        XCTAssertEqual(decomposedValue.value, Data([2]))
        let names = try await list(volume, item: item)
        XCTAssertNil(names.error)
        XCTAssertEqual(try XCTUnwrap(names.value).map(\.data), [decomposed, composed])
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testInvalidNamesAreRejectedBeforeAnyAttributeRequest() async throws {
        let fixture = try XattrPeer()
        defer { fixture.stop() }
        let (volume, _, item) = try await prepare(fixture)
        let requestCount = fixture.requests.count
        let invalid: [(FSFileName, Int32)] = [
            (FSFileName(data: Data()), EINVAL),
            (FSFileName(data: Data([0xff, 0xfe])), EINVAL),
            (FSFileName(string: String(repeating: "a", count: 128)), ENAMETOOLONG),
            (FSFileName(string: String(repeating: "é", count: 64)), ENAMETOOLONG)
        ]
        for (name, code) in invalid {
            let result = try await get(volume, item: item, name: name)
            assertPOSIX(result.error, code)
            XCTAssertNil(result.value)
            assertPOSIX(try await set(volume, item: item, name: name, value: Data([1])), code)
        }
        XCTAssertEqual(fixture.requests.count, requestCount)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testInvalidValuesAndUnknownPoliciesAreRejectedBeforeAnyAttributeRequest() async throws {
        let fixture = try XattrPeer()
        defer { fixture.stop() }
        let (volume, _, item) = try await prepare(fixture)
        let name = FSFileName(string: "user.value")
        let requestCount = fixture.requests.count
        for policy: FSVolume.SetXattrPolicy in [.alwaysSet, .mustCreate, .mustReplace] {
            assertPOSIX(try await set(volume, item: item, name: name, value: nil, policy: policy), EINVAL)
        }
        assertPOSIX(try await set(volume, item: item, name: name, value: Data(repeating: 0xff, count: 1_048_577)), E2BIG)
        let unknown = try XCTUnwrap(FSVolume.SetXattrPolicy(rawValue: 99))
        assertPOSIX(try await set(volume, item: item, name: name, value: Data(), policy: unknown), EINVAL)
        XCTAssertEqual(fixture.requests.count, requestCount)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testReadOnlyPoliciesRejectMutationsWithoutRPCButPermitAttributeReads() async throws {
        let name = Data("user.existing".utf8)
        let bytes = Data([0, 255])
        for hardReadOnly in [false, true] {
            let fixture = try XattrPeer(attributes: [name: bytes])
            defer { fixture.stop() }
            let (volume, _, item) = try await prepare(fixture, readOnly: hardReadOnly,
                                                     taskOptions: hardReadOnly ? ["-o", "rw"] : ["-o", "ro"])
            let requestCount = fixture.requests.count
            assertPOSIX(try await set(volume, item: item, name: FSFileName(data: name), value: Data([1])), EROFS)
            assertPOSIX(try await set(volume, item: item, name: FSFileName(data: name), value: nil, policy: .delete), EROFS)
            XCTAssertEqual(fixture.requests.count, requestCount)
            let result = try await get(volume, item: item, name: FSFileName(data: name))
            XCTAssertNil(result.error)
            XCTAssertEqual(result.value, bytes)
            let names = try await list(volume, item: item)
            XCTAssertNil(names.error)
            XCTAssertEqual(names.value?.map(\.data), [name])
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testMissingMeaningIsMappedOnlyForTaggedAttributeErrors() async throws {
        let scenarios: [(Int32, Bool, Int32)] = [(61, true, ENOATTR), (61, false, 61), (0, true, EIO)]
        for (wireErrno, missing, expected) in scenarios {
            let fixture = try XattrPeer(override: { request in
                guard ["getxattr", "listxattr", "removexattr"].contains(request.operation) else { return nil }
                return .bytes(XattrPeer.error(wireErrno, missing: missing))
            })
            defer { fixture.stop() }
            let (volume, _, item) = try await prepare(fixture)
            let result = try await get(volume, item: item, name: FSFileName(string: "missing"))
            assertPOSIX(result.error, expected)
            XCTAssertNil(result.value)
            let names = try await list(volume, item: item)
            assertPOSIX(names.error, expected)
            XCTAssertNil(names.value)
            assertPOSIX(try await set(volume, item: item, name: FSFileName(string: "missing"), value: nil, policy: .delete), expected)
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testMalformedListsFailWithoutReturningPartialNames() async throws {
        for invalid in [["valid", ""], ["valid", "bad\0name"], ["valid", String(repeating: "a", count: 128)], ["same", "same"]] {
            let fixture = try XattrPeer(override: { request in
                request.operation == "listxattr" ? .bytes(try! XattrPeer.metadata(["xattr_names": invalid])) : nil
            })
            defer { fixture.stop() }
            let (volume, _, item) = try await prepare(fixture)
            let names = try await list(volume, item: item)
            assertPOSIX(names.error, EIO)
            XCTAssertNil(names.value)
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testForeignAndReclaimedItemsAreRejectedBeforeAttributeRequests() async throws {
        let name = Data("user.identity".utf8)
        let fixture = try XattrPeer(attributes: [name: Data([7])])
        defer { fixture.stop() }
        let (volume, root, oldItem) = try await prepare(fixture)
        let (otherVolume, _, foreignItem) = try await prepare(fixture)
        let beforeForeign = fixture.requests.count
        let foreign = try await get(volume, item: foreignItem, name: FSFileName(data: name))
        assertPOSIX(foreign.error, ESTALE)
        XCTAssertEqual(fixture.requests.count, beforeForeign)
        let reclaimError = try await errorReply("Item reclaimed") { volume.reclaimItem(oldItem, replyHandler: $0) }
        XCTAssertNil(reclaimError)
        let newItem = try await lookup(volume, root: root)
        XCTAssertFalse(oldItem === newItem)
        let beforeStale = fixture.requests.count
        let stale = try await get(volume, item: oldItem, name: FSFileName(data: name))
        assertPOSIX(stale.error, ESTALE)
        let staleList = try await list(volume, item: oldItem)
        assertPOSIX(staleList.error, ESTALE)
        assertPOSIX(try await set(volume, item: oldItem, name: FSFileName(data: name), value: Data([1])), ESTALE)
        XCTAssertEqual(fixture.requests.count, beforeStale)
        let fresh = try await get(volume, item: newItem, name: FSFileName(data: name))
        XCTAssertNil(fresh.error)
        XCTAssertEqual(fresh.value, Data([7]))
        try await shutdown(otherVolume)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testUnmountRemountUsesFreshItemsAndPreservesPeerAttributeValues() async throws {
        let name = FSFileName(string: "user.remount")
        let fixture = try XattrPeer()
        defer { fixture.stop() }
        let (volume, _, oldItem) = try await prepare(fixture)
        let setError = try await set(volume, item: oldItem, name: name, value: Data([0, 255]))
        XCTAssertNil(setError)
        let mounted = try await errorReply("Volume mounted") { volume.mount(replyHandler: $0) }
        XCTAssertNil(mounted)
        let unmounted = expectation(description: "Volume unmounted")
        let completion = XattrBox<Bool>()
        volume.unmount { completion.store(true); unmounted.fulfill() }
        await fulfillment(of: [unmounted], timeout: 3)
        _ = try XCTUnwrap(completion.value)
        let beforeStale = fixture.requests.count
        let stale = try await get(volume, item: oldItem, name: name)
        assertPOSIX(stale.error, ESTALE)
        XCTAssertEqual(fixture.requests.count, beforeStale)
        let activated = try await activate(volume)
        let remounted = try await errorReply("Volume remounted") { volume.mount(replyHandler: $0) }
        XCTAssertNil(remounted)
        let newItem = try await lookup(volume, root: activated)
        XCTAssertFalse(oldItem === newItem)
        let result = try await get(volume, item: newItem, name: name)
        XCTAssertNil(result.error)
        XCTAssertEqual(result.value, Data([0, 255]))
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testShutdownCancelsAttributeReadAndRejectsFurtherCallbacks() async throws {
        let accepted = expectation(description: "Attribute read accepted before shutdown")
        let fixture = try XattrPeer(override: { request in
            guard request.operation == "getxattr" else { return nil }
            accepted.fulfill()
            return .silent
        })
        defer { fixture.stop() }
        let (volume, _, item) = try await prepare(fixture)
        let finished = expectation(description: "Attribute read cancelled")
        let result = XattrBox<XattrResult<Data>>()
        volume.getXattr(named: FSFileName(string: "user.stalled"), of: item) { value, error in
            result.store(XattrResult(value: value, error: error)); finished.fulfill()
        }
        await fulfillment(of: [accepted], timeout: 3)
        try await shutdown(volume)
        await fulfillment(of: [finished], timeout: 3)
        let cancelled = try XCTUnwrap(result.value)
        assertPOSIX(cancelled.error, EINTR)
        XCTAssertNil(cancelled.value)
        let count = fixture.requests.count
        let afterClose = try await get(volume, item: item, name: FSFileName(string: "user.stalled"))
        assertPOSIX(afterClose.error, ENXIO)
        let afterList = try await list(volume, item: item)
        assertPOSIX(afterList.error, ENXIO)
        assertPOSIX(try await set(volume, item: item, name: FSFileName(string: "user.stalled"), value: Data()), ENXIO)
        assertPOSIX(try await set(volume, item: item, name: FSFileName(string: "user.stalled"), value: nil, policy: .delete), ENXIO)
        XCTAssertEqual(fixture.requests.count, count)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    private func prepare(_ fixture: XattrPeer, readOnly: Bool = false, taskOptions: [String] = []) async throws -> (RepoReachVolume, FSItem, FSItem) {
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: XattrPeer.token))
        let volume = RepoReachVolume(client: client, identifier: UUID(), readOnly: readOnly)
        let root = try await activate(volume, taskOptions: taskOptions)
        return (volume, root, try await lookup(volume, root: root))
    }

    private func activate(_ volume: RepoReachVolume, taskOptions: [String] = []) async throws -> FSItem {
        let finished = expectation(description: "Volume activated")
        let reply = XattrBox<XattrResult<FSItem>>()
        volume.activate(taskOptions: taskOptions) { value, error in reply.store(XattrResult(value: value, error: error)); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        let result = try XCTUnwrap(reply.value)
        if let error = result.error { throw error }
        return try XCTUnwrap(result.value)
    }

    private func lookup(_ volume: RepoReachVolume, root: FSItem) async throws -> FSItem {
        let finished = expectation(description: "Real file item looked up")
        let reply = XattrBox<XattrResult<FSItem>>()
        volume.lookupItem(named: FSFileName(string: "tracked.txt"), inDirectory: root) { value, _, error in
            reply.store(XattrResult(value: value, error: error)); finished.fulfill()
        }
        await fulfillment(of: [finished], timeout: 3)
        let result = try XCTUnwrap(reply.value)
        if let error = result.error { throw error }
        return try XCTUnwrap(result.value)
    }

    private func get(_ volume: RepoReachVolume, item: FSItem, name: FSFileName) async throws -> XattrResult<Data> {
        let finished = expectation(description: "Attribute get callback")
        let reply = XattrBox<XattrResult<Data>>()
        volume.getXattr(named: name, of: item) { value, error in reply.store(XattrResult(value: value, error: error)); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value)
    }

    private func list(_ volume: RepoReachVolume, item: FSItem) async throws -> XattrResult<[FSFileName]> {
        let finished = expectation(description: "Attribute list callback")
        let reply = XattrBox<XattrResult<[FSFileName]>>()
        volume.listXattrs(of: item) { value, error in reply.store(XattrResult(value: value, error: error)); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value)
    }

    private func set(_ volume: RepoReachVolume, item: FSItem, name: FSFileName, value: Data?,
                     policy: FSVolume.SetXattrPolicy = .alwaysSet) async throws -> Error? {
        try await errorReply("Attribute set callback") {
            volume.setXattr(named: name, to: value, on: item, policy: policy, replyHandler: $0)
        }
    }

    private func errorReply(_ description: String, operation: (@escaping (Error?) -> Void) -> Void) async throws -> Error? {
        let finished = expectation(description: description)
        let reply = XattrBox<XattrResult<Bool>>()
        operation { error in reply.store(XattrResult(value: true, error: error)); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value).error
    }

    private func shutdown(_ volume: RepoReachVolume) async throws {
        let finished = expectation(description: "Volume shutdown drained")
        let reply = XattrBox<Bool>()
        Task { await volume.shutdown(); reply.store(true); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        guard reply.value == true else { throw FSBridgeError.timedOut }
    }

    private func assertPOSIX(_ error: Error?, _ code: Int32, file: StaticString = #filePath, line: UInt = #line) {
        let value = error as NSError?
        XCTAssertEqual(value?.domain, NSPOSIXErrorDomain, file: file, line: line)
        XCTAssertEqual(value?.code, Int(code), file: file, line: line)
    }
}

private struct XattrResult<Value> { let value: Value?; let error: Error? }

private final class XattrBox<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var stored: Value?
    var value: Value? { lock.lock(); defer { lock.unlock() }; return stored }
    func store(_ value: Value) { lock.lock(); defer { lock.unlock() }; stored = value }
}

/// A capability-authenticated private HTTP peer with raw-byte attribute keys.
/// Swift String equality normalizes Unicode; Data keys intentionally do not.
private final class XattrPeer: @unchecked Sendable {
    struct Request {
        let operation: String
        let target: String
        let inode: UInt64
        let name: Data?
        let policy: String?
        let body: Data
    }
    enum Override { case bytes(Data), silent }
    static let token = String(repeating: "b", count: 64)
    let socketPath: String
    private let directory: URL
    private let listener: Int32
    private let lock = NSLock()
    private let workers = DispatchGroup()
    private let accepted = DispatchSemaphore(value: 0)
    private var connections: Set<Int32> = []
    private var stopped = false
    private var recorded: [Request] = []
    private var recordedFailures: [String] = []
    private var attributes: [Data: Data]
    private let override: (Request) -> Override?
    var requests: [Request] { locked { recorded } }
    var failures: [String] { locked { recordedFailures } }

    init(attributes: [Data: Data] = [:], override: @escaping (Request) -> Override? = { _ in nil }) throws {
        directory = URL(fileURLWithPath: "/tmp/rr-xa-" + UUID().uuidString.prefix(8), isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        socketPath = directory.appendingPathComponent("s").path
        self.attributes = attributes
        self.override = override
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
        let bound = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard bound == 0, chmod(socketPath, 0o600) == 0, Darwin.listen(listener, 16) == 0 else {
            let code = Darwin.errno
            Darwin.close(listener)
            try? FileManager.default.removeItem(at: directory)
            throw FSBridgeError.unavailable(code)
        }
        DispatchQueue.global(qos: .userInitiated).async { self.acceptLoop() }
    }

    func stop() {
        let didStop = locked { () -> Bool in
            guard !stopped else { return false }
            stopped = true
            for fd in connections { Darwin.shutdown(fd, SHUT_RDWR) }
            Darwin.shutdown(listener, SHUT_RDWR)
            Darwin.close(listener)
            return true
        }
        guard didStop else { return }
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
                connections.insert(fd); workers.enter(); return true
            }
            guard admitted else { return }
            DispatchQueue.global(qos: .userInitiated).async {
                defer { self.locked { self.connections.remove(fd); Darwin.close(fd) }; self.workers.leave() }
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
            if let response = override(request) {
                switch response {
                case .bytes(let bytes): Self.send(bytes, to: fd)
                case .silent:
                    var byte: UInt8 = 0
                    while Darwin.recv(fd, &byte, 1, 0) > 0 {}
                }
                return
            }
            Self.send(try response(to: request), to: fd)
        } catch { locked { if !stopped { recordedFailures.append(String(describing: error)) } } }
    }

    private func response(to request: Request) throws -> Data {
        switch request.operation {
        case "getattr": return try Self.metadata(["node": Self.node(inode: 1, type: "dir")])
        case "lookup": return try Self.metadata(["node": Self.node(inode: 7, type: "file")])
        case "statfs", "forget": return try Self.metadata([:])
        case "getxattr":
            guard request.inode == 7, let name = request.name else { return Self.error(EINVAL) }
            guard let value = locked({ attributes[name] }) else { return Self.error(61, missing: true) }
            return Self.http(value, type: "application/octet-stream", status: 200, errno: 0)
        case "setxattr":
            guard request.inode == 7, let name = request.name else { return Self.error(EINVAL) }
            let error = locked { () -> Int32 in
                if request.policy == "must_create", attributes[name] != nil { return EEXIST }
                if request.policy == "must_replace", attributes[name] == nil { return 61 }
                guard let policy = request.policy, ["always_set", "must_create", "must_replace"].contains(policy) else { return EINVAL }
                attributes[name] = request.body
                return 0
            }
            return error == 0 ? try Self.metadata(["written": request.body.count]) : Self.error(error, missing: error == 61)
        case "listxattr":
            guard request.inode == 7 else { return Self.error(EINVAL) }
            let names = locked { attributes.keys.sorted { $0.lexicographicallyPrecedes($1) }.map { String(decoding: $0, as: UTF8.self) } }
            return try Self.metadata(["xattr_names": names])
        case "removexattr":
            guard request.inode == 7, let name = request.name else { return Self.error(EINVAL) }
            let removed = locked { attributes.removeValue(forKey: name) }
            return removed == nil ? Self.error(61, missing: true) : try Self.metadata([:])
        default: return Self.error(EOPNOTSUPP)
        }
    }

    static func metadata(_ fields: [String: Any]) throws -> Data {
        var body = fields
        body["version"] = 1; body["errno"] = 0
        return http(try JSONSerialization.data(withJSONObject: body), type: "application/json", status: 200, errno: 0)
    }
    static func error(_ code: Int32, missing: Bool = false) -> Data {
        let fields: [String: Any] = ["version": 1, "errno": code, "xattr_missing": missing]
        return http(try! JSONSerialization.data(withJSONObject: fields), type: "application/json", status: code == 0 ? 200 : 409, errno: code)
    }
    private static func http(_ body: Data, type: String, status: Int, errno: Int32) -> Data {
        var bytes = Data("HTTP/1.1 \(status) Response\r\nContent-Type: \(type)\r\nContent-Length: \(body.count)\r\nX-RepoReach-Errno: \(errno)\r\nConnection: close\r\n\r\n".utf8)
        bytes.append(body)
        return bytes
    }
    private static func node(inode: UInt64, type: String) -> [String: Any] {
        ["inode": inode, "generation": 1, "attributes": ["size": 1, "nlink": 1, "mode": type == "dir" ? 0o40755 : 0o100644,
          "type": type, "uid": getuid(), "gid": getgid(), "atime_ns": 0, "mtime_ns": 0, "ctime_ns": 0, "birthtime_ns": 0]]
    }
    private static func send(_ bytes: Data, to fd: Int32) {
        var position = 0
        while position < bytes.count {
            let count = bytes.withUnsafeBytes { Darwin.send(fd, $0.baseAddress!.advanced(by: position), bytes.count - position, 0) }
            if count < 0 && Darwin.errno == EINTR { continue }
            guard count > 0 else { return }
            position += count
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
        guard headers["authorization"] == "Bearer " + token, let count = headers["content-length"].flatMap(Int.init),
              count >= 0, count <= 1_048_576 else { throw FSBridgeError.invalidRequest }
        var body = Data(bytes[ending.upperBound...])
        while body.count < count { try receive(fd, into: &body) }
        guard body.count == count else { throw FSBridgeError.invalidRequest }
        let target = String(first[1])
        // Go url.Values uses form-query decoding: a literal '+' means space.
        // Decode percent escapes only after that substitution, preserving %2B.
        if let url = URLComponents(string: target.replacingOccurrences(of: "+", with: "%20")), url.path == "/v1/fs/xattr" {
            var fields: [String: String] = [:]
            for field in url.queryItems ?? [] {
                guard fields[field.name] == nil, let value = field.value else { throw FSBridgeError.invalidRequest }
                fields[field.name] = value
            }
            guard let inode = fields["inode"].flatMap(UInt64.init), let name = fields["name"],
                  first[0] == "GET" || first[0] == "PUT" else { throw FSBridgeError.invalidRequest }
            return Request(operation: first[0] == "GET" ? "getxattr" : "setxattr", target: target, inode: inode,
                           name: Data(name.utf8), policy: fields["policy"], body: body)
        }
        guard first[0] == "POST", target == "/v1/fs", let fields = try JSONSerialization.jsonObject(with: body) as? [String: Any],
              let op = fields["op"] as? String else { throw FSBridgeError.invalidRequest }
        return Request(operation: op, target: target, inode: (fields["inode"] as? NSNumber)?.uint64Value ?? 0,
                       name: (fields["name"] as? String).map { Data($0.utf8) }, policy: nil, body: body)
    }
    private static func receive(_ fd: Int32, into bytes: inout Data) throws {
        var buffer = [UInt8](repeating: 0, count: 8_192)
        let count = Darwin.recv(fd, &buffer, buffer.count, 0)
        guard count > 0 else { throw FSBridgeError.unavailable(Darwin.errno) }
        bytes.append(contentsOf: buffer.prefix(count))
    }
    private func locked<Value>(_ body: () -> Value) -> Value { lock.lock(); defer { lock.unlock() }; return body() }
}
