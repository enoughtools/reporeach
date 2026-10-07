import XCTest
@testable import RepoReach

final class FilesystemExtensionAvailabilityTests: XCTestCase {
    private let ownURL = URL(fileURLWithPath: "/fixture/EnoughRepos.app/Contents/Extensions/RepoReachFSKit.appex")
    private let otherURL = URL(fileURLWithPath: "/fixture/Old Copy.app/Contents/Extensions/RepoReachFSKit.appex")

    private func module(_ url: URL, enabled: Bool, identifier: String = FilesystemExtensionAvailability.moduleIdentifier,
                        shortName: String? = "reporeach") -> FilesystemExtensionAvailability.InstalledModule {
        .init(identifier: identifier, url: url, enabled: enabled, filesystemShortName: shortName)
    }

    func testFirstLaunchRequiresApprovalEvenWhenMacOSSupportsFSKit() {
        let state = FilesystemExtensionAvailability.assess([module(ownURL, enabled: false)], expectedURL: ownURL)
        XCTAssertEqual(state, .disabled)
        XCTAssertTrue(state.needsSetup)
        XCTAssertTrue(state.instructions.contains("File System Extensions"))
        XCTAssertTrue(state.instructions.contains("EnoughRepos Filesystem"))
        XCTAssertTrue(state.instructions.contains("Check Again"))
    }

    func testEnabledExactBundleCompletesSetup() {
        let state = FilesystemExtensionAvailability.assess([module(ownURL, enabled: true)], expectedURL: ownURL)
        XCTAssertEqual(state, .enabled)
        XCTAssertFalse(state.needsSetup)
    }

    func testOSSupportAndUncertainDiscoveryNeverClaimPermission() {
        XCTAssertFalse(FilesystemExtensionAvailability.unsupported.needsSetup)
        XCTAssertFalse(FilesystemExtensionAvailability.checking.needsSetup)
        XCTAssertTrue(FilesystemExtensionAvailability.unavailable.needsSetup)
        XCTAssertEqual(FilesystemExtensionAvailability.assess([], expectedURL: ownURL), .notRegistered)
    }

    func testEnabledDifferentCopyCannotSatisfyApproval() {
        for modules in [[module(otherURL, enabled: true)],
                        [module(ownURL, enabled: false), module(otherURL, enabled: true)],
                        [module(ownURL, enabled: true), module(otherURL, enabled: true)]] {
            XCTAssertEqual(FilesystemExtensionAvailability.assess(modules, expectedURL: ownURL), .conflicting)
        }
    }

    func testDisabledOtherCopyAndUnrelatedModuleDoNotBlockOwnApproval() {
        let modules = [module(ownURL, enabled: true), module(otherURL, enabled: false),
                       module(otherURL, enabled: true, identifier: "com.example.unrelated.filesystem", shortName: "otherfs")]
        XCTAssertEqual(FilesystemExtensionAvailability.assess(modules, expectedURL: ownURL), .enabled)
        XCTAssertEqual(FilesystemExtensionAvailability.assess([module(otherURL, enabled: false)], expectedURL: ownURL), .notRegistered)
    }

    func testDuplicateOwnRegistrationRequiresAttention() {
        let own = module(ownURL, enabled: true)
        XCTAssertEqual(FilesystemExtensionAvailability.assess([own, own], expectedURL: ownURL), .conflicting)
    }

    func testDifferentBundleAdvertisingTheSameFilesystemConflicts() {
        for identifier in ["com.enoughtools.reporeach.validation.fskit", "com.example.other.filesystem"] {
            let modules = [module(ownURL, enabled: true), module(otherURL, enabled: true, identifier: identifier)]
            XCTAssertEqual(FilesystemExtensionAvailability.assess(modules, expectedURL: ownURL), .conflicting)
        }
    }

    func testUnknownEnabledMetadataDoesNotClaimUnambiguousSelection() {
        for shortName in [nil, ""] {
            let unknown = module(otherURL, enabled: true, identifier: "com.example.unknown.filesystem", shortName: shortName)
            XCTAssertEqual(FilesystemExtensionAvailability.assess([module(ownURL, enabled: true), unknown], expectedURL: ownURL), .unavailable)
        }
        XCTAssertEqual(FilesystemExtensionAvailability.assess([module(ownURL, enabled: true, shortName: "otherfs")], expectedURL: ownURL), .unavailable)
    }

    func testDiscoveryRejectsRemoteAndAmbiguousModuleLocations() {
        for rawURL in ["https://example.com/EnoughRepos.app/Contents/Extensions/RepoReachFSKit.appex",
                       "file://remote/fixture/EnoughRepos.app/Contents/Extensions/RepoReachFSKit.appex",
                       ownURL.absoluteString + "?copy=1", ownURL.absoluteString + "#other"] {
            let url = URL(string: rawURL)!
            XCTAssertEqual(FilesystemExtensionAvailability.assess([module(url, enabled: true)], expectedURL: ownURL), .unavailable)
            XCTAssertEqual(FilesystemExtensionAvailability.assess([], expectedURL: url), .unavailable)
        }
        XCTAssertEqual(FilesystemExtensionAvailability.assess(Array(repeating: module(ownURL, enabled: true), count: 65), expectedURL: ownURL), .unavailable)
    }

    func testManagedApplicationSymlinkStillMatchesItsActualModule() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let bundle = directory.appendingPathComponent("Actual.app", isDirectory: true)
        let moduleURL = bundle.appendingPathComponent("Contents/Extensions/RepoReachFSKit.appex", isDirectory: true)
        try FileManager.default.createDirectory(at: moduleURL, withIntermediateDirectories: true)
        let alias = directory.appendingPathComponent("EnoughRepos.app", isDirectory: true)
        try FileManager.default.createSymbolicLink(at: alias, withDestinationURL: bundle)
        XCTAssertEqual(FilesystemExtensionAvailability.assess([module(moduleURL, enabled: true)],
                       expectedURL: alias.appendingPathComponent("Contents/Extensions/RepoReachFSKit.appex", isDirectory: true)), .enabled)
        let urlWithoutDirectoryHint = URL(fileURLWithPath: moduleURL.path, isDirectory: false)
        XCTAssertEqual(FilesystemExtensionAvailability.assess([module(urlWithoutDirectoryHint, enabled: true)], expectedURL: moduleURL), .enabled)
    }
}

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

    func testFinderDeliveryLocatesOnlyAContainingApplicationBundle() {
        let extensionURL = URL(fileURLWithPath: "/Users/example/Applications/Enough Repos.app/Contents/PlugIns/RepoReachFinder.appex")
        XCTAssertEqual(ActionRoute.containingApplicationURL(forFinderExtensionURL: extensionURL)?.path,
                       "/Users/example/Applications/Enough Repos.app")
        for path in ["/tmp/RepoReachFinder.appex", "/tmp/Other/Contents/PlugIns/RepoReachFinder.appex",
                     "/tmp/EnoughRepos.app/PlugIns/RepoReachFinder.appex", "/tmp/EnoughRepos.app/Contents/Extensions/RepoReachFinder.appex",
                     "/tmp/EnoughRepos.app/Contents/PlugIns/RepoReachFinder.app", "/tmp/EnoughRepos.app/Contents/PlugIns/../RepoReachFinder.appex"] {
            XCTAssertNil(ActionRoute.containingApplicationURL(forFinderExtensionURL: URL(fileURLWithPath: path)), path)
        }
        XCTAssertNil(ActionRoute.containingApplicationURL(forFinderExtensionURL: URL(string: "https://example.com/EnoughRepos.app/Contents/PlugIns/Finder.appex")!))
    }

    func testFinderDeliveryRequiresItsOwnRegisteredApplicationCopy() {
        let application = URL(fileURLWithPath: "/Users/example/Applications/Enough Repos.app", isDirectory: true)
        let extensionURL = application.appendingPathComponent("Contents/PlugIns/RepoReachFinder.appex")
        let staged = URL(fileURLWithPath: "/not-present/stage/EnoughRepos.app", isDirectory: true)
        XCTAssertEqual(ActionRoute.registeredContainingApplicationURL(
            forFinderExtensionURL: extensionURL, extensionBundleIdentifier: ActionRoute.finderExtensionBundleIdentifier,
            registeredApplicationURLs: [staged, application]), application)
        for copies in [[], [staged]] {
            XCTAssertNil(ActionRoute.registeredContainingApplicationURL(
                forFinderExtensionURL: extensionURL, extensionBundleIdentifier: ActionRoute.finderExtensionBundleIdentifier,
                registeredApplicationURLs: copies))
        }
        for identifier in [nil, ActionRoute.applicationBundleIdentifier, "com.example.foreign.finder"] {
            XCTAssertNil(ActionRoute.registeredContainingApplicationURL(
                forFinderExtensionURL: extensionURL, extensionBundleIdentifier: identifier,
                registeredApplicationURLs: [application]))
        }
    }

    func testFinderDeliveryRejectsRemoteAndAmbiguousRegisteredURLs() {
        let application = URL(fileURLWithPath: "/not-present/EnoughRepos.app", isDirectory: true)
        let extensionURL = application.appendingPathComponent("Contents/PlugIns/RepoReachFinder.appex")
        for value in ["https://example.com/not-present/EnoughRepos.app", "file://remote/not-present/EnoughRepos.app",
                      "file:///not-present/EnoughRepos.app?query=yes", "file:///not-present/EnoughRepos.app#fragment",
                      "file:///not-present/Other/../EnoughRepos.app", "file:///not-present/EnoughRepos.app/child"] {
            XCTAssertNil(ActionRoute.registeredContainingApplicationURL(
                forFinderExtensionURL: extensionURL, extensionBundleIdentifier: ActionRoute.finderExtensionBundleIdentifier,
                registeredApplicationURLs: [URL(string: value)!]), value)
        }
        for value in ["file://remote/not-present/EnoughRepos.app/Contents/PlugIns/RepoReachFinder.appex",
                      "file:///not-present/EnoughRepos.app/Contents/PlugIns/RepoReachFinder.appex?query=yes"] {
            XCTAssertNil(ActionRoute.registeredContainingApplicationURL(
                forFinderExtensionURL: URL(string: value)!, extensionBundleIdentifier: ActionRoute.finderExtensionBundleIdentifier,
                registeredApplicationURLs: [application]), value)
        }
    }

    func testFinderDeliveryDoesNotRequireParentBundleMetadata() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let application = directory.appendingPathComponent("EnoughRepos.app", isDirectory: true)
        let extensionURL = application.appendingPathComponent("Contents/PlugIns/RepoReachFinder.appex", isDirectory: true)
        let contents = extensionURL.appendingPathComponent("Contents", isDirectory: true)
        try FileManager.default.createDirectory(at: contents, withIntermediateDirectories: true)
        let info = try PropertyListSerialization.data(fromPropertyList: [
            "CFBundleIdentifier": ActionRoute.finderExtensionBundleIdentifier,
            "CFBundlePackageType": "XPC!"
        ], format: .xml, options: 0)
        try info.write(to: contents.appendingPathComponent("Info.plist"))
        let ownBundle = try XCTUnwrap(Bundle(url: extensionURL))
        XCTAssertEqual(ownBundle.bundleIdentifier, ActionRoute.finderExtensionBundleIdentifier)
        XCTAssertNil(Bundle(url: application)?.bundleIdentifier, "The Finder sandbox cannot rely on parent Info.plist access")
        XCTAssertEqual(ActionRoute.registeredContainingApplicationURL(
            forFinderExtensionURL: ownBundle.bundleURL, extensionBundleIdentifier: ownBundle.bundleIdentifier,
            registeredApplicationURLs: [application]), application)
    }
}

final class FinderActionDispatchTests: XCTestCase {
    private let root = "/not-present/Repos"
    private let virtualRoot = "/not-present/PrivateVolume"
    private func snapshot(_ repositories: [FinderRepositoryStatus]? = nil) -> FinderStatusSnapshot {
        FinderStatusSnapshot(mountRoot: root, repositories: repositories ?? [
            FinderRepositoryStatus(id: "owner/first", state: "available", pinned: false),
            FinderRepositoryStatus(id: "owner/second", state: "available", pinned: false)
        ], virtualRoot: virtualRoot)
    }
    private func url(_ path: String) -> URL { URL(fileURLWithPath: path) }

    func testDedicatedActionUsesOfficialCallbackSelectionWithoutMenuItemPayload() {
        let status = snapshot()
        // Menu construction and callback can have different contexts. The
        // controller's official action-time context is the authority.
        XCTAssertEqual(FinderStatusCache.selectedRepositoryID(for: [url(root + "/owner/first")], targetedURL: nil, in: status), "owner/first")
        let route = FinderStatusCache.actionRoute(for: .keep, selectionURLs: [url(virtualRoot + "/owner/second/Sources/main.swift")],
                                                targetedURL: nil, in: status)
        XCTAssertEqual(route?.repo, "owner/second")
        XCTAssertEqual(route?.action, .keep)
        XCTAssertEqual(route.flatMap { ActionRoute(url: $0.url) }, route)
    }

    func testAmbiguousOrUnmanagedNonemptySelectionNeverFallsBackToTarget() {
        let status = snapshot()
        let target = url(root + "/owner/first")
        for selections in [[target, url(root + "/owner/second")], [url("/outside/file")],
                           [target, url("/outside/file")], [URL(string: "https://example.com/owner/first")!]] {
            for action in [ActionRoute.Action.keep, .free, .refresh] {
                XCTAssertNil(FinderStatusCache.actionRoute(for: action, selectionURLs: selections, targetedURL: target, in: status))
            }
        }
    }

    func testMultipleChildrenOfOneRepositoryAndCurrentFolderBackgroundAreSupported() {
        let status = snapshot()
        let selections = [url(root + "/owner/first/Sources/a.swift"), url(virtualRoot + "/owner/first/README.md")]
        XCTAssertEqual(FinderStatusCache.actionRoute(for: .free, selectionURLs: selections, targetedURL: nil, in: status)?.repo, "owner/first")
        XCTAssertEqual(FinderStatusCache.actionRoute(for: .refresh, selectionURLs: [], targetedURL: url(virtualRoot + "/owner/first/Sources"), in: status)?.repo, "owner/first")
        XCTAssertNil(FinderStatusCache.actionRoute(for: .free, selectionURLs: [], targetedURL: url(root + "/owner"), in: status))
        XCTAssertNil(FinderStatusCache.actionRoute(for: .keep, selectionURLs: [], targetedURL: nil, in: status))
    }

    func testFreshCallbackSnapshotRejectsHiddenBusyAndAdoptedFreeActions() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = FinderStatusCache(fileURL: directory.appendingPathComponent("finder.json"))
        let selection = [url(root + "/owner/first")]
        try cache.write(snapshot())
        XCTAssertNotNil(FinderStatusCache.actionRoute(for: .free, selectionURLs: selection, targetedURL: nil, in: try XCTUnwrap(cache.read())))
        let operation = FinderOperationStatus(action: "keep", status: "running", completedBlobs: nil, totalBlobs: nil,
                                             downloadedBytes: nil, totalBytes: nil, error: nil)
        let busy = FinderRepositoryStatus(id: "owner/first", state: "available", pinned: false, operation: operation)
        try cache.write(snapshot([busy]))
        for action in [ActionRoute.Action.keep, .free, .refresh] {
            XCTAssertNil(FinderStatusCache.actionRoute(for: action, selectionURLs: selection, targetedURL: nil, in: try XCTUnwrap(cache.read())))
        }
        try cache.write(snapshot([]))
        XCTAssertNil(FinderStatusCache.actionRoute(for: .free, selectionURLs: selection, targetedURL: nil, in: try XCTUnwrap(cache.read())))
        let adopted = FinderRepositoryStatus(id: "owner/first", state: "local", pinned: false, localPath: "/not-present/Original/first", localKind: "adopted")
        try cache.write(snapshot([adopted]))
        XCTAssertNil(FinderStatusCache.actionRoute(for: .free, selectionURLs: selection, targetedURL: nil, in: try XCTUnwrap(cache.read())))
        XCTAssertNotNil(FinderStatusCache.actionRoute(for: .keep, selectionURLs: selection, targetedURL: nil, in: try XCTUnwrap(cache.read())))
    }

    func testCallbackEligibilityRejectsAlreadyKeptOrEmptyVirtualStorage() {
        let selection = [url(root + "/owner/first")]
        let pinned = FinderRepositoryStatus(id: "owner/first", state: "local", pinned: true,
                                            localPath: root + "/owner/first", localKind: "materialized")
        XCTAssertNil(FinderStatusCache.actionRoute(for: .keep, selectionURLs: selection, targetedURL: nil, in: snapshot([pinned])))
        XCTAssertNotNil(FinderStatusCache.actionRoute(for: .free, selectionURLs: selection, targetedURL: nil, in: snapshot([pinned])))
        let virtual = FinderRepositoryStatus(id: "owner/first", state: "virtual", pinned: false)
        XCTAssertNil(FinderStatusCache.actionRoute(for: .free, selectionURLs: selection, targetedURL: nil, in: snapshot([virtual])))
        let cached = FinderRepositoryStatus(id: virtual.id, state: virtual.state, pinned: false, downloadedBytes: 37)
        XCTAssertNotNil(FinderStatusCache.actionRoute(for: .free, selectionURLs: selection, targetedURL: nil, in: snapshot([cached])))
    }
}

final class FinderCacheTests: XCTestCase {
    private func snapshot(root: String = "/Users/example/Repositories") -> FinderStatusSnapshot {
        FinderStatusSnapshot(mountRoot: root, repositories: [FinderRepositoryStatus(id: "owner/repo", state: "pinned", pinned: true)])
    }

    func testAcceptedKeepPublishesBusyBeforeRepositoryStateChanges() throws {
        let initial = try JSONDecoder().decode(EngineStatus.self, from: Data(#"{"mountRoot":"/Users/example/Repos","virtualRoot":"/Users/example/PrivateVolume","repositories":[{"id":"owner/repo","owner":"owner","name":"repo","state":"virtual","error":"previous attempt failed","downloadedBytes":37}]}"#.utf8))
        let response = try JSONDecoder().decode(RepositoryActionResponse.self, from: Data(#"{"operation":{"id":"accepted","repositoryID":"owner/repo","action":"keep","status":"running"}}"#.utf8))
        let accepted = initial.recording(response.operation)
        let finder = try XCTUnwrap(accepted.finderSnapshot.repositories.first)

        XCTAssertEqual(finder.state, "virtual")
        XCTAssertEqual(accepted.virtualRoot, initial.virtualRoot)
        XCTAssertTrue(finder.isWorking)
        XCTAssertFalse(finder.canFreeStorage)
        XCTAssertEqual(finder.badgeIdentifier, "downloading", "Accepted work takes precedence over a stale previous error")
        XCTAssertEqual(finder.operation?.stageDescription, "Preparing download")
        XCTAssertNil(finder.operation?.progress)
        XCTAssertEqual(accepted.recording(response.operation).operations.count, 1)
    }

    func testOperationProgressAndFailureReachFinderWithoutRepositoryStateMutation() throws {
        let initial = EngineStatus(mountRoot: "/Users/example/Repos", repositories: [
            RepositoryRecord(id: "owner/repo", owner: "owner", name: "repo", description: "", state: "available")
        ])
        let running = try JSONDecoder().decode(EngineOperation.self, from: Data(#"{"id":"download","repositoryID":"owner/repo","action":"keep","status":"running","completedBlobs":3,"totalBlobs":10,"downloadedBytes":4096,"totalBytes":8192}"#.utf8))
        let live = initial.recording(running)
        let finder = try XCTUnwrap(live.finderSnapshot.repositories.first)
        XCTAssertEqual(finder.badgeIdentifier, "downloading-3")
        XCTAssertEqual(finder.operation?.progress, 0.3)
        XCTAssertEqual(finder.operation?.stageDescription, "Downloading files")
        XCTAssertTrue(finder.operation?.progressDescription?.hasPrefix("3 of 10 files · ") == true)
        XCTAssertEqual(running.progressDescription, finder.operation?.progressDescription)

        let failed = try JSONDecoder().decode(EngineOperation.self, from: Data(#"{"id":"download","repositoryID":"owner/repo","action":"keep","status":"failed","completedBlobs":10,"totalBlobs":10,"error":"Local work was retained"}"#.utf8))
        let result = live.recording(failed)
        let failure = try XCTUnwrap(result.finderSnapshot.repositories.first)
        XCTAssertEqual(result.operations.count, 1)
        XCTAssertEqual(failure.state, "available")
        XCTAssertFalse(failure.isWorking)
        XCTAssertEqual(failure.badgeIdentifier, "error")
        XCTAssertEqual(failure.operation?.error, "Local work was retained")
        XCTAssertEqual(try JSONDecoder().decode(FinderStatusSnapshot.self, from: JSONEncoder().encode(result.finderSnapshot)), result.finderSnapshot)
    }

    func testFinderOperationHistoryPrefersRunningWorkAndCompletionClearsBusy() throws {
        let status = try JSONDecoder().decode(EngineStatus.self, from: Data(#"{"mountRoot":"/Users/example/Repos","repositories":[{"id":"owner/repo","owner":"owner","name":"repo","state":"available"}],"operations":[{"id":"current","repositoryID":"owner/repo","action":"keep","status":"running"},{"id":"old","repositoryID":"owner/repo","action":"free","status":"failed","error":"Old failure"}]}"#.utf8))
        XCTAssertEqual(status.finderSnapshot.repositories.first?.badgeIdentifier, "downloading")
        let complete = try JSONDecoder().decode(EngineOperation.self, from: Data(#"{"id":"current","repositoryID":"owner/repo","action":"keep","status":"complete"}"#.utf8))
        let completed = status.recording(complete).finderSnapshot.repositories.first
        XCTAssertFalse(try XCTUnwrap(completed).isWorking, "A completed operation must stop the busy badge")
        XCTAssertNotEqual(completed?.badgeIdentifier, "error", "Completed retry supersedes the older failed operation")
    }

    func testLegacyFinderOperationMetadataIsOptionalAndProgressIsBounded() throws {
        for payload in [
            #"{"id":"owner/repo","state":"preparing","pinned":false}"#,
            #"{"id":"owner/repo","state":"preparing","pinned":false,"operation":null}"#
        ] {
            let repository = try JSONDecoder().decode(FinderRepositoryStatus.self, from: Data(payload.utf8))
            XCTAssertNil(repository.operation)
            XCTAssertTrue(repository.isWorking)
            XCTAssertEqual(repository.badgeIdentifier, "downloading")
        }
        for (completed, expected) in [(-1, 0.0), (11, 1.0)] {
            let operation = FinderOperationStatus(action: "keep", status: "running", completedBlobs: Int64(completed), totalBlobs: 10,
                                                  downloadedBytes: nil, totalBytes: nil, error: nil)
            XCTAssertEqual(operation.progress, expected)
            if expected == 1 { XCTAssertEqual(operation.stageDescription, "Creating local checkout") }
        }
    }

    func testFinderProjectionKeepsOperationMetadataScopedToVisibleRepositories() throws {
        let status = try JSONDecoder().decode(EngineStatus.self, from: Data(#"{"mountRoot":"/Users/example/Repos","repositories":[{"id":"owner/repo","owner":"owner","name":"repo"},{"id":"owner/hidden","owner":"owner","name":"hidden","disabled":true},{"id":"other/repo","owner":"other","name":"repo"}],"organizations":[{"name":"other","enabled":false}],"operations":[{"id":"hidden","repositoryID":"owner/hidden","action":"keep","status":"running"},{"id":"current","repositoryID":"owner/repo","action":"keep","status":"running","currentPath":"private/source.swift"}]}"#.utf8))
        let snapshot = status.finderSnapshot
        XCTAssertEqual(snapshot.repositories.map(\.id), ["owner/repo"])
        XCTAssertTrue(try XCTUnwrap(snapshot.repositories.first).isWorking)
        let encoded = String(decoding: try JSONEncoder().encode(snapshot), as: UTF8.self)
        XCTAssertFalse(encoded.contains("private/source.swift"), "Finder progress needs counts, not filenames")
        XCTAssertFalse(encoded.contains("owner/hidden"))
        XCTAssertFalse(encoded.contains("other/repo"))
    }

    func testVisibleNestedChildBadgesRefreshWithoutReenumeratingRepositoryRoots() {
        let status = FinderStatusSnapshot(mountRoot: "/not-present/Repos", repositories: [
            FinderRepositoryStatus(id: "owner/repo", state: "virtual", pinned: false)
        ], virtualRoot: "/not-present/PrivateVolume")
        let directory = URL(fileURLWithPath: "/not-present/PrivateVolume/owner/repo/addons", isDirectory: true)
        let child = directory.appendingPathComponent("Child", isDirectory: true)
        let unrelated = URL(fileURLWithPath: "/not-present/PrivateVolume/owner/repo/addons-other/Child", isDirectory: true)
        var tracking = FinderBadgeTracking()
        tracking.beginObserving(directory)
        tracking.recordBadgeRequest(child)
        tracking.recordBadgeRequest(unrelated)
        tracking.recordBadgeRequest(URL(string: "https://example.com/Child")!)

        XCTAssertEqual(tracking.urlsToRebadge(in: status), [child])
        XCTAssertEqual(FinderStatusCache.repositoryID(for: child, in: status), "owner/repo")
        tracking.endObserving(directory)
        XCTAssertTrue(tracking.urlsToRebadge(in: status).isEmpty)
        XCTAssertTrue(tracking.requestedURLs.isEmpty)
    }

    func testEndingOverlappingObservationPreservesOtherVisibleBadgeRequests() {
        let status = snapshot()
        let parent = URL(fileURLWithPath: status.mountRoot, isDirectory: true)
        let nested = parent.appendingPathComponent("owner/repo/Sources", isDirectory: true)
        let child = nested.appendingPathComponent("main.swift")
        var tracking = FinderBadgeTracking()
        tracking.beginObserving(parent)
        tracking.beginObserving(nested)
        tracking.recordBadgeRequest(child)
        tracking.endObserving(parent)
        XCTAssertEqual(tracking.urlsToRebadge(in: status), [child])
        tracking.endObserving(nested)
        XCTAssertTrue(tracking.requestedURLs.isEmpty)
    }

    func testBadgeTrackingBoundsLargeDirectoriesAndReleasesClosedObservations() {
        let directory = URL(fileURLWithPath: "/not-present/Repos/owner/repo/Files", isDirectory: true)
        var tracking = FinderBadgeTracking()
        tracking.beginObserving(directory)
        for index in 0..<10_005 { tracking.recordBadgeRequest(directory.appendingPathComponent("file-\(index)")) }
        XCTAssertEqual(tracking.requestedURLs.count, 10_000)
        XCTAssertTrue(tracking.requestedURLs.contains(directory.appendingPathComponent("file-10004")), "Newly visible requests must replace older entries at the bound")
        tracking.endObserving(directory)
        XCTAssertTrue(tracking.requestedURLs.isEmpty)
        XCTAssertTrue(tracking.observedDirectories.isEmpty)
        for index in 0..<100 {
            let next = directory.appendingPathComponent("directory-\(index)", isDirectory: true)
            tracking.beginObserving(next)
            tracking.recordBadgeRequest(next.appendingPathComponent("file"))
            tracking.endObserving(next)
        }
        XCTAssertTrue(tracking.requestedURLs.isEmpty, "Browsing closed directories must not accumulate badge history")
        XCTAssertTrue(tracking.observedDirectories.isEmpty)
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
            XCTAssertTrue(error.localizedDescription.contains("Update EnoughRepos"))
        }
    }

    func testAuthDeviceCodeIsOptionalUntilPublished() throws {
        let auth = try JSONDecoder().decode(AuthSession.self, from: Data(#"{"authenticated":false,"pending":true}"#.utf8))
        XCTAssertTrue(auth.pending)
        XCTAssertNil(auth.deviceCode)
    }
}
