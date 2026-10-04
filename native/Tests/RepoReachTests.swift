import XCTest
@testable import RepoReach

final class ActionRouteTests: XCTestCase {
    func testFinderActionRoundTrip() throws {
        let route = try XCTUnwrap(ActionRoute(repo: "enoughtools/enough-ui.v2", action: .keep))
        XCTAssertEqual(ActionRoute(url: route.url), route)
    }

    func testRejectsAmbiguousAndUnsafeDeepLinks() {
        let invalid = [
            "reporeach://action?repo=owner/repo&repo=other/repo&action=free",
            "reporeach://action?repo=owner/repo&action=free&extra=yes",
            "reporeach://action?repo=owner/..&action=free",
            "reporeach://action?repo=owner%2Frepo%2Fescape&action=free",
            "reporeach://action?repo=owner/repo&action=delete",
            "reporeach://action?repo=owner/repo&action=free#fragment",
            "reporeach://attacker@action?repo=owner/repo&action=free",
            "https://action?repo=owner/repo&action=free"
        ]
        for value in invalid { XCTAssertNil(ActionRoute(url: URL(string: value)!), value) }
    }

    func testRepositoryValidationPreservesLegitimateGitHubNames() {
        XCTAssertTrue(ActionRoute.isValidRepositoryID("cloudflare/artifact-fs"))
        XCTAssertTrue(ActionRoute.isValidRepositoryID("octocat_enterprise/repo"))
        XCTAssertTrue(ActionRoute.isValidRepositoryID("enoughtools/repo_name.v2"))
        XCTAssertFalse(ActionRoute.isValidRepositoryID("/owner/repo"))
        XCTAssertFalse(ActionRoute.isValidRepositoryID("owner/.."))
        XCTAssertFalse(ActionRoute.isValidRepositoryID("owner/repo?token=secret"))
    }
}

final class FinderCacheTests: XCTestCase {
    private func snapshot(root: String = "/Users/example/Repositories") -> FinderStatusSnapshot {
        FinderStatusSnapshot(mountRoot: root, repositories: [FinderRepositoryStatus(id: "owner/repo", state: "pinned", pinned: true)])
    }

    func testSelectionMapsOnlyContainedRegisteredRepository() {
        let status = snapshot()
        XCTAssertEqual(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Repositories/owner/repo/Sources/main.swift"), in: status), "owner/repo")
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Repositories-other/owner/repo"), in: status))
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Repositories/owner/missing"), in: status))
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Repositories/owner/repo/../../../unrelated"), in: status))
    }

    func testRepeatedAtomicWriteAndPrivatePermissions() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = FinderStatusCache(fileURL: directory.appendingPathComponent("status.json"))
        try cache.write(snapshot())
        XCTAssertEqual(cache.read(), snapshot())
        let changed = snapshot(root: "/Users/example/OtherRepositories")
        try cache.write(changed)
        XCTAssertEqual(cache.read(), changed)
        let mode = try FileManager.default.attributesOfItem(atPath: cache.fileURL.path)[.posixPermissions] as? NSNumber
        XCTAssertEqual(mode?.intValue, 0o600)
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: directory.path), ["status.json"])
    }

    func testRejectsCorruptAndUnsafeCache() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = directory.appendingPathComponent("status.json")
        let cache = FinderStatusCache(fileURL: file)
        try Data("{bad json".utf8).write(to: file)
        XCTAssertNil(cache.read())
        XCTAssertThrowsError(try cache.write(snapshot(root: "/")))
        let duplicate = FinderStatusSnapshot(mountRoot: "/Users/example/Repos", repositories: [FinderRepositoryStatus(id: "owner/repo", state: "virtual", pinned: false), FinderRepositoryStatus(id: "owner/repo", state: "virtual", pinned: false)])
        XCTAssertThrowsError(try cache.write(duplicate))
    }
}

final class EngineContractTests: XCTestCase {
    func testDecodesStatusWithOptionalFieldsAndPrivateRepo() throws {
        let json = #"{"version":"0.1.0","mountRoot":"/Users/example/Repositories","mounted":false,"dependencyReady":true,"account":{"login":"octocat"},"repositories":[{"id":"owner/repo","owner":"owner","name":"repo","private":true,"state":"virtual"}]}"#
        let status = try JSONDecoder().decode(EngineStatus.self, from: Data(json.utf8))
        XCTAssertEqual(status.account?.login, "octocat")
        XCTAssertTrue(status.repositories[0].privateRepository)
        XCTAssertEqual(status.repositories[0].description, "")
        XCTAssertTrue(status.operations.isEmpty)
    }

    func testOperationProgressUsesCompletedFilesAndClamps() throws {
        let json = #"{"id":"task","repositoryID":"owner/repo","action":"keep","status":"running","completedBlobs":11,"totalBlobs":10,"downloadedBytes":20,"totalBytes":100}"#
        let operation = try JSONDecoder().decode(EngineOperation.self, from: Data(json.utf8))
        XCTAssertTrue(operation.isRunning)
        XCTAssertEqual(operation.progress, 1)
    }

    func testAPIErrorsPreserveMeaningAndRedactCredentials() {
        let response = CommandResult(status: 1, output: Data(#"{"error":"GitHub refused Bearer ghp_secretTokenHere"}"#.utf8), errorOutput: Data())
        XCTAssertThrowsError(try EngineClient.decode(response) as EngineStatus) { error in
            XCTAssertTrue(error.localizedDescription.contains("GitHub refused"))
            XCTAssertFalse(error.localizedDescription.contains("secretTokenHere"))
        }
    }

    func testInvalidSuccessfulResponseIsActionable() {
        let response = CommandResult(status: 0, output: Data("not json".utf8), errorOutput: Data())
        XCTAssertThrowsError(try EngineClient.decode(response) as EngineStatus) { error in
            XCTAssertTrue(error.localizedDescription.contains("Update RepoReach"))
        }
    }

    func testAuthDeviceCodeIsOptionalUntilPublished() throws {
        let auth = try JSONDecoder().decode(AuthSession.self, from: Data(#"{"authenticated":false,"pending":true}"#.utf8))
        XCTAssertTrue(auth.pending)
        XCTAssertNil(auth.deviceCode)
    }
}
