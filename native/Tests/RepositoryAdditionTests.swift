import Foundation
import XCTest
@testable import RepoReach

final class RepositoryAdditionModelTests: XCTestCase {
    func testExistingStatusDefaultsNewRepositoryAndOrganizationFields() throws {
        let status = try decodeStatus(#"{"mountRoot":"/Users/example/Repositories","repositories":[{"id":"owner/repo","owner":"owner","name":"repo"}]}"#)
        let repository = try XCTUnwrap(status.repositories.first)

        XCTAssertEqual(repository.source, "github")
        XCTAssertFalse(repository.disabled)
        XCTAssertFalse(repository.isManual)
        XCTAssertTrue(repository.isEnabled(in: status.organizations))
        XCTAssertNil(repository.localPath)
        XCTAssertNil(repository.localKind)
        XCTAssertFalse(repository.isLocal)
        XCTAssertTrue(status.organizations.isEmpty)
    }

    func testMissingAndNullOrganizationListsAreBackwardCompatible() throws {
        for json in [
            #"{"mountRoot":"/Users/example/Repositories"}"#,
            #"{"mountRoot":"/Users/example/Repositories","organizations":null}"#
        ] {
            XCTAssertTrue(try decodeStatus(json).organizations.isEmpty)
        }
        XCTAssertTrue(EngineStatus(mountRoot: "/Users/example/Repositories").organizations.isEmpty)
    }

    func testManualAndDisabledRepositoriesAndOrganizationSettingsRoundTrip() throws {
        let status = try decodeStatus(#"{"mountRoot":"/Users/example/Repositories","repositories":[{"id":"local/project","owner":"local","name":"project","source":"manual","disabled":true}],"organizations":[{"name":"local","enabled":false},{"name":"enoughtools","enabled":true}]}"#)
        let repository = try XCTUnwrap(status.repositories.first)

        XCTAssertTrue(repository.isManual)
        XCTAssertTrue(repository.disabled)
        XCTAssertFalse(repository.isEnabled(in: status.organizations))
        XCTAssertEqual(status.organizations.map(\.id), ["local", "enoughtools"])
        XCTAssertEqual(status.organizations.map(\.enabled), [false, true])
        XCTAssertEqual(try JSONDecoder().decode(EngineStatus.self, from: JSONEncoder().encode(status)), status)
    }

    func testEmptyLegacySourceStillRepresentsGitHubRepository() throws {
        let repository = try decodeRepository(source: "")
        XCTAssertFalse(repository.isManual)
        XCTAssertTrue(repository.isEnabled(in: []))
    }

    func testOrganizationSettingAppliesToManuallyAdoptedRepository() throws {
        let repository = try decodeRepository(source: "manual")

        XCTAssertTrue(repository.isEnabled(in: []))
        XCTAssertTrue(repository.isEnabled(in: [OrganizationRecord(name: "other", enabled: false)]))
        XCTAssertTrue(repository.isEnabled(in: [OrganizationRecord(name: "owner", enabled: true)]))
        XCTAssertFalse(repository.isEnabled(in: [OrganizationRecord(name: "owner", enabled: false)]))
    }

    func testOrganizationDisableMatchesOwnerCaseInsensitively() throws {
        let status = try decodeStatus(#"{"mountRoot":"/Users/example/Repositories","repositories":[{"id":"Team/repo","owner":"Team","name":"repo","source":"manual"}],"organizations":[{"name":"TEAM","enabled":false}]}"#)
        XCTAssertFalse(try XCTUnwrap(status.repositories.first).isEnabled(in: status.organizations))
    }

    func testManualAliasesRoundTripThroughFinderRoute() throws {
        let id = "team.alias/_project.v2"
        let route = try XCTUnwrap(ActionRoute(repo: id, action: .keep))
        XCTAssertEqual(ActionRoute(url: route.url)?.repo, id)
        XCTAssertNotNil(ActionRoute(repo: "-team-/project", action: .refresh))
        XCTAssertNil(ActionRoute(repo: "../project", action: .keep))
    }

    func testRepositoryDisableCannotBeOverriddenByEnabledOrganization() throws {
        for source in ["github", "manual"] {
            let repository = try decodeRepository(source: source, disabled: true)
            XCTAssertFalse(repository.isEnabled(in: []))
            XCTAssertFalse(repository.isEnabled(in: [OrganizationRecord(name: "owner", enabled: true)]))
        }
    }

    func testAdoptedCheckoutRetainsPhysicalLocationAndRoundTrips() throws {
        let status = try decodeStatus(#"{"mountRoot":"/Users/example/Repositories","repositories":[{"id":"local/project","owner":"local","name":"project","source":"manual","localPath":"/Users/example/Source Projects/project","localKind":"adopted","state":"available"}]}"#)
        let repository = try XCTUnwrap(status.repositories.first)

        XCTAssertTrue(repository.isLocal)
        XCTAssertTrue(repository.isAdopted)
        XCTAssertEqual(repository.displayState, "Adopted checkout")
        XCTAssertEqual(repository.folderURL(in: status.mountRoot)?.path, "/Users/example/Source Projects/project")
        XCTAssertEqual(try JSONDecoder().decode(EngineStatus.self, from: JSONEncoder().encode(status)), status)
    }

    func testMaterializedCheckoutUsesLocalPathAndKeepsOperationStateVisible() {
        let repository = RepositoryRecord(id: "owner/repo", owner: "owner", name: "repo", description: "", state: "available",
                                          pinned: true, localPath: "/Users/example/Repositories/owner/repo", localKind: "materialized")
        XCTAssertTrue(repository.isLocal)
        XCTAssertFalse(repository.isAdopted)
        XCTAssertEqual(repository.displayState, "Local checkout")
        XCTAssertEqual(repository.folderURL(in: "/Users/example/Other")?.path, repository.localPath)
        for (state, label) in [("downloading", "Downloading"), ("preparing", "Preparing"), ("error", "Needs attention")] {
            let working = RepositoryRecord(id: repository.id, owner: repository.owner, name: repository.name, description: "", state: state,
                                           localPath: repository.localPath, localKind: repository.localKind)
            XCTAssertEqual(working.displayState, label)
        }
    }

    func testLocalLocationMustBeAnAbsoluteNonRootPathWithKnownKind() {
        for (path, kind) in [("relative/repo", "adopted"), ("/", "adopted"), ("/Users/example/pro\u{0000}ject", "adopted"),
                             ("/Users/example/repo", "unknown"), ("/Users/example/repo", "")] {
            let repository = RepositoryRecord(id: "owner/repo", owner: "owner", name: "repo", description: "", localPath: path, localKind: kind)
            XCTAssertFalse(repository.isLocal)
            XCTAssertEqual(repository.folderURL(in: "/Users/example/Repositories")?.path, "/Users/example/Repositories/owner/repo")
        }
    }

    func testVirtualFolderUsesValidatedIdentityAndRejectsUnsafeRoot() {
        let repository = RepositoryRecord(id: "owner/repo", owner: "../outside", name: "other", description: "")
        XCTAssertEqual(repository.folderURL(in: "/Users/example/Repositories")?.path, "/Users/example/Repositories/owner/repo")
        XCTAssertNil(repository.folderURL(in: "/"))
        XCTAssertNil(repository.folderURL(in: "relative"))
    }

    private func decodeStatus(_ json: String) throws -> EngineStatus {
        try JSONDecoder().decode(EngineStatus.self, from: Data(json.utf8))
    }

    private func decodeRepository(source: String, disabled: Bool = false) throws -> RepositoryRecord {
        let data = try JSONSerialization.data(withJSONObject: [
            "id": "owner/repo", "owner": "owner", "name": "repo",
            "source": source, "disabled": disabled
        ])
        return try JSONDecoder().decode(RepositoryRecord.self, from: data)
    }
}

final class RepositoryAdditionRequestTests: XCTestCase {
    func testAdoptionRequestOmitsUnspecifiedAliases() throws {
        let request = RepositoryAdoptionRequest(remoteURL: "git@git.example.com:team/project.git", owner: nil, name: nil, branch: nil)
        let body = try object(for: request)

        XCTAssertEqual(body["remoteURL"] as? String, request.remoteURL)
        XCTAssertEqual(Set(body.keys), ["remoteURL"])
    }

    func testAdoptionRequestPreservesExplicitAliasesAndBranch() throws {
        let request = RepositoryAdoptionRequest(remoteURL: "/Users/example/Source Projects/project", owner: "local", name: "project.v2", branch: "feature/work")
        let body = try object(for: request)

        XCTAssertEqual(body["remoteURL"] as? String, request.remoteURL)
        XCTAssertEqual(body["owner"] as? String, "local")
        XCTAssertEqual(body["name"] as? String, "project.v2")
        XCTAssertEqual(body["branch"] as? String, "feature/work")
        XCTAssertEqual(Set(body.keys), ["remoteURL", "owner", "name", "branch"])
    }

    func testOrganizationSettingEncodesJSONBooleans() throws {
        for enabled in [false, true] {
            let body = try object(for: OrganizationSettingsRequest(owner: "enoughtools", enabled: enabled))
            XCTAssertEqual(body["owner"] as? String, "enoughtools")
            XCTAssertEqual(body["enabled"] as? Bool, enabled)
            XCTAssertNil(body["enabled"] as? String)
            XCTAssertEqual(Set(body.keys), ["owner", "enabled"])
        }
    }

    func testRepositoryVisibilityEncodesJSONBooleans() throws {
        for enabled in [false, true] {
            let body = try object(for: RepositoryVisibilityRequest(id: "owner/repo", enabled: enabled))
            XCTAssertEqual(body["id"] as? String, "owner/repo")
            XCTAssertEqual(body["enabled"] as? Bool, enabled)
            XCTAssertNil(body["enabled"] as? String)
            XCTAssertEqual(Set(body.keys), ["id", "enabled"])
        }
    }

    private func object<Body: Encodable>(for body: Body) throws -> [String: Any] {
        try XCTUnwrap(JSONSerialization.jsonObject(with: EngineClient.encodeBody(body)) as? [String: Any])
    }
}

final class RepositoryRemoteInputTests: XCTestCase {
    func testValidRemotesAndLocalPathsArePreserved() throws {
        let values = [
            "https://github.com/enoughtools/reporeach.git",
            "https://git.example.com:8443/team/project.git",
            "git@github.com:enoughtools/reporeach.git",
            "ssh://git@git.example.com:2222/team/project.git",
            "/Users/example/Source Projects/project",
            "/Volumes/Development/team/project.git",
            "file:///Users/example/Source%20Projects/project"
        ]
        for value in values {
            XCTAssertEqual(try RepositoryInput.validateRemoteURL(value), value)
        }
    }

    func testSurroundingWhitespaceIsTrimmed() throws {
        XCTAssertEqual(try RepositoryInput.validateRemoteURL(" \n\thttps://git.example.com/team/project.git \n"), "https://git.example.com/team/project.git")
        XCTAssertEqual(try RepositoryInput.validateRemoteURL("  /Users/example/Source Projects/project  "), "/Users/example/Source Projects/project")
    }

    func testOrdinarySSHUsernamesArePreserved() throws {
        for value in [
            "ssh://deploy-bot@git.example.com/team/project.git",
            "deploy-bot@git.example.com:team/project.git"
        ] {
            XCTAssertEqual(try RepositoryInput.validateRemoteURL(value), value)
        }
    }

    func testLongAbsolutePathIsNotMistakenForRedactedCredentials() throws {
        let value = "/Users/example/" + String(repeating: "nested/", count: 283) + "repo"
        XCTAssertEqual(value.utf8.count, 2000)
        XCTAssertEqual(try RepositoryInput.validateRemoteURL(value), value)
    }

    func testRejectsEmptyControlCharactersAndOversizedInput() {
        let values = [
            "", " \n\t ",
            "https://git.example.com/team/pro\u{0000}ject.git",
            "https://git.example.com/team/pro\nject.git",
            "/Users/example/pro\tject",
            "/Users/example/pro\u{007F}ject",
            "https://git.example.com/" + String(repeating: "a", count: 4096)
        ]
        for value in values {
            XCTAssertThrowsError(try RepositoryInput.validateRemoteURL(value))
        }
    }

    func testRejectsCredentialsBeforeRemoteCanBeUsedInCLIArguments() {
        // These are deliberately synthetic fixtures, never real credentials.
        let values = [
            "https://user@git.example.com/team/project.git",
            "https://user:synthetic-password@git.example.com/team/project.git",
            "https://%75ser:synthetic-password@git.example.com/team/project.git",
            "ssh://git:synthetic-password@git.example.com/team/project.git",
            "file://user:synthetic-password@localhost/Users/example/project",
            "https://git.example.com/team/project.git?token=synthetic-password",
            "https://git.example.com/team/project.git#synthetic-password",
            "file:///Users/example/project?token=synthetic-password",
            "file:///Users/example/project#synthetic-password"
        ]
        for value in values {
            XCTAssertThrowsError(try RepositoryInput.validateRemoteURL(value)) { error in
                XCTAssertFalse(error.localizedDescription.contains("synthetic-password"))
            }
        }
    }

    func testRejectsRecognizedGitHubTokenPatternsWithoutEchoingThem() {
        // Pattern-shaped examples ensure credentials cannot leak through a path or URL.
        let tokens = [
            "ghp_SYNTHETIC_TEST_CREDENTIAL", "gho_SYNTHETIC_TEST_CREDENTIAL",
            "ghu_SYNTHETIC_TEST_CREDENTIAL", "ghs_SYNTHETIC_TEST_CREDENTIAL",
            "ghr_SYNTHETIC_TEST_CREDENTIAL", "github_pat_SYNTHETIC_TEST_CREDENTIAL"
        ]
        for token in tokens {
            for value in ["https://git.example.com/team/\(token).git", "/Users/example/\(token)"] {
                XCTAssertThrowsError(try RepositoryInput.validateRemoteURL(value)) { error in
                    XCTAssertFalse(error.localizedDescription.contains(token))
                }
            }
        }
    }

    func testRejectsTokenUsernamesInSSHAndSCPRemotesWithoutEchoingThem() {
        // Pattern-shaped examples exercise both GitLab and case-insensitive GitHub tokens.
        let tokens = [
            "glpat-SYNTHETIC_TEST_CREDENTIAL",
            "GhP_SYNTHETIC_TEST_CREDENTIAL",
            "GitHub_PaT_SYNTHETIC_TEST_CREDENTIAL"
        ]
        for token in tokens {
            for value in [
                "ssh://\(token)@git.example.com/team/project.git",
                "\(token)@git.example.com:team/project.git"
            ] {
                XCTAssertThrowsError(try RepositoryInput.validateRemoteURL(value)) { error in
                    XCTAssertFalse(error.localizedDescription.contains(token))
                }
            }
        }
    }

    func testRejectsAuthenticationStyleSSHAndSCPUsernames() {
        for user in ["oauth2", "x-token-auth"] {
            for value in [
                "ssh://\(user)@git.example.com/team/project.git",
                "\(user)@git.example.com:team/project.git"
            ] {
                XCTAssertThrowsError(try RepositoryInput.validateRemoteURL(value))
            }
        }
    }

    func testRejectsMalformedCredentialBearingAuthoritiesWithoutEchoingSecrets() {
        for value in [
            "ssh://git:synthetic-password@bad[host]/team/project.git",
            "https://user:synthetic-password@@git.example.com/team/project.git"
        ] {
            XCTAssertThrowsError(try RepositoryInput.validateRemoteURL(value)) { error in
                XCTAssertFalse(error.localizedDescription.contains("synthetic-password"))
            }
        }
    }
}
