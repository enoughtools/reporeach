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

    func testAdoptedCheckoutMapsOriginalAndCataloguePathsWithoutReadingFiles() {
        let repository = FinderRepositoryStatus(id: "local/project", state: "available", pinned: false,
                                                localPath: "/Users/example/Source Projects/project", localKind: "adopted")
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: [repository])

        XCTAssertEqual(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Source Projects/project/.git/index"), in: status), repository.id)
        XCTAssertEqual(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Repositories/local/project/Sources/main.swift"), in: status), repository.id)
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Source Projects/project-other"), in: status))
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Source Projects/project/../outside"), in: status))
        XCTAssertEqual(FinderStatusCache.observedDirectoryURLs(in: status), [
            URL(fileURLWithPath: status.mountRoot, isDirectory: true), repository.localURL!
        ])
    }

    func testNestedCheckoutSelectionUsesDeepestRegisteredRepository() {
        let outer = FinderRepositoryStatus(id: "local/outer", state: "available", pinned: false, localPath: "/Users/example/Source/outer", localKind: "adopted")
        let inner = FinderRepositoryStatus(id: "local/inner", state: "available", pinned: false, localPath: "/Users/example/Source/outer/nested", localKind: "adopted")
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: [outer, inner])
        XCTAssertEqual(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Source/outer/nested/file"), in: status), inner.id)
        XCTAssertEqual(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Source/outer/file"), in: status), outer.id)
    }

    func testAmbiguousPhysicalRegistrationsOfferNoRepositoryAction() {
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: [
            FinderRepositoryStatus(id: "local/first", state: "available", pinned: false, localPath: "/Users/example/Source/project", localKind: "adopted"),
            FinderRepositoryStatus(id: "local/second", state: "available", pinned: false, localPath: "/Users/example/Source/project", localKind: "adopted")
        ])
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: "/Users/example/Source/project/file"), in: status))
    }

    func testLegacyFinderSnapshotAndLocalMetadataRoundTrip() throws {
        let legacy = try JSONDecoder().decode(FinderStatusSnapshot.self, from: Data(#"{"mountRoot":"/Users/example/Repositories","repositories":[{"id":"owner/repo","state":"virtual","pinned":false}]}"#.utf8))
        XCTAssertNil(legacy.repositories.first?.localPath)
        XCTAssertNil(legacy.repositories.first?.localKind)
        XCTAssertEqual(legacy.repositories.first?.downloadedBytes, 0)
        XCTAssertNil(legacy.virtualRoot)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = FinderStatusCache(fileURL: directory.appendingPathComponent("status.json"))
        let status = FinderStatusSnapshot(mountRoot: legacy.mountRoot, repositories: [
            FinderRepositoryStatus(id: "owner/repo", state: "available", pinned: true,
                                   localPath: "/Users/example/Repositories/owner/repo", localKind: "materialized")
        ])
        try cache.write(status)
        XCTAssertEqual(cache.read(), status)
        XCTAssertEqual(FinderStatusCache.repositoryURLs(for: status.repositories[0], in: status).count, 1)
    }

    func testFinderCacheRejectsUnsafeOrIncompleteLocalMetadata() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = FinderStatusCache(fileURL: directory.appendingPathComponent("status.json"))
        let invalid: [(String?, String?)] = [("/", "adopted"), ("relative/project", "adopted"), ("/Users/example/project", "unknown"),
                                            ("/Users/example/pro\u{0000}ject", "materialized"), ("/Users/example/project", nil), (nil, "adopted")]
        for (path, kind) in invalid {
            let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: [
                FinderRepositoryStatus(id: "owner/repo", state: "available", pinned: false, localPath: path, localKind: kind)
            ])
            XCTAssertThrowsError(try cache.write(status))
        }
    }

    func testResolvedVirtualSelectionMapsRepositoryAndObservesBothCatalogueRoots() {
        let repository = FinderRepositoryStatus(id: "owner/repo", state: "virtual", pinned: false)
        let virtualRoot = "/Users/example/Library/Application Support/RepoReach/native-catalogue/volume"
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: [repository], virtualRoot: virtualRoot)
        let selection = URL(fileURLWithPath: virtualRoot).appendingPathComponent("owner/repo/Sources/main.swift")

        XCTAssertEqual(FinderStatusCache.repositoryID(for: selection, in: status), repository.id)
        XCTAssertEqual(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: status.mountRoot + "/owner/repo"), in: status), repository.id)
        XCTAssertEqual(FinderStatusCache.repositoryURLs(for: repository, in: status), [
            URL(fileURLWithPath: status.mountRoot + "/owner/repo", isDirectory: true),
            URL(fileURLWithPath: virtualRoot + "/owner/repo", isDirectory: true)
        ])
        XCTAssertEqual(FinderStatusCache.observedDirectoryURLs(in: status), [
            URL(fileURLWithPath: status.mountRoot, isDirectory: true), URL(fileURLWithPath: virtualRoot, isDirectory: true)
        ])
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: virtualRoot + "-other/owner/repo"), in: status))
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: virtualRoot + "/owner/missing"), in: status))
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: virtualRoot + "/owner/repo/../../../outside"), in: status))
    }

    func testLocalCheckoutsNeverAcquireVirtualRepositoryPaths() {
        let virtualRoot = "/Users/example/PrivateVirtualCatalogue"
        let repositories = [
            FinderRepositoryStatus(id: "local/adopted", state: "available", pinned: false,
                                   localPath: "/Users/example/Source/adopted", localKind: "adopted"),
            FinderRepositoryStatus(id: "owner/kept", state: "available", pinned: true,
                                   localPath: "/Users/example/Repositories/owner/kept", localKind: "materialized")
        ]
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: repositories, virtualRoot: virtualRoot)
        XCTAssertFalse(FinderStatusCache.observedDirectoryURLs(in: status).contains(URL(fileURLWithPath: virtualRoot, isDirectory: true)))
        for repository in repositories {
            let virtualURL = URL(fileURLWithPath: virtualRoot + "/" + repository.id, isDirectory: true)
            XCTAssertFalse(FinderStatusCache.repositoryURLs(for: repository, in: status).contains(virtualURL))
            XCTAssertNil(FinderStatusCache.repositoryID(for: virtualURL.appendingPathComponent(".git/index"), in: status))
            XCTAssertEqual(FinderStatusCache.repositoryID(for: repository.localURL!.appendingPathComponent(".git/index"), in: status), repository.id)
        }
    }

    func testVirtualAndPhysicalSelectionCollisionOffersNoAction() {
        let virtualRoot = "/Users/example/PrivateVirtualCatalogue"
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: [
            FinderRepositoryStatus(id: "owner/repo", state: "virtual", pinned: false),
            FinderRepositoryStatus(id: "local/physical", state: "available", pinned: false,
                                   localPath: virtualRoot + "/owner/repo", localKind: "adopted")
        ], virtualRoot: virtualRoot)
        XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: virtualRoot + "/owner/repo/file"), in: status))
    }

    func testVirtualRootRoundTripsAndLegacyNullRootRemainsCompatible() throws {
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: snapshot().repositories,
                                          virtualRoot: "/Users/example/PrivateVirtualCatalogue")
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = FinderStatusCache(fileURL: directory.appendingPathComponent("status.json"))
        try cache.write(status)
        XCTAssertEqual(cache.read(), status)
        let legacy = try JSONDecoder().decode(FinderStatusSnapshot.self, from: Data(#"{"mountRoot":"/Users/example/Repositories","repositories":[],"virtualRoot":null}"#.utf8))
        XCTAssertNil(legacy.virtualRoot)
        XCTAssertEqual(FinderStatusCache.observedDirectoryURLs(in: legacy), [URL(fileURLWithPath: legacy.mountRoot, isDirectory: true)])
    }

    func testFinderDownloadedBytesDefaultsAndRoundTripsWhileRepositoryRemainsVirtual() throws {
        let legacy = try JSONDecoder().decode(FinderRepositoryStatus.self, from: Data(#"{"id":"owner/repo","state":"virtual","pinned":false,"downloadedBytes":null}"#.utf8))
        XCTAssertEqual(legacy.downloadedBytes, 0)
        XCTAssertFalse(legacy.canFreeStorage)
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = FinderStatusCache(fileURL: directory.appendingPathComponent("status.json"))
        let repository = FinderRepositoryStatus(id: legacy.id, state: "virtual", pinned: false, downloadedBytes: 37)
        let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: [repository])
        try cache.write(status)
        let loaded = try XCTUnwrap(cache.read()?.repositories.first)
        XCTAssertEqual(loaded, repository)
        XCTAssertTrue(loaded.canFreeStorage)
        XCTAssertEqual(loaded.state, "virtual")
    }

    func testUnsafeOrOverlappingVirtualRootsCannotBeObservedOrMapped() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = FinderStatusCache(fileURL: directory.appendingPathComponent("status.json"))
        for path in ["", "/", "relative/volume", "/Users/example/volume\u{0000}", "/Users/example/../volume",
                     "/Users/example/Repositories", "/Users/example/Repositories/nested", "/Users/example"] {
            let status = FinderStatusSnapshot(mountRoot: "/Users/example/Repositories", repositories: snapshot().repositories, virtualRoot: path)
            XCTAssertThrowsError(try cache.write(status))
            XCTAssertTrue(FinderStatusCache.observedDirectoryURLs(in: status).isEmpty)
            XCTAssertTrue(FinderStatusCache.repositoryURLs(for: status.repositories[0], in: status).isEmpty)
            XCTAssertNil(FinderStatusCache.repositoryID(for: URL(fileURLWithPath: status.mountRoot + "/owner/repo"), in: status))
        }
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
        XCTAssertNil(status.virtualRoot)
    }

    func testVirtualRootStatusFieldSurvivesDecodeAndRoundTrip() throws {
        let json = #"{"mountRoot":"/Users/example/Repositories","virtualRoot":"/Users/example/PrivateVirtualCatalogue"}"#
        let status = try JSONDecoder().decode(EngineStatus.self, from: Data(json.utf8))
        XCTAssertEqual(status.virtualRoot, "/Users/example/PrivateVirtualCatalogue")
        XCTAssertEqual(try JSONDecoder().decode(EngineStatus.self, from: JSONEncoder().encode(status)), status)
        XCTAssertNil(EngineStatus(mountRoot: status.mountRoot).virtualRoot)
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
