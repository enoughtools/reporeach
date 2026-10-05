import Foundation

/// Compile with App/Models.swift and App/EngineClient.swift, then pass the built
/// Go executable as the first argument. Uses a fake gh, no GitHub/network data.
@main
struct ControlSmoke {
    static func main() async {
        do { try await run() }
        catch {
            fputs("FAIL: \(error.localizedDescription)\n", stderr)
            exit(1)
        }
    }

    static func run() async throws {
        guard CommandLine.arguments.count == 2 else { fatalError("Pass the built artifact-fs executable") }
        let engine = URL(fileURLWithPath: CommandLine.arguments[1])
        let root = URL(fileURLWithPath: "/tmp/rr-native-" + String(UUID().uuidString.prefix(8)), isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: root) }
        let gh = root.appendingPathComponent("gh")
        let release = root.appendingPathComponent("release")
        let signed = root.appendingPathComponent("signed")
        let trace = root.appendingPathComponent("gh-trace")
        let script = #"""
        #!/bin/sh
        printf '%s\n' "$*" >> "$REPOREACH_FIXTURE_TRACE"
        case "$*" in
          'api --hostname github.com user')
            [ -f "$REPOREACH_FIXTURE_SIGNED" ] || exit 4
            printf '%s\n' '{"login":"fixture-account"}'
            ;;
          'auth login --hostname github.com --git-protocol https --web')
            printf '%s\n' '! First copy your one-time code: AB12-CD34' >&2
            printf '%s\n' 'Open https://github.com/login/device?token=fixture-only-do-not-forward' >&2
            while [ ! -f "$REPOREACH_FIXTURE_RELEASE" ]; do sleep 0.05; done
            touch "$REPOREACH_FIXTURE_SIGNED"
            ;;
          'api --hostname github.com --paginate user/repos?per_page=100&visibility=all&affiliation=owner,collaborator,organization_member&sort=full_name&direction=asc')
            printf '%s\n' '[{"name":"fixture-repo","owner":{"login":"fixture-account"},"description":"Synthetic test metadata","default_branch":"main","private":true}]'
            ;;
          *) exit 1 ;;
        esac
        """#
        try Data(script.utf8).write(to: gh)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: gh.path)
        let state = root.appendingPathComponent("state", isDirectory: true)
        let socket = root.appendingPathComponent("s.sock")
        let mount = root.appendingPathComponent("mount", isDirectory: true)
        let service = Process()
        service.executableURL = engine
        service.arguments = ["desktop", "serve", "--state-dir", state.path, "--mount-root", mount.path, "--socket", socket.path, "--gh", gh.path]
        var environment = ProcessInfo.processInfo.environment
        for key in ["GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"] { environment.removeValue(forKey: key) }
        environment["REPOREACH_FIXTURE_RELEASE"] = release.path
        environment["REPOREACH_FIXTURE_SIGNED"] = signed.path
        environment["REPOREACH_FIXTURE_TRACE"] = trace.path
        environment["GH_CONFIG_DIR"] = root.appendingPathComponent("gh-config").path
        environment["GIT_CONFIG_GLOBAL"] = "/dev/null"
        service.environment = environment
        let null = FileHandle(forWritingAtPath: "/dev/null")!
        service.standardOutput = null; service.standardError = null
        try service.run()
        defer { if service.isRunning { service.terminate(); service.waitUntilExit() } }
        let client = EngineClient(executable: engine, socket: socket)
        var initial: EngineStatus?
        for _ in 0..<100 {
            initial = try? await client.request("GET", path: "/v1/status", timeout: 3)
            if initial != nil { break }
            try await Task.sleep(nanoseconds: 100_000_000)
        }
        try expect(initial?.repositories.isEmpty == true && initial?.mounted == false, "initial status")
        let duplicate = try await CommandRunner.run(executable: engine, arguments: service.arguments!, timeout: 4)
        try expect(duplicate.status != 0, "duplicate service is refused")
        let checkout = root.appendingPathComponent("source-checkout", isDirectory: true)
        try FileManager.default.createDirectory(at: checkout, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try await git(["init", "--initial-branch=main", checkout.path])
        let committed = checkout.appendingPathComponent("committed.txt")
        try Data("committed fixture\n".utf8).write(to: committed)
        try await git(["-C", checkout.path, "add", "committed.txt"])
        try await git(["-C", checkout.path, "-c", "user.name=RepoReach Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Fixture"])
        try Data("staged fixture\n".utf8).write(to: committed)
        try await git(["-C", checkout.path, "add", "committed.txt"])
        try Data("unstaged fixture\n".utf8).write(to: committed)
        try Data("untracked fixture\n".utf8).write(to: checkout.appendingPathComponent("untracked.txt"))
        let sourceStatus = try await git(["-C", checkout.path, "status", "--porcelain=v1"])
        let sourceDiff = try await git(["-C", checkout.path, "diff", "--binary"])
        let stagedDiff = try await git(["-C", checkout.path, "diff", "--cached", "--binary"])
        let traceBefore = (try? Data(contentsOf: trace)) ?? Data()
        let adoption = RepositoryAdoptionRequest(remoteURL: checkout.path, owner: "fixture.group", name: "adopted.repo", branch: nil)
        let adopted: EngineStatus = try await client.request("POST", path: "/v1/repositories/adopt", body: adoption, timeout: 125)
        let adoptedID = "fixture.group/adopted.repo"
        try expect(adopted.account == nil && adopted.repositories.contains(where: { $0.id == adoptedID && $0.isManual && $0.state == "virtual" }), "local adoption without GitHub sign-in")
        let hiddenGroup: EngineStatus = try await client.request("POST", path: "/v1/organizations/settings", body: OrganizationSettingsRequest(owner: "fixture.group", enabled: false))
        try expect(hiddenGroup.organizations.contains(where: { $0.name == "fixture.group" && !$0.enabled }), "group disable contract")
        try expect(hiddenGroup.repositories.first(where: { $0.id == adoptedID })?.isEnabled(in: hiddenGroup.organizations) == false, "group effectively hides manual repository")
        let individuallyHidden: EngineStatus = try await client.request("POST", path: "/v1/repositories/visibility", body: RepositoryVisibilityRequest(id: adoptedID, enabled: false))
        try expect(individuallyHidden.repositories.first(where: { $0.id == adoptedID })?.disabled == true, "individual disable contract")
        let restoredGroup: EngineStatus = try await client.request("POST", path: "/v1/organizations/settings", body: OrganizationSettingsRequest(owner: "fixture.group", enabled: true))
        try expect(restoredGroup.repositories.first(where: { $0.id == adoptedID })?.isEnabled(in: restoredGroup.organizations) == false, "group enable retains individual hidden setting")
        let restoredRepository: EngineStatus = try await client.request("POST", path: "/v1/repositories/visibility", body: RepositoryVisibilityRequest(id: adoptedID, enabled: true))
        try expect(restoredRepository.repositories.first(where: { $0.id == adoptedID })?.isEnabled(in: restoredRepository.organizations) == true, "repository re-enabled contract")
        try expect(try await git(["-C", checkout.path, "status", "--porcelain=v1"]) == sourceStatus, "original staged and untracked status unchanged")
        try expect(try await git(["-C", checkout.path, "diff", "--binary"]) == sourceDiff, "original uncommitted contents unchanged")
        try expect(try await git(["-C", checkout.path, "diff", "--cached", "--binary"]) == stagedDiff, "original staged contents unchanged")
        try expect(((try? Data(contentsOf: trace)) ?? Data()) == traceBefore, "adoption and visibility never invoke GitHub CLI")
        let session: AuthSession = try await client.request("POST", path: "/v1/auth/start")
        try expect(session.pending, "auth flow is asynchronous")
        var pending: AuthSession?
        for _ in 0..<50 {
            pending = try await client.request("GET", path: "/v1/auth/status")
            if pending?.deviceCode != nil { break }
            try await Task.sleep(nanoseconds: 50_000_000)
        }
        try expect(pending?.deviceCode == "AB12-CD34", "device code contract")
        try expect(pending?.authorizationURL == "https://github.com/login/device", "authorization URL normalized")
        try Data().write(to: release)
        var authenticated: AuthSession?
        for _ in 0..<50 {
            authenticated = try await client.request("GET", path: "/v1/auth/status")
            if authenticated?.authenticated == true { break }
            try await Task.sleep(nanoseconds: 50_000_000)
        }
        try expect(authenticated?.account?.login == "fixture-account" && authenticated?.deviceCode == nil, "completed auth contract")
        let discovered: EngineStatus = try await client.request("POST", path: "/v1/discover")
        try expect(discovered.repositories.count == 2 && discovered.repositories.contains(where: { $0.id == "fixture-account/fixture-repo" && $0.privateRepository }), "discovery/private repo contract")
        let moved = root.appendingPathComponent("moved", isDirectory: true)
        let changed: EngineStatus = try await client.request("POST", path: "/v1/settings", body: ["mountRoot": moved.path])
        try expect(changed.mountRoot == moved.path, "settings migration")
        var rootRefused = false
        do {
            let _: EmptyResponse = try await client.request("POST", path: "/v1/settings", body: ["mountRoot": "/"])
        } catch let failure as EngineFailure {
            rootRefused = true
            try expect(!failure.message.isEmpty, "settings structured error")
        }
        try expect(rootRefused, "unsafe mount root refused")
        do {
            let _: EmptyResponse = try await client.request("POST", path: "/v1/repositories/action", body: ["id": "missing/repo", "action": "keep"])
            throw EngineFailure(message: "Unregistered repo was accepted")
        } catch let failure as EngineFailure {
            try expect(failure.message.contains("catalogue"), "action structured error")
        }
        let _: EmptyResponse = try await client.request("POST", path: "/v1/repositories/action", body: ["id": "fixture-account/fixture-repo", "action": "free"])
        var operation: EngineOperation?
        for _ in 0..<50 {
            let status: EngineStatus = try await client.request("GET", path: "/v1/status")
            operation = status.operations.last
            if operation?.isRunning == false { break }
            try await Task.sleep(nanoseconds: 50_000_000)
        }
        try expect(operation?.repositoryID == "fixture-account/fixture-repo" && operation?.action == "free" && operation?.isRunning == false, "async operation contract")
        if initial?.dependencyReady == false {
            var mountRefused = false
            do {
                let _: EmptyResponse = try await client.request("POST", path: "/v1/mount")
            } catch let failure as EngineFailure {
                mountRefused = true
                try expect(failure.message.contains("macOS 26"), "native filesystem OS requirement error")
            }
            try expect(mountRefused, "mount refused without dependency")
        }
        service.terminate(); service.waitUntilExit()
        try expect(!FileManager.default.fileExists(atPath: socket.path), "graceful shutdown removes socket")
        print("PASS: local adoption without sign-in, original dirty/staged work preservation, group/repository visibility, status, duplicate startup, device auth, discovery, settings, operation/error decoding, dependency failure, graceful shutdown")
    }

    @discardableResult
    static func git(_ arguments: [String]) async throws -> Data {
        let result = try await CommandRunner.run(executable: URL(fileURLWithPath: "/usr/bin/git"), arguments: arguments)
        try expect(result.status == 0, "synthetic local Git fixture setup")
        return result.output
    }

    static func expect(_ condition: Bool, _ message: String) throws {
        guard condition else { throw EngineFailure(message: "Smoke failed: " + message) }
    }
}
