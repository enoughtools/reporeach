import XCTest
import Foundation
import Darwin
#if canImport(Bridge)
@testable import Bridge
#elseif canImport(NativeFilesystem)
@testable import NativeFilesystem
#endif

final class FSBridgeTests: XCTestCase {
    private let token = String(repeating: "a", count: 64)

    func testConfigurationRequiresPrivateRegularVersionOneFile() throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = directory.appendingPathComponent("connection.json")
        let document = Data("{\"version\":1,\"socket\":\"/tmp/fixture.sock\",\"token\":\"\(token)\"}".utf8)
        try document.write(to: file)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
        let configuration = try FSBridgeConfiguration.load(from: file)
        XCTAssertEqual(configuration.socketPath, "/tmp/fixture.sock")
        XCTAssertFalse(configuration.debugDescription.contains(token))
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: file.path)
        XCTAssertThrowsError(try FSBridgeConfiguration.load(from: file))
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
        let link = directory.appendingPathComponent("link.json")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: file)
        XCTAssertThrowsError(try FSBridgeConfiguration.load(from: link))
        try Data("{\"version\":2,\"socket\":\"/tmp/fixture.sock\",\"token\":\"\(token)\"}".utf8).write(to: file)
        XCTAssertThrowsError(try FSBridgeConfiguration.load(from: file))
        let fifo = directory.appendingPathComponent("pipe.json")
        XCTAssertEqual(mkfifo(fifo.path, 0o600), 0)
        XCTAssertThrowsError(try FSBridgeConfiguration.load(from: fifo))
    }

    func testConfigurationRejectsHeaderInjectionAndOversizedSocketPaths() {
        XCTAssertThrowsError(try FSBridgeConfiguration(socketPath: "/tmp/socket", token: token + "\r\nInjected: yes"))
        XCTAssertThrowsError(try FSBridgeConfiguration(socketPath: "/tmp/socket", token: String(repeating: "A", count: 64)))
        XCTAssertThrowsError(try FSBridgeConfiguration(socketPath: "relative/socket", token: token))
        XCTAssertThrowsError(try FSBridgeConfiguration(socketPath: "/" + String(repeating: "é", count: 52), token: token))
        XCTAssertThrowsError(try FSBridgeConfiguration(socketPath: "/tmp/a\0b", token: token))
    }

    func testReconnectRereadsRotatedPrivateDescriptorAfterClose() async throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = directory.appendingPathComponent("connection.json")
        func publish(socket: String, token: String) throws {
            let body = try JSONSerialization.data(withJSONObject: ["version": 1, "socket": socket, "token": token])
            try body.write(to: file, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
        }
        try publish(socket: "/tmp/old-session.sock", token: token)
        let original = try FSBridgeClient(configURL: file)
        await original.close()
        let rotatedToken = String(repeating: "b", count: 64)
        let fixture = try BridgeSocketFixture { _ in Self.http(body: Data(#"{"version":1,"errno":0}"#.utf8)) }
        defer { fixture.stop() }
        try publish(socket: fixture.socketPath, token: rotatedToken)
        let fresh = try original.reconnected()
        _ = try await fresh.request(FSBridgeRequest(op: "getattr", inode: 1))
        XCTAssertEqual(try fixture.capturedRequest().headers["authorization"], "Bearer " + rotatedToken)
        await fresh.close()
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: file.path)
        XCTAssertThrowsError(try original.reconnected())
    }

    func testReconnectFromExplicitConfigurationHasIndependentTransport() async throws {
        let fixture = try BridgeSocketFixture { _ in Self.http(body: Data(#"{"version":1,"errno":0}"#.utf8)) }
        defer { fixture.stop() }
        let original = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        await original.close()
        let fresh = try original.reconnected()
        let response = try await fresh.request(FSBridgeRequest(op: "getattr", inode: 1))
        XCTAssertEqual(response.errno, 0)
        await fresh.close()
    }

    func testMetadataEncodingUsesExactSnakeCaseAndOmittedOptionals() throws {
        let request = FSBridgeRequest(op: "rename", oldParent: 9, oldName: "before", newParent: 10, newName: "after")
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: Any])
        XCTAssertEqual(Set(object.keys), ["version", "op", "old_parent", "old_name", "new_parent", "new_name"])
        XCTAssertEqual(object["version"] as? Int, 1)
        let changes = FSBridgeAttributeChanges(size: UInt64(Int64.max), mode: 0o640, uid: nil, gid: nil, atimeNS: nil, mtimeNS: -1)
        let changeObject = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(changes)) as? [String: Any])
        XCTAssertEqual(Set(changeObject.keys), ["size", "mode", "mtime_ns"])
        XCTAssertEqual(changeObject["mtime_ns"] as? Int, -1)
    }

    func testDecodesInodeGenerationAndNanosecondsWithoutPrecisionLoss() throws {
        let body = Data(#"{"version":1,"errno":0,"node":{"inode":18446744073709551615,"generation":42,"attributes":{"size":9223372036854775807,"nlink":2,"mode":33188,"type":"file","uid":501,"gid":20,"atime_ns":-1,"mtime_ns":1780000000123456789,"ctime_ns":1780000000123456788,"birthtime_ns":0}}}"#.utf8)
        let response = try JSONDecoder().decode(FSBridgeResponse.self, from: body)
        XCTAssertEqual(response.node?.inode, UInt64.max)
        XCTAssertEqual(response.node?.attributes.mtimeNS, 1_780_000_000_123_456_789)
        XCTAssertEqual(response.node?.attributes.atimeNS, -1)
    }

    func testSizeKnowledgePreservesUnknownAndExplicitKnownWireValues() throws {
        let fields = #""size":4096,"nlink":1,"mode":33188,"type":"file","uid":501,"gid":20,"atime_ns":0,"mtime_ns":0,"ctime_ns":0,"birthtime_ns":0"#
        let expectedKeys: Set<String> = ["size", "nlink", "mode", "type", "uid", "gid",
                                         "atime_ns", "mtime_ns", "ctime_ns", "birthtime_ns", "size_known"]
        for known in [false, true] {
            let body = Data("{\(fields),\"size_known\":\(known)}".utf8)
            let attributes = try JSONDecoder().decode(FSBridgeAttributes.self, from: body)
            XCTAssertEqual(attributes.sizeKnown, known)
            XCTAssertEqual(attributes.size, 4096)
            let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(attributes)) as? [String: Any])
            XCTAssertEqual(Set(object.keys), expectedKeys)
            XCTAssertEqual(object["size_known"] as? Bool, known)
        }
    }

    func testLegacyAttributesOmitSizeKnowledgeRatherThanEncodeNull() throws {
        let body = Data(#"{"size":4096,"nlink":1,"mode":33188,"type":"file","uid":501,"gid":20,"atime_ns":0,"mtime_ns":0,"ctime_ns":0,"birthtime_ns":0}"#.utf8)
        let attributes = try JSONDecoder().decode(FSBridgeAttributes.self, from: body)
        XCTAssertNil(attributes.sizeKnown)
        XCTAssertEqual(attributes.size, 4096)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(attributes)) as? [String: Any])
        XCTAssertEqual(Set(object.keys), ["size", "nlink", "mode", "type", "uid", "gid",
                                         "atime_ns", "mtime_ns", "ctime_ns", "birthtime_ns"])
        XCTAssertNil(object["size_known"])
    }

    func testBatchForgetEncodesOnlyNumericInodeAndReferencePairs() throws {
        let request = FSBridgeRequest(op: "batchforget", forgets: [
            FSBridgeForget(inode: 7, n: 2), FSBridgeForget(inode: UInt64.max, n: UInt64.max)
        ])
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: Any])
        XCTAssertEqual(Set(object.keys), ["version", "op", "forgets"])
        XCTAssertEqual(object["version"] as? Int, 1)
        XCTAssertEqual(object["op"] as? String, "batchforget")
        let pairs = try XCTUnwrap(object["forgets"] as? [[String: Any]])
        XCTAssertEqual(pairs.count, 2)
        for pair in pairs { XCTAssertEqual(Set(pair.keys), ["inode", "n"]) }
        XCTAssertEqual(pairs[0]["inode"] as? UInt64, 7)
        XCTAssertEqual(pairs[0]["n"] as? UInt64, 2)
        XCTAssertEqual(pairs[1]["inode"] as? UInt64, UInt64.max)
        XCTAssertEqual(pairs[1]["n"] as? UInt64, UInt64.max)
    }

    func testLegacyForgetOmitsOptionalBatchRatherThanEncodeNull() throws {
        let request = FSBridgeRequest(op: "forget", inode: 7, n: 2)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: Any])
        XCTAssertEqual(Set(object.keys), ["version", "op", "inode", "n"])
        XCTAssertEqual(object["inode"] as? UInt64, 7)
        XCTAssertEqual(object["n"] as? UInt64, 2)
        XCTAssertNil(object["forgets"])
    }

    func testOpenAccessIsAnOptionalNumericWireField() throws {
        for access: UInt32 in [1, 2, 3] {
            let request = FSBridgeRequest(op: "open", inode: 7, access: access)
            let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: Any])
            XCTAssertEqual(object["access"] as? UInt32, access)
            XCTAssertEqual(Set(object.keys), ["version", "op", "inode", "access"])
        }
        let defaultRequest = FSBridgeRequest(op: "open", inode: 7)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(defaultRequest)) as? [String: Any])
        XCTAssertNil(object["access"])
    }

    func testMetadataSendsAuthenticationOnlyInHeaderAndPropagatesErrno() async throws {
        let fixture = try BridgeSocketFixture { _ in
            Self.http(body: Data("{\"version\":1,\"errno\":\(ENOENT)}".utf8), errno: ENOENT)
        }
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        do {
            _ = try await client.request(FSBridgeRequest(op: "lookup", parent: 1, name: "missing"))
            XCTFail("Expected an inode lookup failure")
        } catch { XCTAssertEqual(error as? FSBridgeError, .filesystem(ENOENT)) }
        let captured = try fixture.capturedRequest()
        XCTAssertEqual(captured.headers["authorization"], "Bearer " + token)
        XCTAssertFalse(captured.target.contains(token))
        XCTAssertNil(captured.body.range(of: Data(token.utf8)))
        XCTAssertEqual(captured.method, "POST")
        XCTAssertEqual(captured.target, "/v1/fs")
        await client.close()
    }

    func testBinaryReadPreservesAllBytes() async throws {
        let bytes = Data((0...255).map(UInt8.init)) + Data([0, 255, 254, 128])
        let fixture = try BridgeSocketFixture { _ in Self.http(body: bytes, contentType: "application/octet-stream") }
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let received = try await client.read(inode: 7, handle: 11, offset: 13, size: bytes.count)
        XCTAssertEqual(received, bytes)
        let request = try fixture.capturedRequest()
        XCTAssertEqual(request.target, "/v1/fs/read?inode=7&handle=11&offset=13&size=260")
        XCTAssertTrue(request.body.isEmpty)
        await client.close()
    }

    func testBinaryWriteDoesNotConvertPayloadToTextOrRetry() async throws {
        let bytes = Data([0, 255, 254, 128, 13, 10, 0, 97])
        let fixture = try BridgeSocketFixture { request in
            Self.http(body: Data("{\"version\":1,\"errno\":0,\"written\":\(request.body.count)}".utf8))
        }
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let written = try await client.write(inode: 7, handle: 11, offset: 13, data: bytes)
        XCTAssertEqual(written, bytes.count)
        let request = try fixture.capturedRequest()
        XCTAssertEqual(request.method, "PUT")
        XCTAssertEqual(request.body, bytes)
        XCTAssertEqual(request.headers["content-type"], "application/octet-stream")
        await client.close()
    }

    func testRejectsOversizedAndOverflowingChunksBeforeConnecting() async throws {
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: "/tmp/nonexistent-rr.sock", token: token))
        for (offset, size) in [(UInt64(0), -1), (0, FSBridgeClient.maximumChunkSize + 1), (UInt64(Int64.max), 1)] {
            do { _ = try await client.read(inode: 1, handle: 0, offset: offset, size: size); XCTFail("Expected invalid bounds") }
            catch { XCTAssertEqual(error as? FSBridgeError, .invalidRequest) }
        }
        do { _ = try await client.write(inode: 1, handle: 0, offset: UInt64.max, data: Data()); XCTFail("Expected invalid offset") }
        catch { XCTAssertEqual(error as? FSBridgeError, .invalidRequest) }
        await client.close()
    }

    func testZeroLengthWriteAcceptsSuccessfulOmittedCount() async throws {
        let fixture = try BridgeSocketFixture { _ in Self.http(body: Data(#"{"version":1,"errno":0}"#.utf8)) }
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let written = try await client.write(inode: 7, handle: 11, offset: 0, data: Data())
        XCTAssertEqual(written, 0)
        XCTAssertTrue(try fixture.capturedRequest().body.isEmpty)
        await client.close()
    }

    func testXattrBinaryOperationsPreserveEncodedNamesValuesAndPolicies() async throws {
        let name = "com.example.-_~/a?b&c=é%#\r\n"
        let target = "/v1/fs/xattr?inode=7&name=com.example.-_~%2Fa%3Fb%26c%3D%C3%A9%25%23%0D%0A"
        let values = [Data(), Data((0...255).map(UInt8.init)) + Data([0, 255, 13, 10]),
                      Data(repeating: 255, count: 1_048_576)]
        for value in values {
            let fixture = try BridgeSocketFixture { _ in Self.http(body: value, contentType: "application/octet-stream") }
            defer { fixture.stop() }
            let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
            let received = try await client.getXattr(inode: 7, name: name)
            XCTAssertEqual(received, value)
            let request = try fixture.capturedRequest()
            XCTAssertEqual(request.method, "GET")
            XCTAssertEqual(request.target, target)
            XCTAssertTrue(request.body.isEmpty)
            XCTAssertEqual(request.headers["authorization"], "Bearer " + token)
            await client.close()
        }
        let policies: [(FSBridgeXattrPolicy, String)] = [
            (.alwaysSet, "always_set"), (.mustCreate, "must_create"), (.mustReplace, "must_replace")
        ]
        for (policy, wirePolicy) in policies {
            for value in values {
                let fixture = try BridgeSocketFixture { request in
                    let body = "{\"version\":1,\"errno\":0,\"written\":\(request.body.count)}"
                    return Self.http(body: Data(body.utf8))
                }
                defer { fixture.stop() }
                let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
                try await client.setXattr(inode: 7, name: name, value: value, policy: policy)
                let request = try fixture.capturedRequest()
                XCTAssertEqual(request.method, "PUT")
                XCTAssertEqual(request.target, target + "&policy=" + wirePolicy)
                XCTAssertEqual(request.body, value)
                XCTAssertEqual(request.headers["content-type"], "application/octet-stream")
                XCTAssertEqual(request.headers["authorization"], "Bearer " + token)
                XCTAssertFalse(request.target.contains(token))
                await client.close()
            }
        }
    }

    func testXattrRejectsMalformedBinaryResponsesAndInexactAcknowledgements() async throws {
        let responses: [(Data, FSBridgeError)] = [
            (Data("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 0\r\n\r\n".utf8), .malformedResponse),
            (Self.http(body: Data([1]), contentType: "application/json"), .malformedResponse),
            (Data("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nX-RepoReach-Errno: 0\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\n\r\n".utf8), .malformedResponse),
            (Data("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nX-RepoReach-Errno: 0\r\nContent-Length: 2\r\n\r\nx".utf8), .malformedResponse),
            (Data("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nX-RepoReach-Errno: 0\r\nContent-Length: 1048577\r\n\r\n".utf8), .responseTooLarge)
        ]
        for (response, expected) in responses {
            let fixture = try BridgeSocketFixture { _ in response }
            defer { fixture.stop() }
            let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
            await assertBridgeError(expected) { _ = try await client.getXattr(inode: 7, name: "com.example.binary") }
            await client.close()
        }
        let acknowledgements = [
            #"{"version":1,"errno":0}"#,
            #"{"version":1,"errno":0,"written":-1}"#,
            #"{"version":1,"errno":0,"written":1}"#,
            #"{"version":1,"errno":0,"written":3}"#,
            #"{"version":2,"errno":0,"written":2}"#
        ]
        for body in acknowledgements {
            let fixture = try BridgeSocketFixture { _ in Self.http(body: Data(body.utf8)) }
            defer { fixture.stop() }
            let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
            await assertBridgeError(.malformedResponse) {
                try await client.setXattr(inode: 7, name: "com.example.binary", value: Data([0, 255]), policy: .alwaysSet)
            }
            await client.close()
        }
        for body in [#"{"version":1,"errno":0}"#, #"{"version":1,"errno":0,"written":1}"#] {
            let fixture = try BridgeSocketFixture { _ in Self.http(body: Data(body.utf8)) }
            defer { fixture.stop() }
            let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
            await assertBridgeError(.malformedResponse) {
                try await client.setXattr(inode: 7, name: "com.example.empty", value: Data(), policy: .alwaysSet)
            }
            await client.close()
        }
    }

    func testXattrMetadataPreservesExactWireFieldsAndUTF8NameIdentity() async throws {
        let names = ["com.example.\u{00E9}", "com.example.e\u{0301}", "com.example./?&=\r\n"]
        let body = try JSONSerialization.data(withJSONObject: ["version": 1, "errno": 0, "xattr_names": names])
        let listFixture = try BridgeSocketFixture { _ in Self.http(body: body) }
        defer { listFixture.stop() }
        let listClient = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: listFixture.socketPath, token: token))
        let received = try await listClient.listXattrs(inode: 7)
        // Swift String equality normalizes Unicode; metadata identity is its exact UTF-8 spelling.
        XCTAssertEqual(received.map { Data($0.utf8) }, names.map { Data($0.utf8) })
        XCTAssertNotEqual(Data(received[0].utf8), Data(received[1].utf8))
        let listRequest = try listFixture.capturedRequest()
        XCTAssertEqual(listRequest.method, "POST")
        XCTAssertEqual(listRequest.target, "/v1/fs")
        let listObject = try XCTUnwrap(JSONSerialization.jsonObject(with: listRequest.body) as? [String: Any])
        XCTAssertEqual(Set(listObject.keys), ["version", "op", "inode"])
        XCTAssertEqual(listObject["version"] as? Int, 1)
        XCTAssertEqual(listObject["op"] as? String, "listxattr")
        XCTAssertEqual(listObject["inode"] as? Int, 7)
        await listClient.close()

        let escapedNames = (0..<128).map {
            String(format: "%03d", $0) + String(repeating: "\u{0001}", count: 124)
        }
        XCTAssertTrue(escapedNames.allSatisfy { $0.utf8.count == 127 })
        let escapedBody = try JSONSerialization.data(withJSONObject: ["version": 1, "errno": 0, "xattr_names": escapedNames])
        XCTAssertGreaterThan(escapedBody.count, 65_536)
        let escapedFixture = try BridgeSocketFixture { _ in Self.http(body: escapedBody) }
        defer { escapedFixture.stop() }
        let escapedClient = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: escapedFixture.socketPath, token: token))
        let escapedReceived = try await escapedClient.listXattrs(inode: 7)
        XCTAssertEqual(escapedReceived.map { Data($0.utf8) }, escapedNames.map { Data($0.utf8) })
        await escapedClient.close()

        let removeFixture = try BridgeSocketFixture { _ in Self.http(body: Data(#"{"version":1,"errno":0}"#.utf8)) }
        defer { removeFixture.stop() }
        let removeClient = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: removeFixture.socketPath, token: token))
        try await removeClient.removeXattr(inode: 7, name: names[2])
        let removeRequest = try removeFixture.capturedRequest()
        XCTAssertEqual(removeRequest.method, "POST")
        XCTAssertEqual(removeRequest.target, "/v1/fs")
        XCTAssertEqual(removeRequest.headers["authorization"], "Bearer " + token)
        let removeObject = try XCTUnwrap(JSONSerialization.jsonObject(with: removeRequest.body) as? [String: Any])
        XCTAssertEqual(Set(removeObject.keys), ["version", "op", "inode", "name"])
        XCTAssertEqual(removeObject["version"] as? Int, 1)
        XCTAssertEqual(removeObject["op"] as? String, "removexattr")
        XCTAssertEqual(removeObject["inode"] as? Int, 7)
        XCTAssertEqual(Data(try XCTUnwrap(removeObject["name"] as? String).utf8), Data(names[2].utf8))
        await removeClient.close()
    }

    func testXattrRejectsInvalidListNamesAndExactUTF8Duplicates() async throws {
        let invalidNames = [
            [""], ["com.example.a\0b"], [String(repeating: "a", count: 128)],
            [String(repeating: "é", count: 64)], ["com.example.same", "com.example.same"],
            (0..<129).map { "com.example.\($0)" }
        ]
        var bodies = try invalidNames.map {
            try JSONSerialization.data(withJSONObject: ["version": 1, "errno": 0, "xattr_names": $0])
        }
        bodies.append(contentsOf: [
            Data(#"{"version":1,"errno":0}"#.utf8),
            Data(#"{"version":1,"errno":0,"xattr_names":null}"#.utf8),
            Data(#"{"version":1,"errno":0,"xattr_names":"com.example.name"}"#.utf8),
            Data(#"{"version":1,"errno":0,"xattr_names":[1]}"#.utf8)
        ])
        for body in bodies {
            let fixture = try BridgeSocketFixture { _ in Self.http(body: body) }
            defer { fixture.stop() }
            let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
            await assertBridgeError(.malformedResponse) { _ = try await client.listXattrs(inode: 7) }
            await client.close()
        }
    }

    func testXattrMissingTagIsOperationSpecificAndFollowsNumericErrnoValidation() async throws {
        // Linux ENODATA is 61; Darwin ENOATTR has a different value.
        let foreignMissingErrno: Int32 = 61
        let cases: [(Int32, Int32, Bool?, FSBridgeError)] = [
            (foreignMissingErrno, foreignMissingErrno, true, .filesystem(ENOATTR)),
            (foreignMissingErrno, foreignMissingErrno, nil, .filesystem(foreignMissingErrno)),
            (foreignMissingErrno, ENOENT, true, .malformedResponse),
            (0, 0, true, .malformedResponse)
        ]
        for operation in ["get", "set", "list", "remove", "getattr"] {
            for (errno, headerErrno, missing, xattrExpected) in cases {
                var object: [String: Any] = ["version": 1, "errno": errno]
                if let missing { object["xattr_missing"] = missing }
                let body = try JSONSerialization.data(withJSONObject: object)
                let fixture = try BridgeSocketFixture { _ in Self.http(body: body, errno: headerErrno, status: 404) }
                defer { fixture.stop() }
                let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
                let expected = operation == "getattr" && missing == true && errno != 0 && errno == headerErrno ?
                    FSBridgeError.filesystem(foreignMissingErrno) : xattrExpected
                await assertBridgeError(expected) {
                    switch operation {
                    case "get": _ = try await client.getXattr(inode: 7, name: "com.example.missing")
                    case "set": try await client.setXattr(inode: 7, name: "com.example.missing", value: Data(), policy: .mustReplace)
                    case "list": _ = try await client.listXattrs(inode: 7)
                    case "remove": try await client.removeXattr(inode: 7, name: "com.example.missing")
                    default: _ = try await client.request(FSBridgeRequest(op: "getattr", inode: 7))
                    }
                }
                await client.close()
            }
        }
    }

    func testXattrBoundsAreCheckedBeforeConnectingAndUseUTF8Bytes() async throws {
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: "/tmp/nonexistent-rr-xattr.sock", token: token))
        for name in ["", "a\0b", String(repeating: "a", count: 128), String(repeating: "é", count: 64)] {
            await assertBridgeError(.invalidRequest) { _ = try await client.getXattr(inode: 7, name: name) }
            await assertBridgeError(.invalidRequest) { try await client.setXattr(inode: 7, name: name, value: Data(), policy: .alwaysSet) }
            await assertBridgeError(.invalidRequest) { try await client.removeXattr(inode: 7, name: name) }
        }
        await assertBridgeError(.invalidRequest) { _ = try await client.getXattr(inode: 0, name: "com.example.name") }
        await assertBridgeError(.invalidRequest) { try await client.setXattr(inode: 0, name: "com.example.name", value: Data(), policy: .alwaysSet) }
        await assertBridgeError(.invalidRequest) { _ = try await client.listXattrs(inode: 0) }
        await assertBridgeError(.invalidRequest) { try await client.removeXattr(inode: 0, name: "com.example.name") }
        await assertBridgeError(.invalidRequest) {
            try await client.setXattr(inode: 7, name: "com.example.large",
                                      value: Data(repeating: 0, count: 1_048_577), policy: .alwaysSet)
        }
        await client.close()

        let boundaryName = String(repeating: "é", count: 63) + "a"
        XCTAssertEqual(boundaryName.utf8.count, 127)
        let fixture = try BridgeSocketFixture { _ in Self.http(body: Data(), contentType: "application/octet-stream") }
        defer { fixture.stop() }
        let boundaryClient = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let value = try await boundaryClient.getXattr(inode: UInt64.max, name: boundaryName)
        XCTAssertTrue(value.isEmpty)
        XCTAssertTrue(try fixture.capturedRequest().target.hasPrefix("/v1/fs/xattr?inode=18446744073709551615&name="))
        await boundaryClient.close()
    }

    func testRejectsMismatchedErrnoAndResponseVersion() async throws {
        for body in [#"{"version":1,"errno":2}"#, #"{"version":2,"errno":0}"#] {
            let fixture = try BridgeSocketFixture { _ in Self.http(body: Data(body.utf8), errno: 0) }
            let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
            do { _ = try await client.request(FSBridgeRequest(op: "getattr", inode: 1)); XCTFail("Expected malformed response") }
            catch { XCTAssertEqual(error as? FSBridgeError, .malformedResponse) }
            await client.close()
            fixture.stop()
        }
    }

    func testRejectsOversizedDeclaredLengthAndAmbiguousFraming() async throws {
        let responses = [
            "HTTP/1.1 200 OK\r\nContent-Length: 99999999\r\n\r\n",
            "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nTransfer-Encoding: chunked\r\n\r\n",
            "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n"
        ]
        for (index, response) in responses.enumerated() {
            let fixture = try BridgeSocketFixture { _ in Data(response.utf8) }
            let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
            do { _ = try await client.request(FSBridgeRequest(op: "getattr", inode: 1)); XCTFail("Expected invalid framing") }
            catch { XCTAssertEqual(error as? FSBridgeError, index == 0 ? .responseTooLarge : .malformedResponse) }
            await client.close()
            fixture.stop()
        }
    }

    func testChunkedMetadataIsDecodedButChunkedBinaryIsRejected() async throws {
        let body = Data(#"{"version":1,"errno":0}"#.utf8)
        var chunked = Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nX-RepoReach-Errno: 0\r\nContent-Type: application/octet-stream\r\n\r\n\(String(body.count, radix: 16))\r\n".utf8)
        chunked.append(body)
        chunked.append(Data("\r\n0\r\n\r\n".utf8))
        let fixture = try BridgeSocketFixture { _ in chunked }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let response = try await client.request(FSBridgeRequest(op: "getattr", inode: 1))
        XCTAssertEqual(response.errno, 0)
        await client.close()
        fixture.stop()
        let binaryFixture = try BridgeSocketFixture { _ in chunked }
        let binaryClient = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: binaryFixture.socketPath, token: token))
        do { _ = try await binaryClient.read(inode: 1, handle: 0, offset: 0, size: body.count); XCTFail("Expected explicit binary length") }
        catch { XCTAssertEqual(error as? FSBridgeError, .malformedResponse) }
        await binaryClient.close()
        binaryFixture.stop()
    }

    func testDeadlineCancelsSilentConnection() async throws {
        let fixture = try BridgeSocketFixture { _ in nil }
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let started = ProcessInfo.processInfo.systemUptime
        do { _ = try await client.request(FSBridgeRequest(op: "getattr", inode: 1), timeout: 0.05); XCTFail("Expected timeout") }
        catch { XCTAssertEqual(error as? FSBridgeError, .timedOut) }
        XCTAssertLessThan(ProcessInfo.processInfo.systemUptime - started, 2)
        await client.close()
    }

    func testTaskCancellationAndClientCloseDrainConnections() async throws {
        let accepted = expectation(description: "Filesystem request accepted")
        let fixture = try BridgeSocketFixture { _ in accepted.fulfill(); return nil }
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let operation = Task { try await client.request(FSBridgeRequest(op: "getattr", inode: 1)) }
        await fulfillment(of: [accepted], timeout: 2)
        operation.cancel()
        do { _ = try await operation.value; XCTFail("Expected cancellation") }
        catch { XCTAssertEqual(error as? FSBridgeError, .cancelled) }
        await client.close()
        do { _ = try await client.request(FSBridgeRequest(op: "getattr", inode: 1)); XCTFail("Expected closed client") }
        catch { XCTAssertEqual(error as? FSBridgeError, .unavailable(ENOTCONN)) }
    }

    func testCloseCancelsActiveRequestAndDrainsWorker() async throws {
        let accepted = expectation(description: "Active request accepted before close")
        let fixture = try BridgeSocketFixture { _ in accepted.fulfill(); return nil }
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let operation = Task { try await client.request(FSBridgeRequest(op: "getattr", inode: 1)) }
        await fulfillment(of: [accepted], timeout: 2)
        await client.close()
        do { _ = try await operation.value; XCTFail("Expected close to cancel active request") }
        catch { XCTAssertEqual(error as? FSBridgeError, .cancelled) }
    }

    func testQueuedCancellationCompletesWhileAllWorkersAreBusy() async throws {
        let accepted = expectation(description: "Eight worker sockets are occupied")
        accepted.expectedFulfillmentCount = 8
        let fixture = try BridgeStallingFixture(onConnect: { accepted.fulfill() })
        defer { fixture.stop() }
        let client = FSBridgeClient(configuration: try FSBridgeConfiguration(socketPath: fixture.socketPath, token: token))
        let active = (0..<8).map { _ in Task { try await client.request(FSBridgeRequest(op: "getattr", inode: 1)) } }
        await fulfillment(of: [accepted], timeout: 2)
        let queuedStarted = expectation(description: "Queued task started")
        let queuedFinished = expectation(description: "Queued cancellation completes promptly")
        let queued = Task {
            queuedStarted.fulfill()
            do {
                _ = try await client.request(FSBridgeRequest(op: "getattr", inode: 1))
                XCTFail("Expected queued request cancellation")
            } catch { XCTAssertEqual(error as? FSBridgeError, .cancelled) }
            queuedFinished.fulfill()
        }
        await fulfillment(of: [queuedStarted], timeout: 2)
        queued.cancel()
        await fulfillment(of: [queuedFinished], timeout: 2)
        await client.close()
        await queued.value
        for task in active { _ = try? await task.value }
    }

    private func assertBridgeError(_ expected: FSBridgeError, file: StaticString = #filePath, line: UInt = #line,
                                   operation: () async throws -> Void) async {
        do { try await operation(); XCTFail("Expected bridge failure", file: file, line: line) }
        catch { XCTAssertEqual(error as? FSBridgeError, expected, file: file, line: line) }
    }

    private static func http(body: Data, contentType: String = "application/json", errno: Int32 = 0, status: Int = 200) -> Data {
        var response = Data("HTTP/1.1 \(status) Fixture\r\nContent-Type: \(contentType)\r\nContent-Length: \(body.count)\r\nX-RepoReach-Errno: \(errno)\r\nConnection: close\r\n\r\n".utf8)
        response.append(body)
        return response
    }

    private func testDirectory() throws -> URL {
        let directory = URL(fileURLWithPath: "/tmp/rr-fsb-" + UUID().uuidString.prefix(8), isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                                attributes: [.posixPermissions: 0o700])
        return directory
    }
}

/// Occupies every transport worker without sending response bytes. This lets a
/// cancellation test distinguish queued cancellation from socket cancellation.
private final class BridgeStallingFixture: @unchecked Sendable {
    let socketPath: String
    private let directory: URL
    private let listener: Int32
    private let lock = NSLock()
    private var connections: [Int32] = []
    private var stopped = false
    private let finished = DispatchSemaphore(value: 0)

    init(onConnect: @escaping () -> Void) throws {
        directory = URL(fileURLWithPath: "/tmp/rr-fsq-" + UUID().uuidString.prefix(8), isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                                attributes: [.posixPermissions: 0o700])
        socketPath = directory.appendingPathComponent("socket").path
        listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard listener >= 0 else { throw FSBridgeError.unavailable(Darwin.errno) }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(socketPath.utf8) + [0]) }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0, Darwin.listen(listener, 32) == 0 else {
            Darwin.close(listener)
            try? FileManager.default.removeItem(at: directory)
            throw FSBridgeError.unavailable(Darwin.errno)
        }
        DispatchQueue.global(qos: .userInitiated).async {
            defer { self.finished.signal() }
            while true {
                let fd = Darwin.accept(self.listener, nil, nil)
                guard fd >= 0 else { return }
                self.lock.lock()
                if self.stopped { Darwin.close(fd); self.lock.unlock(); return }
                self.connections.append(fd)
                self.lock.unlock()
                onConnect()
            }
        }
    }

    func stop() {
        lock.lock()
        if stopped { lock.unlock(); return }
        stopped = true
        for fd in connections { Darwin.shutdown(fd, SHUT_RDWR); Darwin.close(fd) }
        connections.removeAll()
        Darwin.shutdown(listener, SHUT_RDWR)
        Darwin.close(listener)
        lock.unlock()
        _ = finished.wait(timeout: .now() + 2)
        try? FileManager.default.removeItem(at: directory)
    }
}

/// A single-request HTTP fixture on a private UNIX socket. Binary request bytes
/// stay in Data; only the HTTP header is decoded as text.
private final class BridgeSocketFixture: @unchecked Sendable {
    struct Request {
        let method: String
        let target: String
        let headers: [String: String]
        let body: Data
    }
    let socketPath: String
    private let directory: URL
    private let listener: Int32
    private let lock = NSLock()
    private var captured: Request?
    private var failure: Error?
    private var connection: Int32 = -1
    private var stopped = false
    private let finished = DispatchSemaphore(value: 0)

    init(response: @escaping (Request) -> Data?) throws {
        directory = URL(fileURLWithPath: "/tmp/rr-fsf-" + UUID().uuidString.prefix(8), isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false,
                                                attributes: [.posixPermissions: 0o700])
        socketPath = directory.appendingPathComponent("socket").path
        listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard listener >= 0 else { throw FSBridgeError.unavailable(Darwin.errno) }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(socketPath.utf8) + [0]) }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0, Darwin.listen(listener, 1) == 0 else {
            Darwin.close(listener)
            try? FileManager.default.removeItem(at: directory)
            throw FSBridgeError.unavailable(Darwin.errno)
        }
        DispatchQueue.global(qos: .userInitiated).async {
            defer { self.finished.signal() }
            let fd = Darwin.accept(self.listener, nil, nil)
            guard fd >= 0 else { return }
            self.lock.lock()
            self.connection = fd
            let stopped = self.stopped
            self.lock.unlock()
            defer {
                self.lock.lock()
                self.connection = -1
                Darwin.close(fd)
                self.lock.unlock()
            }
            if stopped { return }
            var noSigPipe: Int32 = 1
            _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
            do {
                let request = try Self.readRequest(fd)
                self.lock.lock()
                self.captured = request
                self.lock.unlock()
                if let bytes = response(request) {
                    var sent = 0
                    while sent < bytes.count {
                        let count = bytes.withUnsafeBytes { Darwin.send(fd, $0.baseAddress!.advanced(by: sent), bytes.count - sent, 0) }
                        if count < 0 && Darwin.errno == EINTR { continue }
                        guard count > 0 else { return }
                        sent += count
                    }
                } else {
                    var byte: UInt8 = 0
                    while Darwin.recv(fd, &byte, 1, 0) > 0 {}
                }
            } catch {
                self.lock.lock()
                self.failure = error
                self.lock.unlock()
            }
        }
    }

    func capturedRequest() throws -> Request {
        lock.lock()
        defer { lock.unlock() }
        if let failure { throw failure }
        guard let captured else { throw FSBridgeError.malformedResponse }
        return captured
    }

    func stop() {
        lock.lock()
        if stopped { lock.unlock(); return }
        stopped = true
        if connection >= 0 { Darwin.shutdown(connection, SHUT_RDWR) }
        Darwin.shutdown(listener, SHUT_RDWR)
        Darwin.close(listener)
        lock.unlock()
        _ = finished.wait(timeout: .now() + 2)
        try? FileManager.default.removeItem(at: directory)
    }

    private static func readRequest(_ fd: Int32) throws -> Request {
        let separator = Data([13, 10, 13, 10])
        var buffer = Data()
        while buffer.range(of: separator) == nil {
            guard buffer.count <= 16_384 else { throw FSBridgeError.responseTooLarge }
            try receive(fd, into: &buffer)
        }
        let ending = buffer.range(of: separator)!
        guard let header = String(data: buffer[..<ending.lowerBound], encoding: .ascii) else { throw FSBridgeError.malformedResponse }
        let lines = header.components(separatedBy: "\r\n")
        let first = lines[0].split(separator: " ")
        guard first.count == 3 else { throw FSBridgeError.malformedResponse }
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":") else { throw FSBridgeError.malformedResponse }
            headers[line[..<colon].lowercased()] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        guard let rawLength = headers["content-length"], let count = Int(rawLength), count >= 0,
              count <= FSBridgeClient.maximumChunkSize else { throw FSBridgeError.malformedResponse }
        var body = Data(buffer[ending.upperBound...])
        while body.count < count { try receive(fd, into: &body) }
        guard body.count == count else { throw FSBridgeError.malformedResponse }
        return Request(method: String(first[0]), target: String(first[1]), headers: headers, body: body)
    }

    private static func receive(_ fd: Int32, into buffer: inout Data) throws {
        var bytes = [UInt8](repeating: 0, count: 8_192)
        let count = Darwin.recv(fd, &bytes, bytes.count, 0)
        guard count > 0 else { throw FSBridgeError.unavailable(Darwin.errno) }
        buffer.append(contentsOf: bytes.prefix(count))
    }
}
