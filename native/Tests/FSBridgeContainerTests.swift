import Darwin
import Foundation
import XCTest
#if canImport(Bridge)
@testable import Bridge
#endif

final class FSBridgeContainerTests: XCTestCase {
    private let group = "AB12CD34EF.rr"

    func testNonNilContainerDoesNotAuthorizeAnUnclaimedGroup() throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        for claims: Any? in [nil, [], ["ZZZZZZZZZZ.rr"], [group: true], [1]] {
            XCTAssertThrowsError(try FSBridgeContainer.validated(groupIdentifier: group, signedGroups: claims, teamIdentifier: "AB12CD34EF", containerURL: directory))
        }
        for team in [nil, "ZZZZZZZZZZ", "", "AB12CD34EF.rr"] {
            XCTAssertThrowsError(try FSBridgeContainer.validated(groupIdentifier: group, signedGroups: [group], teamIdentifier: team, containerURL: directory))
        }
        XCTAssertThrowsError(try FSBridgeContainer.validated(groupIdentifier: group, signedGroups: [group], teamIdentifier: "AB12CD34EF", containerURL: nil))
        XCTAssertThrowsError(try FSBridgeContainer.validated(groupIdentifier: group, signedGroups: [group], teamIdentifier: "AB12CD34EF", containerURL: URL(string: "https://example.com")))
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: directory.path), [])
    }

    func testGroupIdentifierRequiresTheExactResolvedTeamFormat() throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        for identifier in ["", "$(DEVELOPMENT_TEAM).rr", "AB12CD34EF", "AB12CD34EF.other", "ab12cd34ef.rr",
                           "AB12CD34E.rr", "AB12CD34EFF.rr", "AB12CD34ÉF.rr", "AB12CD34EF.rr\0"] {
            XCTAssertThrowsError(try FSBridgeContainer.validated(groupIdentifier: identifier, signedGroups: [identifier], teamIdentifier: "AB12CD34EF", containerURL: directory), identifier)
        }
    }

    func testPrivateOwnedCanonicalDirectoryHasRealAccessAndLeavesNoProbeFile() throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let container = try resolve(directory)
        XCTAssertEqual(container.groupIdentifier, group)
        XCTAssertEqual(container.directory.path, directory.path)
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: directory.path), [])
        let link = directory.appendingPathComponent("linked")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: directory)
        XCTAssertThrowsError(try resolve(link))
        let file = directory.appendingPathComponent("regular")
        try Data([1]).write(to: file)
        XCTAssertThrowsError(try resolve(file))
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: directory.path)
        XCTAssertThrowsError(try resolve(directory))
        try FileManager.default.setAttributes([.posixPermissions: 0o500], ofItemAtPath: directory.path)
        XCTAssertThrowsError(try resolve(directory))
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
    }

    func testContainerReservesSocketCapacityUsingUTF8Bytes() throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let oversized = directory.appendingPathComponent(String(repeating: "é", count: 40), isDirectory: true)
        try FileManager.default.createDirectory(at: oversized, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        XCTAssertLessThan(oversized.path.count + 18, MemoryLayout.size(ofValue: sockaddr_un().sun_path))
        XCTAssertGreaterThanOrEqual(oversized.path.utf8.count + 18, MemoryLayout.size(ofValue: sockaddr_un().sun_path))
        XCTAssertThrowsError(try resolve(oversized))
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: oversized.path), [])
    }

    func testSocketMustBeThePrivateDirectChildWithCanonicalStateName() throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let container = try resolve(directory)
        let socket = directory.appendingPathComponent("b0123456789abcdef")
        let listener = try bindSocket(socket)
        defer { Darwin.close(listener) }
        try container.validateSocket(path: socket.path)
        for path in [directory.path + "/./b0123456789abcdef", directory.path + "/../b0123456789abcdef",
                     directory.path + "//b0123456789abcdef", directory.path + "/nested/b0123456789abcdef",
                     directory.path + "/b0123456789abcdeF", directory.path + "/b0123456789abcdef0",
                     directory.path + "/b0123456789abcdef\0", "/private/tmp/b0123456789abcdef"] {
            XCTAssertThrowsError(try container.validateSocket(path: path), path)
        }
        try FileManager.default.setAttributes([.posixPermissions: 0o666], ofItemAtPath: socket.path)
        XCTAssertThrowsError(try container.validateSocket(path: socket.path))
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: socket.path)
        let link = directory.appendingPathComponent("b1111111111111111")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: socket)
        XCTAssertThrowsError(try container.validateSocket(path: link.path))
        let regular = directory.appendingPathComponent("b2222222222222222")
        try Data([1]).write(to: regular)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: regular.path)
        XCTAssertThrowsError(try container.validateSocket(path: regular.path))
    }

    func testDescriptorAndReconnectKeepTheContainerRestriction() async throws {
        let directory = try testDirectory()
        defer { try? FileManager.default.removeItem(at: directory) }
        let container = try resolve(directory)
        let socket = directory.appendingPathComponent("b0123456789abcdef")
        let listener = try bindSocket(socket)
        defer { Darwin.close(listener) }
        let descriptor = directory.appendingPathComponent("connection.json")
        func publish(_ socketPath: String) throws {
            let bytes = try JSONSerialization.data(withJSONObject: ["version": 1, "socket": socketPath,
                                                                   "token": String(repeating: "a", count: 64)])
            try bytes.write(to: descriptor, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: descriptor.path)
        }
        try publish(socket.path)
        let client = try FSBridgeClient(configURL: descriptor, container: container)
        await client.close()
        let reconnected = try client.reconnected()
        await reconnected.close()
        try publish("/tmp/outside-group.sock")
        XCTAssertThrowsError(try client.reconnected())
        XCTAssertThrowsError(try FSBridgeConfiguration.load(from: descriptor, container: container))
        // Standalone bridge tooling intentionally does not claim an app group.
        XCTAssertEqual(try FSBridgeConfiguration.load(from: descriptor).socketPath, "/tmp/outside-group.sock")
    }

    private func resolve(_ directory: URL) throws -> FSBridgeContainer {
        try FSBridgeContainer.validated(groupIdentifier: group, signedGroups: [group], teamIdentifier: "AB12CD34EF", containerURL: directory)
    }

    private func testDirectory() throws -> URL {
        let directory = URL(fileURLWithPath: "/private/tmp/rr-group-" + UUID().uuidString.prefix(8), isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        return directory
    }

    private func bindSocket(_ url: URL) throws -> Int32 {
        let listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard listener >= 0 else { throw POSIXError(.ENFILE) }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(url.path.utf8) + [0]) }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0, chmod(url.path, 0o600) == 0 else {
            Darwin.close(listener)
            throw POSIXError(.EACCES)
        }
        return listener
    }
}
