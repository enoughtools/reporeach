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

    func testNonWritableDescriptorResourceSelectsWritableRepositoryAccess() async throws {
        guard #available(macOS 26.0, *) else { throw XCTSkip("Path resources require macOS 26") }
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([7])) })
        defer { fixture.stop() }
        let resource = FSPathURLResource(url: URL(fileURLWithPath: fixture.socketPath).deletingLastPathComponent(), writable: false)
        XCTAssertFalse(resource.isWritable)
        let selected = RepoReachFileSystem.readOnlyPolicy(for: resource, taskOptions: [])
        XCTAssertFalse(selected)
        let (volume, item) = try await prepare(fixture, readOnly: selected)
        let openError = try await open(volume, item: item, modes: .write)
        XCTAssertNil(openError)
        let bytes = Data([0, 255, 128, 0, 13, 10])

        let result = try await write(volume, item: item, offset: 4, data: bytes)

        XCTAssertNil(result.error)
        XCTAssertEqual(result.count, bytes.count)
        let request = try XCTUnwrap(fixture.requests.first { $0.operation == "write" })
        XCTAssertEqual(request.body, bytes)
        XCTAssertEqual(request.offset, 4)
        XCTAssertEqual(request.handle, 101)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.map(\.access), [2])
        let closeError = try await close(volume, item: item, modes: [])
        XCTAssertNil(closeError)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testLoadReadOnlyRestrictionRequiresExactExplicitOption() throws {
        guard #available(macOS 26.0, *) else { throw XCTSkip("Path resources require macOS 26") }
        for writable in [false, true] {
            let resource = FSPathURLResource(url: URL(fileURLWithPath: "/tmp/reporeach-test-connection"), writable: writable)
            for options in [[], ["--rdonly=false"], ["--not-rdonly"], ["prefix--rdonly"], ["-o", "ro"]] {
                XCTAssertFalse(RepoReachFileSystem.readOnlyPolicy(for: resource, taskOptions: options), "Unexpected load restriction: \(options)")
            }
            XCTAssertTrue(RepoReachFileSystem.readOnlyPolicy(for: resource, taskOptions: ["--rdonly"]))
        }
    }

    func testHardLoadReadOnlyCannotBeOverriddenByWritableSessionOptions() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([0, 255])) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture, readOnly: true, taskOptions: ["-o", "rw"])
        let mountError = try await mount(volume, taskOptions: ["-o", "rw"])
        XCTAssertNil(mountError)
        assertPOSIX(try await open(volume, item: item, modes: .write), EROFS)
        let result = try await read(volume, item: item, length: 2)
        XCTAssertNil(result.error)
        XCTAssertEqual(result.count, 2)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.map(\.access), [1])
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testActivationReadOnlyOptionsSurviveEmptyActivationAndMount() async throws {
        for option in ["ro", "rdonly"] {
            let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([7])) })
            defer { fixture.stop() }
            let (volume, item) = try await prepare(fixture, taskOptions: ["-o", option])
            let reactivation = try await activate(volume)
            XCTAssertNil(reactivation.error)
            let mountError = try await mount(volume)
            XCTAssertNil(mountError)
            assertPOSIX(try await open(volume, item: item, modes: .write), EROFS)
            let result = try await read(volume, item: item, length: 1)
            XCTAssertNil(result.error)
            XCTAssertEqual(result.count, 1)
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testFirstMountCanOverrideActivationPolicyInEitherDirection() async throws {
        for initiallyReadOnly in [false, true] {
            let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) })
            defer { fixture.stop() }
            let initial = initiallyReadOnly ? "ro" : "rw"
            let opposite = initiallyReadOnly ? "rw" : "ro"
            let (volume, item) = try await prepare(fixture, taskOptions: ["-o", initial])

            let mountError = try await mount(volume, taskOptions: ["-o", opposite])

            XCTAssertNil(mountError)
            let requestCount = fixture.requests.count
            let openError = try await open(volume, item: item, modes: .write)
            let bytes = Data([0, 255, 128, 0])
            let result = try await write(volume, item: item, data: bytes)
            if initiallyReadOnly {
                XCTAssertNil(openError)
                XCTAssertNil(result.error)
                XCTAssertEqual(result.count, bytes.count)
                XCTAssertEqual(fixture.requests.filter { $0.operation == "write" }.map(\.body), [bytes])
                let closeError = try await close(volume, item: item, modes: [])
                XCTAssertNil(closeError)
            } else {
                assertPOSIX(openError, EROFS)
                assertPOSIX(result.error, EROFS)
                XCTAssertEqual(result.count, 0)
                XCTAssertEqual(fixture.requests.count, requestCount)
            }
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testOrderedMountOptionsUseExactNamesAndReadOnlyPriorityWithinOneValue() async throws {
        let cases: [([String], Bool)] = [
            (["-o", "ro", "-o", "rw"], false),
            (["-o", "rw", "-o", "ro"], true),
            (["-o", "rw,ro"], true),
            (["-o", "ro,rw"], true),
            (["-o", "rdonly,rw"], true),
            (["-o", "rw,noexec"], false),
            (["-o", "arrow,read-only,rdonlyish"], false),
            (["-o", "RO"], false),
            (["-oro"], false),
            (["-o", "ro", "-o", "unknown"], true),
            (["-o", "rw", "--", "-o", "ro"], false),
            (["-o", "ro", "--", "-o", "rw"], true)
        ]
        for (options, expectedReadOnly) in cases {
            let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) })
            defer { fixture.stop() }
            let (volume, item) = try await prepare(fixture, taskOptions: options)
            let error = try await open(volume, item: item, modes: .write)
            if expectedReadOnly { assertPOSIX(error, EROFS) }
            else {
                XCTAssertNil(error, "Writable options rejected: \(options)")
                let closeError = try await close(volume, item: item, modes: [])
                XCTAssertNil(closeError)
            }
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testReadOnlyRejectsMutationCallbacksWithoutSendingMutationRequests() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([0, 255])) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture, taskOptions: ["-o", "ro"])
        let activation = try await activate(volume)
        XCTAssertNil(activation.error)
        let root = try XCTUnwrap(activation.item)
        let requestCount = fixture.requests.count
        assertPOSIX(try await open(volume, item: item, modes: .write), EROFS)
        let createError = try await create(volume, root: root)
        assertPOSIX(createError, EROFS)
        let attributes = FSItem.SetAttributesRequest()
        attributes.size = 0
        let setattrError = try await setAttributes(volume, item: item, attributes: attributes)
        assertPOSIX(setattrError, EROFS)
        let result = try await write(volume, item: item, data: Data([1]))
        XCTAssertEqual(result.count, 0)
        assertPOSIX(result.error, EROFS)
        XCTAssertEqual(fixture.requests.count, requestCount)
        XCTAssertFalse(attributes.wasAttributeConsumed(.size))

        let readResult = try await read(volume, item: item, length: 2)

        XCTAssertNil(readResult.error)
        XCTAssertEqual(readResult.count, 2)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.map(\.access), [1])
        XCTAssertTrue(fixture.requests.filter { ["create", "setattr", "write"].contains($0.operation) }.isEmpty)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testMountedPolicyCannotChangeButSamePolicyCallbacksRemainValid() async throws {
        for initiallyReadOnly in [false, true] {
            let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) })
            defer { fixture.stop() }
            let initial = initiallyReadOnly ? "ro" : "rw"
            let opposite = initiallyReadOnly ? "rw" : "ro"
            let (volume, item) = try await prepare(fixture, taskOptions: ["-o", initial])
            let firstMountError = try await mount(volume)
            XCTAssertNil(firstMountError)
            let requestCount = fixture.requests.count
            assertPOSIX(try await mount(volume, taskOptions: ["-o", opposite]), EBUSY)
            let activation = try await activate(volume, taskOptions: ["-o", opposite])
            assertPOSIX(activation.error, EBUSY)
            XCTAssertEqual(fixture.requests.count, requestCount)
            try await assertWritePolicy(volume, item: item, readOnly: initiallyReadOnly)
            let samePolicy = try await activate(volume, taskOptions: ["-o", initial])
            XCTAssertNil(samePolicy.error)
            let sameMountError = try await mount(volume, taskOptions: ["-o", initial])
            XCTAssertNil(sameMountError)
            try await assertWritePolicy(volume, item: item, readOnly: initiallyReadOnly)
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testRetainedReadHandlePreventsPolicyChangesUntilClosed() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let openError = try await open(volume, item: item, modes: .read)
        XCTAssertNil(openError)
        let requestCount = fixture.requests.count
        assertPOSIX(try await mount(volume, taskOptions: ["-o", "ro"]), EBUSY)
        let activation = try await activate(volume, taskOptions: ["-o", "ro"])
        assertPOSIX(activation.error, EBUSY)
        XCTAssertEqual(fixture.requests.count, requestCount)
        let samePolicy = try await activate(volume, taskOptions: ["-o", "rw"])
        XCTAssertNil(samePolicy.error)
        let closeError = try await close(volume, item: item, modes: [])
        XCTAssertNil(closeError)
        let changed = try await activate(volume, taskOptions: ["-o", "ro"])
        XCTAssertNil(changed.error)
        assertPOSIX(try await open(volume, item: item, modes: .write), EROFS)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testPendingFailedReleasePreventsPolicyChangeBeforeDrain() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data([7])) },
                                            release: { attempt in attempt == 1 ? EIO : 0 })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let result = try await read(volume, item: item, length: 1)
        assertPOSIX(result.error, EIO)
        let requestCount = fixture.requests.count
        let activation = try await activate(volume, taskOptions: ["-o", "ro"])
        assertPOSIX(activation.error, EBUSY)
        assertPOSIX(try await mount(volume, taskOptions: ["-o", "ro"]), EBUSY)
        XCTAssertEqual(fixture.requests.count, requestCount)
        try await shutdown(volume)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101, 101])
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testFailedActivationAndMountDoNotPublishProposedReadOnlyPolicy() async throws {
        for initiallyReadOnly in [false, true] {
            let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) }, metadataFailure: { request, attempt in
                (request.operation == "statfs" && attempt == 2) || (request.operation == "getattr" && attempt == 3) ? EIO : 0
            })
            defer { fixture.stop() }
            let initial = initiallyReadOnly ? "ro" : "rw"
            let opposite = initiallyReadOnly ? "rw" : "ro"
            let (volume, item) = try await prepare(fixture, taskOptions: ["-o", initial])
            let activation = try await activate(volume, taskOptions: ["-o", opposite])
            assertPOSIX(activation.error, EIO)
            try await assertWritePolicy(volume, item: item, readOnly: initiallyReadOnly)
            assertPOSIX(try await mount(volume, taskOptions: ["-o", opposite]), EIO)
            try await assertWritePolicy(volume, item: item, readOnly: initiallyReadOnly)
            let changed = try await activate(volume, taskOptions: ["-o", opposite])
            XCTAssertNil(changed.error)
            try await assertWritePolicy(volume, item: item, readOnly: !initiallyReadOnly)
            try await shutdown(volume)
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    func testDanglingMountOptionFailsWithoutChangingCurrentPolicyOrSendingRequests() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) })
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture, taskOptions: ["-o", "ro"])
        let requestCount = fixture.requests.count
        let activation = try await activate(volume, taskOptions: ["-o"])
        assertPOSIX(activation.error, EINVAL)
        assertPOSIX(try await mount(volume, taskOptions: ["-o"]), EINVAL)
        XCTAssertEqual(fixture.requests.count, requestCount)
        assertPOSIX(try await open(volume, item: item, modes: .write), EROFS)
        let changed = try await activate(volume, taskOptions: ["-o", "rw"])
        XCTAssertNil(changed.error)
        let openError = try await open(volume, item: item, modes: .write)
        XCTAssertNil(openError)
        let closeError = try await close(volume, item: item, modes: [])
        XCTAssertNil(closeError)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testUnmountAndDeactivatePreserveReadOnlyUntilExplicitWritableActivation() async throws {
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) })
        defer { fixture.stop() }
        let (volume, _) = try await prepare(fixture, taskOptions: ["-o", "ro"])
        let mountError = try await mount(volume)
        XCTAssertNil(mountError)
        try await unmount(volume)
        let deactivateError = try await deactivate(volume)
        XCTAssertNil(deactivateError)
        let activation = try await activate(volume)
        XCTAssertNil(activation.error)
        let root = try XCTUnwrap(activation.item)
        let item = try await lookup(volume, root: root)
        assertPOSIX(try await open(volume, item: item, modes: .write), EROFS)
        let changed = try await activate(volume, taskOptions: ["-o", "rw"])
        XCTAssertNil(changed.error)
        let openError = try await open(volume, item: item, modes: .write)
        XCTAssertNil(openError)
        let closeError = try await close(volume, item: item, modes: [])
        XCTAssertNil(closeError)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testDirectoryPageContinuesAndReplaysWithoutRepeatedFetchOrForget() async throws {
        let fixture = try directoryFixture(entries: (0..<3).map { directoryEntry($0) }, eof: false)
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)

        let first = try await enumerate(volume, root: root, capacity: 1)
        XCTAssertNil(first.error)
        XCTAssertEqual(first.entries.map(\.name), ["file0"])
        XCTAssertEqual(first.entries.map(\.cookie), [1])
        let replay = try await enumerate(volume, root: root, verifier: first.verifier, capacity: 1)
        XCTAssertNil(replay.error)
        XCTAssertEqual(replay.entries.map(\.name), ["file0"])
        let second = try await enumerate(volume, root: root, cookie: 1, verifier: first.verifier, capacity: 1)
        XCTAssertNil(second.error)
        XCTAssertEqual(second.entries.map(\.name), ["file1"])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "readdir" }.map(\.offset), [0])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "batchforget" }.count, 1)
        XCTAssertTrue(fixture.requests.filter { $0.operation == "forget" }.isEmpty)

        let third = try await enumerate(volume, root: root, cookie: 2, verifier: first.verifier, capacity: 1)
        XCTAssertNil(third.error)
        XCTAssertEqual(third.entries.map(\.name), ["file2"])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "readdir" }.map(\.offset), [0, 3])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "batchforget" }.count, 1)
        let end = try await enumerate(volume, root: root, cookie: 3, verifier: first.verifier, capacity: 1)
        XCTAssertNil(end.error)
        XCTAssertTrue(end.entries.isEmpty)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "releasedir" }.count, 1)
        try await shutdown(volume)
        XCTAssertTrue(fixture.requests.filter { $0.operation == "forget" }.isEmpty)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testUnknownDirectorySizeIsInvalidUntilGetAttributesResolvesIt() async throws {
        let unresolved = VolumeReadFixture.node(inode: 7, type: "file", size: 0, sizeKnown: false)
        let entry: [String: Any] = ["name": "binary", "offset": 1, "node": unresolved]
        let fixture = try directoryFixture(entries: [entry], lookupNode: unresolved)
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)

        let listing = try await enumerate(volume, root: root, capacity: 1)
        XCTAssertNil(listing.error)
        let listed = try XCTUnwrap(listing.entries.first?.attributes)
        XCTAssertTrue(listed.isValid(.type))
        XCTAssertFalse(listed.isValid(.size))
        XCTAssertFalse(listed.isValid(.allocSize))

        let item = try await lookup(volume, root: root)
        let desired = directoryAttributes()
        let resolved: FSItem.Attributes = try await withCheckedThrowingContinuation { continuation in
            volume.getAttributes(desired, of: item) { result, error in
                if let error { continuation.resume(throwing: error) }
                else if let result { continuation.resume(returning: result) }
                else { continuation.resume(throwing: POSIXError(.EIO)) }
            }
        }
        XCTAssertTrue(resolved.isValid(.size))
        XCTAssertEqual(resolved.size, 3_145_728)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "getattr" }.map(\.inode), [1, 7])
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testMetadataOnlyAttributesLeaveUnknownSizeUnresolvedUntilRequested() async throws {
        let fixture = try sizeFixture(knownSize: 4099)
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let metadata = FSItem.GetAttributesRequest()
        metadata.wantedAttributes = [.type, .mode, .fileID]

        let cheap = try await attributes(volume, item: item, desired: metadata)

        XCTAssertTrue(cheap.isValid(.type))
        XCTAssertEqual(cheap.type, .file)
        XCTAssertEqual(cheap.mode, 0o644)
        XCTAssertTrue(cheap.isValid(.fileID))
        XCTAssertFalse(cheap.isValid(.size), "A metadata probe must not invent an unknown size")
        let exact = FSItem.GetAttributesRequest()
        exact.wantedAttributes = [.size, .fileID]
        let resolved = try await attributes(volume, item: item, desired: exact)
        XCTAssertTrue(resolved.isValid(.size))
        XCTAssertEqual(resolved.size, 4099)
        XCTAssertFalse(resolved.isValid(.type), "The reply must respect the requested attribute mask")
        let requests = fixture.requests.filter { $0.operation == "getattr" && $0.inode == 7 }
        XCTAssertEqual(requests.count, 2)
        let cheapFields = try XCTUnwrap(JSONSerialization.jsonObject(with: requests[0].body) as? [String: Any])
        let exactFields = try XCTUnwrap(JSONSerialization.jsonObject(with: requests[1].body) as? [String: Any])
        XCTAssertNil(cheapFields["require_size"])
        XCTAssertEqual(exactFields["require_size"] as? Bool, true)
        XCTAssertTrue(fixture.requests.filter { ["open", "read"].contains($0.operation) }.isEmpty)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testExactAttributesUseExistingReadHandleAndResolveUnknownSize() async throws {
        let fixture = try sizeFixture(knownSize: 65539)
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let openError = try await open(volume, item: item, modes: .read)
        XCTAssertNil(openError)
        let desired = FSItem.GetAttributesRequest()
        desired.wantedAttributes = [.size]

        let resolved = try await attributes(volume, item: item, desired: desired)

        XCTAssertTrue(resolved.isValid(.size))
        XCTAssertEqual(resolved.size, 65539)
        let request = try XCTUnwrap(fixture.requests.first { $0.operation == "getattr" && $0.inode == 7 })
        XCTAssertEqual(request.handle, 101)
        let fields = try XCTUnwrap(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        XCTAssertEqual(fields["require_size"] as? Bool, true)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "open" }.count, 1)
        let closeError = try await close(volume, item: item, modes: [])
        XCTAssertNil(closeError)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "release" }.map(\.handle), [101])
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testExactSizeRequestRejectsPeerThatLeavesSizeUnknown() async throws {
        let fixture = try sizeFixture(knownSize: 4099, resolvesSize: false)
        defer { fixture.stop() }
        let (volume, item) = try await prepare(fixture)
        let desired = FSItem.GetAttributesRequest()
        desired.wantedAttributes = [.size]

        do {
            _ = try await attributes(volume, item: item, desired: desired)
            XCTFail("An exact size request must not succeed with an unresolved size")
        } catch { assertPOSIX(error, EIO) }

        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testLegacyDirectorySizeRemainsValidWithoutSizeKnownField() async throws {
        let fixture = try directoryFixture(entries: [directoryEntry(0)])
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let listing = try await enumerate(volume, root: root, capacity: 1)
        XCTAssertNil(listing.error)
        let attributes = try XCTUnwrap(listing.entries.first?.attributes)
        XCTAssertTrue(attributes.isValid(.size))
        XCTAssertEqual(attributes.size, 3_145_728)
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testDirectoryBatchForgetAggregatesReferencesForRepeatedInode() async throws {
        let fixture = try directoryFixture(entries: (0..<3).map { directoryEntry($0, inode: 7) })
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let listing = try await enumerate(volume, root: root, capacity: 3)
        XCTAssertNil(listing.error)
        let batch = try XCTUnwrap(fixture.requests.first { $0.operation == "batchforget" })
        XCTAssertEqual(try forgetPairs(batch), [FSBridgeForget(inode: 7, n: 3)])
        try await shutdown(volume)
        XCTAssertTrue(fixture.requests.filter { $0.operation == "forget" }.isEmpty)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testDirectoryBatchFailureKeepsReferencesForSessionDrain() async throws {
        let fixture = try directoryFixture(entries: (0..<3).map { directoryEntry($0) },
            metadataFailure: { request, _ in request.operation == "batchforget" ? EBUSY : 0 })
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let listing = try await enumerate(volume, root: root, capacity: 3)
        assertPOSIX(listing.error, EBUSY)
        XCTAssertTrue(listing.entries.isEmpty)
        try await shutdown(volume)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "forget" }.compactMap(\.inode).sorted(), [7, 8, 9])
        XCTAssertEqual(fixture.requests.filter { $0.operation == "releasedir" }.count, 1)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testMalformedDirectoryPageBalancesReferencesBeforeReplyingError() async throws {
        var second = directoryEntry(1)
        second["offset"] = 1
        let fixture = try directoryFixture(entries: [directoryEntry(0), second])
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let listing = try await enumerate(volume, root: root, capacity: 3)
        assertPOSIX(listing.error, EIO)
        XCTAssertTrue(listing.entries.isEmpty)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "batchforget" }.count, 1)
        try await shutdown(volume)
        XCTAssertTrue(fixture.requests.filter { $0.operation == "forget" }.isEmpty)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "releasedir" }.count, 1)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testFailedDirectoryBatchIsNotReplayedAgainstLaterLookupReferences() async throws {
        let entries = (0..<3).map { directoryEntry($0) }
        let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) },
            metadataFailure: { request, attempt in request.operation == "batchforget" && attempt == 2 ? EBUSY : 0 },
            metadata: { request in
                switch request.operation {
                case "getattr": return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 1, type: "dir")])
                case "statfs", "batchforget", "forget", "releasedir": return try VolumeReadFixture.metadata([:])
                case "opendir": return try VolumeReadFixture.metadata(["handle": 101])
                case "readdir": return try VolumeReadFixture.metadata([
                    "entries": request.offset == 0 ? Array(entries.prefix(2)) : Array(entries.suffix(1)),
                    "next_offset": request.offset == 0 ? 2 : 3, "eof": request.offset != 0])
                default: return VolumeReadFixture.error(EOPNOTSUPP)
                }
            })
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let first = try await enumerate(volume, root: root, capacity: 1)
        XCTAssertNil(first.error)
        let failed = try await enumerate(volume, root: root, cookie: 1, verifier: first.verifier, capacity: 1)
        assertPOSIX(failed.error, EBUSY)
        XCTAssertEqual(failed.entries.map(\.name), ["file1"])
        let retried = try await enumerate(volume, root: root, cookie: 2, verifier: first.verifier, capacity: 1)
        assertPOSIX(retried.error, EIO)
        XCTAssertTrue(retried.entries.isEmpty)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "batchforget" }.count, 2)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "readdir" }.map(\.offset), [0, 2])
        try await shutdown(volume)
        XCTAssertEqual(fixture.requests.filter { $0.operation == "forget" }.compactMap(\.inode), [9])
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testMaximumDirectoryPageUsesBoundedBatchRequests() async throws {
        let fixture = try directoryFixture(entries: (0..<2_048).map { directoryEntry($0, inode: UInt64.max - UInt64($0) - 1) })
        defer { fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let listing = try await enumerate(volume, root: root, capacity: 0)
        XCTAssertNil(listing.error)
        XCTAssertTrue(listing.entries.isEmpty)
        let batches = fixture.requests.filter { $0.operation == "batchforget" }
        XCTAssertEqual(batches.count, 2)
        for batch in batches {
            XCTAssertEqual(try forgetPairs(batch).count, FSBridgeForget.maximumBatchCount)
            XCTAssertLessThanOrEqual(batch.body.count, 65_536)
        }
        try await shutdown(volume)
        XCTAssertTrue(fixture.requests.filter { $0.operation == "forget" }.isEmpty)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    private func directoryEntry(_ index: Int, inode: UInt64? = nil) -> [String: Any] {
        ["name": "file\(index)", "offset": index + 1,
         "node": VolumeReadFixture.node(inode: inode ?? UInt64(index + 7), type: "file")]
    }

    func testConcurrentLookupsShareOneItemAndBalanceOutOfOrderReferences() async throws {
        let firstAccepted = expectation(description: "First lookup accepted")
        let secondAccepted = expectation(description: "Second lookup overlaps first")
        let allowFirst = DispatchSemaphore(value: 0)
        let allowSecond = DispatchSemaphore(value: 0)
        let fixture = try lookupFixture { request in
            let name = try self.requestName(request)
            if name == "first" { firstAccepted.fulfill() }
            else { XCTAssertEqual(name, "second"); secondAccepted.fulfill() }
            let barrier = name == "first" ? allowFirst : allowSecond
            guard barrier.wait(timeout: .now() + 5) == .success else { throw FSBridgeError.timedOut }
            return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 7, type: "file")])
        }
        defer { allowFirst.signal(); allowSecond.signal(); fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let firstFinished = expectation(description: "First lookup replied")
        let secondFinished = expectation(description: "Second lookup replied first")
        let first = ItemReply(), second = ItemReply()
        volume.lookupItem(named: FSFileName(string: "first"), inDirectory: root) { item, _, error in
            first.store(item: item, error: error); firstFinished.fulfill()
        }
        await fulfillment(of: [firstAccepted], timeout: 3)
        volume.lookupItem(named: FSFileName(string: "second"), inDirectory: root) { item, _, error in
            second.store(item: item, error: error); secondFinished.fulfill()
        }
        await fulfillment(of: [secondAccepted], timeout: 3)
        allowSecond.signal()
        await fulfillment(of: [secondFinished], timeout: 3)
        XCTAssertNil(first.value)
        allowFirst.signal()
        await fulfillment(of: [firstFinished], timeout: 3)
        let firstReply = try XCTUnwrap(first.value), secondReply = try XCTUnwrap(second.value)
        XCTAssertNil(firstReply.error); XCTAssertNil(secondReply.error)
        XCTAssertTrue(try XCTUnwrap(firstReply.item) === XCTUnwrap(secondReply.item))
        try await shutdown(volume)
        let forgotten = fixture.requests.filter { $0.operation == "forget" }
        XCTAssertEqual(forgotten.count, 1)
        XCTAssertEqual(forgotten.first?.inode, 7)
        let fields = try XCTUnwrap(JSONSerialization.jsonObject(with: XCTUnwrap(forgotten.first).body) as? [String: Any])
        XCTAssertEqual(fields["n"] as? Int, 2)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testSharedLookupsKeepWritersExclusiveAndBlockLaterLookups() async throws {
        let accepted = expectation(description: "Two shared lookup requests accepted")
        accepted.expectedFulfillmentCount = 2
        let allowFirst = DispatchSemaphore(value: 0), allowSecond = DispatchSemaphore(value: 0)
        let allowWriter = DispatchSemaphore(value: 0)
        let firstReady = CompletionFlag(), secondReady = CompletionFlag(), writerReady = CompletionFlag()
        let writerStarted = CompletionFlag()
        let earlyWriter = expectation(description: "Writer cannot overlap outstanding lookups")
        earlyWriter.isInverted = true
        let writerAccepted = expectation(description: "Queued writer accepted after both lookups")
        let earlyLateLookup = expectation(description: "Later lookup cannot overlap writer")
        earlyLateLookup.isInverted = true
        let fixture = try lookupFixture { request in
            if request.operation == "create" {
                if !firstReady.value || !secondReady.value { earlyWriter.fulfill() }
                writerStarted.complete(); writerAccepted.fulfill()
                guard allowWriter.wait(timeout: .now() + 5) == .success else { throw FSBridgeError.timedOut }
                writerReady.complete()
                return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 8, type: "file"), "handle": 101])
            }
            let name = try self.requestName(request)
            if name == "later" {
                if !writerReady.value { earlyLateLookup.fulfill() }
            } else {
                accepted.fulfill()
                let barrier = name == "first" ? allowFirst : allowSecond
                guard barrier.wait(timeout: .now() + 5) == .success else { throw FSBridgeError.timedOut }
                (name == "first" ? firstReady : secondReady).complete()
            }
            return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 7, type: "file")])
        }
        defer { allowFirst.signal(); allowSecond.signal(); allowWriter.signal(); fixture.stop() }
        let (volume, root) = try await prepareDirectory(fixture)
        let firstFinished = expectation(description: "First shared lookup finished")
        let secondFinished = expectation(description: "Second shared lookup finished")
        for (name, finished) in [("first", firstFinished), ("second", secondFinished)] {
            volume.lookupItem(named: FSFileName(string: name), inDirectory: root) { item, _, error in
                XCTAssertNotNil(item); XCTAssertNil(error); finished.fulfill()
            }
        }
        await fulfillment(of: [accepted], timeout: 3)
        let writerFinished = expectation(description: "Writer callback finished")
        volume.createItem(named: FSFileName(string: "created"), type: .file, inDirectory: root,
                          attributes: FSItem.SetAttributesRequest()) { item, _, error in
            XCTAssertNotNil(item); XCTAssertNil(error); writerFinished.fulfill()
        }
        await fulfillment(of: [earlyWriter], timeout: 0.05)
        allowFirst.signal()
        await fulfillment(of: [firstFinished], timeout: 3)
        XCTAssertFalse(writerStarted.value, "One outstanding lookup must still exclude the writer")
        allowSecond.signal()
        await fulfillment(of: [secondFinished, writerAccepted], timeout: 3)
        let laterFinished = expectation(description: "Later lookup finishes after writer")
        let later = ItemReply()
        volume.lookupItem(named: FSFileName(string: "later"), inDirectory: root) { item, _, error in
            later.store(item: item, error: error); laterFinished.fulfill()
        }
        await fulfillment(of: [earlyLateLookup], timeout: 0.05)
        XCTAssertNil(later.value)
        allowWriter.signal()
        await fulfillment(of: [writerFinished, laterFinished], timeout: 3)
        XCTAssertNil(try XCTUnwrap(later.value).error)
        let namespace = fixture.requests.filter { $0.operation == "lookup" || $0.operation == "create" }
        XCTAssertEqual(namespace.map(\.operation), ["lookup", "lookup", "create", "lookup"])
        try await shutdown(volume)
        XCTAssertTrue(fixture.failures.isEmpty)
    }

    func testLookupDrainWaitsForGrantedReferenceBeforeUnmountAndShutdown() async throws {
        for shuttingDown in [false, true] {
            let allocated = expectation(description: "Peer granted a lookup reference")
            let allowReply = DispatchSemaphore(value: 0)
            let responseReady = CompletionFlag(), referenceForgotten = CompletionFlag(), drained = CompletionFlag()
            let fixture = try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) }, metadata: { request in
                switch request.operation {
                case "getattr": return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 1, type: "dir")])
                case "statfs": return try VolumeReadFixture.metadata([:])
                case "lookup":
                    let name = try self.requestName(request)
                    if name == "held" {
                        allocated.fulfill()
                        guard allowReply.wait(timeout: .now() + 5) == .success else { throw FSBridgeError.timedOut }
                        responseReady.complete()
                    }
                    return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 7, type: "file")])
                case "forget":
                    XCTAssertTrue(responseReady.value, "An unknown reference must not be forgotten before its response")
                    let fields = try XCTUnwrap(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
                    XCTAssertEqual(fields["n"] as? Int, 1)
                    XCTAssertEqual(request.inode, 7)
                    referenceForgotten.complete()
                    return try VolumeReadFixture.metadata([:])
                default: return VolumeReadFixture.error(EOPNOTSUPP)
                }
            })
            defer { allowReply.signal(); fixture.stop() }
            let (volume, root) = try await prepareDirectory(fixture)
            let lookupFinished = expectation(description: "Lookup replies with the learned item")
            let lookupReply = ItemReply()
            volume.lookupItem(named: FSFileName(string: "held"), inDirectory: root) { item, _, error in
                XCTAssertTrue(responseReady.value)
                lookupReply.store(item: item, error: error); lookupFinished.fulfill()
            }
            await fulfillment(of: [allocated], timeout: 3)
            let drainFinished = expectation(description: "Drain balanced the learned reference")
            let complete = {
                XCTAssertTrue(responseReady.value); XCTAssertTrue(referenceForgotten.value)
                drained.complete(); drainFinished.fulfill()
            }
            if shuttingDown { Task { await volume.shutdown(); complete() } }
            else { volume.unmount(replyHandler: complete) }
            // This exclusive callback cannot pass the held lookup. ENXIO proves
            // drain closed admission before we release its response barrier.
            assertPOSIX(try await close(volume, item: root, modes: []), ENXIO)
            XCTAssertNil(lookupReply.value); XCTAssertFalse(drained.value)
            XCTAssertFalse(referenceForgotten.value)
            allowReply.signal()
            await fulfillment(of: [lookupFinished, drainFinished], timeout: 3)
            let learned = try XCTUnwrap(lookupReply.value)
            XCTAssertNil(learned.error)
            let oldItem = try XCTUnwrap(learned.item)
            XCTAssertEqual(fixture.requests.filter { $0.operation == "forget" }.count, 1)
            if !shuttingDown {
                let activated = try await activate(volume)
                XCTAssertNil(activated.error)
                let fresh = try await lookup(volume, root: XCTUnwrap(activated.item))
                XCTAssertFalse(fresh === oldItem, "Remount must not reuse the old session's item")
                try await shutdown(volume)
                XCTAssertEqual(fixture.requests.filter { $0.operation == "forget" }.count, 2)
            }
            XCTAssertTrue(fixture.failures.isEmpty)
        }
    }

    private func lookupFixture(response: @escaping (VolumeReadFixture.Request) throws -> Data?) throws -> VolumeReadFixture {
        try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) }, metadata: { request in
            switch request.operation {
            case "getattr": return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 1, type: "dir")])
            case "lookup", "create": return try response(request)
            case "statfs", "forget", "fsync", "release": return try VolumeReadFixture.metadata([:])
            default: return VolumeReadFixture.error(EOPNOTSUPP)
            }
        })
    }

    private func requestName(_ request: VolumeReadFixture.Request) throws -> String {
        let fields = try XCTUnwrap(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        return try XCTUnwrap(fields["name"] as? String)
    }

    private func sizeFixture(knownSize: UInt64, resolvesSize: Bool = true) throws -> VolumeReadFixture {
        try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) }, metadata: { request in
            switch request.operation {
            case "getattr":
                if request.inode == 1 { return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 1, type: "dir")]) }
                let fields = try XCTUnwrap(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
                let resolve = fields["require_size"] as? Bool == true && resolvesSize
                return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 7, type: "file",
                    size: resolve ? knownSize : 0, sizeKnown: resolve)])
            case "lookup":
                return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: 7, type: "file", size: 0, sizeKnown: false)])
            case "open": return try VolumeReadFixture.metadata(["handle": 101])
            case "statfs", "forget", "flush", "fsync", "release": return try VolumeReadFixture.metadata([:])
            default: return VolumeReadFixture.error(EOPNOTSUPP)
            }
        })
    }

    private func attributes(_ volume: RepoReachVolume, item: FSItem, desired: FSItem.GetAttributesRequest) async throws -> FSItem.Attributes {
        try await withCheckedThrowingContinuation { continuation in
            volume.getAttributes(desired, of: item) { result, error in
                if let error { continuation.resume(throwing: error) }
                else if let result { continuation.resume(returning: result) }
                else { continuation.resume(throwing: POSIXError(.EIO)) }
            }
        }
    }

    private func directoryFixture(entries: [[String: Any]], eof: Bool = true, lookupNode: [String: Any]? = nil,
                                  metadataFailure: @escaping (VolumeReadFixture.Request, Int) -> Int32 = { _, _ in 0 }) throws -> VolumeReadFixture {
        try VolumeReadFixture(read: { _ in VolumeReadFixture.binary(Data()) }, metadataFailure: metadataFailure,
            metadata: { request in
                switch request.operation {
                case "getattr":
                    return try VolumeReadFixture.metadata(["node": VolumeReadFixture.node(inode: request.inode ?? 1,
                        type: request.inode == 1 ? "dir" : "file")])
                case "lookup": return try VolumeReadFixture.metadata(["node": lookupNode ?? VolumeReadFixture.node(inode: 7, type: "file")])
                case "statfs", "batchforget", "forget", "releasedir": return try VolumeReadFixture.metadata([:])
                case "opendir": return try VolumeReadFixture.metadata(["handle": 101])
                case "readdir": return try VolumeReadFixture.metadata(["entries": request.offset == 0 ? entries : [],
                    "next_offset": entries.count, "eof": request.offset == 0 ? eof : true])
                default: return VolumeReadFixture.error(EOPNOTSUPP)
                }
            })
    }

    private func prepareDirectory(_ fixture: VolumeReadFixture) async throws -> (RepoReachVolume, FSItem) {
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: VolumeReadFixture.token))
        let volume = RepoReachVolume(client: client, identifier: UUID(), readOnly: false)
        let root = try await activate(volume)
        if let error = root.error { throw error }
        return (volume, try XCTUnwrap(root.item))
    }

    private func directoryAttributes() -> FSItem.GetAttributesRequest {
        let request = FSItem.GetAttributesRequest()
        request.wantedAttributes = [.type, .size, .allocSize, .fileID, .parentID]
        return request
    }

    private func enumerate(_ volume: RepoReachVolume, root: FSItem, cookie: UInt64 = 0,
                           verifier: FSDirectoryVerifier = FSDirectoryVerifier(0), capacity: Int) async throws -> DirectoryCapture.Value {
        let finished = expectation(description: "Production directory callback")
        let capture = DirectoryCapture(capacity: capacity)
        volume.enumerateDirectory(root, startingAt: FSDirectoryCookie(cookie), verifier: verifier, attributes: directoryAttributes(),
            packEntry: { name, _, _, nextCookie, attributes in
                capture.pack(name: name.string ?? "", cookie: nextCookie.rawValue, attributes: attributes)
            }) { verifier, error in
                capture.finish(verifier: verifier, error: error)
                finished.fulfill()
            }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(capture.value)
    }

    private func forgetPairs(_ request: VolumeReadFixture.Request) throws -> [FSBridgeForget] {
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        let pairs = try XCTUnwrap(object["forgets"])
        return try JSONDecoder().decode([FSBridgeForget].self, from: JSONSerialization.data(withJSONObject: pairs))
    }

    private func prepare(_ fixture: VolumeReadFixture, readOnly: Bool = false,
                         taskOptions: [String] = []) async throws -> (RepoReachVolume, FSItem) {
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: VolumeReadFixture.token))
        let volume = RepoReachVolume(client: client, identifier: UUID(), readOnly: readOnly)
        let root = try await activate(volume, taskOptions: taskOptions)
        if let error = root.error { throw error }
        let item = try await lookup(volume, root: try XCTUnwrap(root.item))
        return (volume, item)
    }

    private func activate(_ volume: RepoReachVolume, taskOptions: [String] = []) async throws -> ItemReply.Value {
        let activated = expectation(description: "Production volume activated")
        let rootReply = ItemReply()
        volume.activate(taskOptions: taskOptions) { item, error in rootReply.store(item: item, error: error); activated.fulfill() }
        await fulfillment(of: [activated], timeout: 3)
        return try XCTUnwrap(rootReply.value)
    }

    private func lookup(_ volume: RepoReachVolume, root: FSItem) async throws -> FSItem {
        let lookedUp = expectation(description: "Production file item looked up")
        let itemReply = ItemReply()
        volume.lookupItem(named: FSFileName(string: "binary"), inDirectory: root) { item, _, error in
            itemReply.store(item: item, error: error)
            lookedUp.fulfill()
        }
        await fulfillment(of: [lookedUp], timeout: 3)
        let file = try XCTUnwrap(itemReply.value)
        if let error = file.error { throw error }
        return try XCTUnwrap(file.item)
    }

    private func mount(_ volume: RepoReachVolume, taskOptions: [String] = []) async throws -> Error? {
        let finished = expectation(description: "Production mount callback")
        let reply = ReadReply()
        volume.mount(taskOptions: taskOptions) { error in reply.store(count: 0, error: error); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value).error
    }

    private func unmount(_ volume: RepoReachVolume) async throws {
        let finished = expectation(description: "Production unmount drained")
        let completed = CompletionFlag()
        volume.unmount { completed.complete(); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        guard completed.value else { throw FSBridgeError.timedOut }
    }

    private func deactivate(_ volume: RepoReachVolume) async throws -> Error? {
        let finished = expectation(description: "Production deactivate callback")
        let reply = ReadReply()
        volume.deactivate(options: []) { error in reply.store(count: 0, error: error); finished.fulfill() }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value).error
    }

    private func create(_ volume: RepoReachVolume, root: FSItem) async throws -> Error? {
        let finished = expectation(description: "Production create callback")
        let reply = ItemReply()
        volume.createItem(named: FSFileName(string: "new-file"), type: .file, inDirectory: root,
                          attributes: FSItem.SetAttributesRequest()) { item, _, error in
            reply.store(item: item, error: error); finished.fulfill()
        }
        await fulfillment(of: [finished], timeout: 3)
        let result = try XCTUnwrap(reply.value)
        XCTAssertNil(result.item)
        return result.error
    }

    private func setAttributes(_ volume: RepoReachVolume, item: FSItem,
                               attributes: FSItem.SetAttributesRequest) async throws -> Error? {
        let finished = expectation(description: "Production setattr callback")
        let reply = ReadReply()
        volume.setAttributes(attributes, on: item) { result, error in
            XCTAssertNil(result); reply.store(count: 0, error: error); finished.fulfill()
        }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value).error
    }

    private func write(_ volume: RepoReachVolume, item: FSItem, offset: off_t = 0, data: Data) async throws -> ReadReply.Value {
        let finished = expectation(description: "Production write callback")
        let reply = ReadReply()
        volume.write(contents: data, to: item, at: offset) { count, error in
            reply.store(count: count, error: error); finished.fulfill()
        }
        await fulfillment(of: [finished], timeout: 3)
        return try XCTUnwrap(reply.value)
    }

    private func assertWritePolicy(_ volume: RepoReachVolume, item: FSItem, readOnly: Bool) async throws {
        let error = try await open(volume, item: item, modes: .write)
        if readOnly { assertPOSIX(error, EROFS) }
        else {
            XCTAssertNil(error)
            let closeError = try await close(volume, item: item, modes: [])
            XCTAssertNil(closeError)
        }
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

private final class DirectoryCapture: @unchecked Sendable {
    struct Entry { let name: String; let cookie: UInt64; let attributes: FSItem.Attributes? }
    struct Value { let verifier: FSDirectoryVerifier; let error: Error?; let entries: [Entry] }
    private let lock = NSLock()
    private let capacity: Int
    private var entries: [Entry] = []
    private var stored: Value?
    init(capacity: Int) { self.capacity = capacity }
    var value: Value? { lock.lock(); defer { lock.unlock() }; return stored }
    func pack(name: String, cookie: UInt64, attributes: FSItem.Attributes?) -> Bool {
        lock.lock(); defer { lock.unlock() }
        guard entries.count < capacity else { return false }
        entries.append(Entry(name: name, cookie: cookie, attributes: attributes))
        return true
    }
    func finish(verifier: FSDirectoryVerifier, error: Error?) {
        lock.lock(); defer { lock.unlock() }
        stored = Value(verifier: verifier, error: error, entries: entries)
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
        let body: Data
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
    private var metadataAttempts: [String: Int] = [:]
    private let readResponse: (Request) -> Data?
    private let releaseResponse: (Int) -> Int32
    private let onOpen: (UInt64) -> Void
    private let metadataFailure: (Request, Int) -> Int32
    private let metadataResponse: ((Request) throws -> Data?)?
    var requests: [Request] { locked { recorded } }
    var failures: [String] { locked { recordedFailures } }

    init(read: @escaping (Request) -> Data?, release: @escaping (Int) -> Int32 = { _ in 0 },
         onOpen: @escaping (UInt64) -> Void = { _ in },
         metadataFailure: @escaping (Request, Int) -> Int32 = { _, _ in 0 },
         metadata: ((Request) throws -> Data?)? = nil) throws {
        directory = URL(fileURLWithPath: "/tmp/rr-fsv-" + UUID().uuidString.prefix(8), isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        socketPath = directory.appendingPathComponent("socket").path
        readResponse = read
        releaseResponse = release
        self.onOpen = onOpen
        self.metadataFailure = metadataFailure
        metadataResponse = metadata
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
            if request.operation != "read" && request.operation != "write" {
                let attempt = locked { () -> Int in
                    metadataAttempts[request.operation, default: 0] += 1
                    return metadataAttempts[request.operation]!
                }
                let failure = metadataFailure(request, attempt)
                if failure != 0 { Self.send(Self.error(failure), to: fd); return }
            }
            let response: Data?
            if let metadataResponse, request.operation != "read" && request.operation != "write" {
                response = try metadataResponse(request)
            } else {
                switch request.operation {
                case "read": response = readResponse(request)
                case "write": response = try Self.metadata(["written": request.body.count])
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
    static func metadata(_ fields: [String: Any]) throws -> Data {
        var body = fields
        body["version"] = 1
        body["errno"] = 0
        return http(body: try JSONSerialization.data(withJSONObject: body), contentType: "application/json", errno: 0, status: 200)
    }
    static func node(inode: UInt64, type: String, size: UInt64 = 3_145_728, sizeKnown: Bool? = nil) -> [String: Any] {
        var attributes: [String: Any] = ["size": size, "nlink": 1,
          "mode": type == "dir" ? 0o40755 : 0o100644, "type": type, "uid": getuid(), "gid": getgid(),
          "atime_ns": 0, "mtime_ns": 0, "ctime_ns": 0, "birthtime_ns": 0]
        if let sizeKnown { attributes["size_known"] = sizeKnown }
        return ["inode": inode, "generation": 1, "attributes": attributes]
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
              let length = Int(rawLength), length >= 0, length <= FSBridgeClient.maximumChunkSize else { throw FSBridgeError.invalidRequest }
        var body = Data(bytes[ending.upperBound...])
        while body.count < length { try receive(fd, into: &body) }
        guard body.count == length else { throw FSBridgeError.invalidRequest }
        if let url = URLComponents(string: String(first[1])),
           (first[0] == "GET" && url.path == "/v1/fs/read") || (first[0] == "PUT" && url.path == "/v1/fs/write") {
            let fields = Dictionary(uniqueKeysWithValues: (url.queryItems ?? []).map { ($0.name, $0.value ?? "") })
            return Request(operation: first[0] == "GET" ? "read" : "write", inode: fields["inode"].flatMap(UInt64.init),
                           handle: fields["handle"].flatMap(UInt64.init), access: nil, offset: fields["offset"].flatMap(UInt64.init),
                           size: first[0] == "GET" ? fields["size"].flatMap(Int.init) : body.count, body: body)
        }
        guard first[0] == "POST", first[1] == "/v1/fs",
              let fields = try JSONSerialization.jsonObject(with: body) as? [String: Any],
              let op = fields["op"] as? String else { throw FSBridgeError.invalidRequest }
        return Request(operation: op, inode: (fields["inode"] as? NSNumber)?.uint64Value,
                       handle: (fields["handle"] as? NSNumber)?.uint64Value, access: (fields["access"] as? NSNumber)?.uint32Value,
                       offset: (fields["offset"] as? NSNumber)?.uint64Value, size: nil, body: body)
    }
    private static func receive(_ fd: Int32, into bytes: inout Data) throws {
        var buffer = [UInt8](repeating: 0, count: 8192)
        let count = Darwin.recv(fd, &buffer, buffer.count, 0)
        guard count > 0 else { throw FSBridgeError.unavailable(Darwin.errno) }
        bytes.append(contentsOf: buffer.prefix(count))
    }
    private func locked<T>(_ body: () -> T) -> T { lock.lock(); defer { lock.unlock() }; return body() }
}
