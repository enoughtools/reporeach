import AppKit
import Combine
import ServiceManagement

@MainActor
final class RepositoryStore: ObservableObject {
    @Published private(set) var status: EngineStatus?
    @Published var search = ""
    @Published var ownerFilter: String?
    @Published private(set) var isBusy = false
    @Published private(set) var errorMessage: String?
    @Published private(set) var authSession: AuthSession?
    @Published private(set) var isStarting = true
    @Published private(set) var serviceRunning = false
    @Published private(set) var launchAtLogin = false
    @Published private(set) var filesystemExtension: FilesystemExtensionAvailability = .checking
    @Published private(set) var isRecoveringVirtualFolders = false
    @Published private(set) var mountRecoveryError: String?
    @Published var selectedRepositoryID: String?
    let demoMode: Bool
    var showSettingsHandler: (() -> Void)?

    private let service = EngineService()
    private var pollTask: Task<Void, Never>?
    private var startupTask: Task<Void, Never>?
    private var filesystemCheckTask: Task<Void, Never>?
    private var busyCount = 0
    private var finderSnapshot: FinderStatusSnapshot?
    private var repositoryActivity: NSObjectProtocol?
    private var pendingRepositoryActions = 0
    private var actionRevision = 0
    private var completingSignIn = false
    private var invalidated = false

    init(demoMode: Bool = false) {
        self.demoMode = demoMode
        launchAtLogin = SMAppService.mainApp.status == .enabled
        if demoMode { status = .demo; selectedRepositoryID = "enoughtools/reporeach"; isStarting = false; serviceRunning = true; filesystemExtension = .enabled }
    }

    var repositories: [RepositoryRecord] { status?.repositories ?? [] }
    var organizations: [OrganizationRecord] {
        (status?.organizations ?? []).sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
    }
    func isRepositoryEnabled(_ repository: RepositoryRecord) -> Bool { repository.isEnabled(in: organizations) }
    func isOwnerEnabled(_ owner: String) -> Bool { organizations.first(where: { $0.name.caseInsensitiveCompare(owner) == .orderedSame })?.enabled ?? true }
    func isRepositoryWorking(_ repository: RepositoryRecord) -> Bool {
        repository.isWorking || status?.operations.contains(where: { $0.repositoryID == repository.id && $0.isRunning }) == true
    }
    func isOwnerWorking(_ owner: String) -> Bool { repositories.contains { $0.owner.caseInsensitiveCompare(owner) == .orderedSame && isRepositoryWorking($0) } }
    var owners: [String] {
        var seen = Set<String>()
        return (organizations.map(\.name) + repositories.map(\.owner))
            .filter { seen.insert($0.lowercased()).inserted }
            .sorted { $0.localizedStandardCompare($1) == .orderedAscending }
    }
    var filteredRepositories: [RepositoryRecord] {
        repositories.filter { repository in
            (ownerFilter == nil || repository.owner.caseInsensitiveCompare(ownerFilter ?? "") == .orderedSame) &&
            (search.isEmpty || repository.id.localizedCaseInsensitiveContains(search) || repository.description.localizedCaseInsensitiveContains(search))
        }.sorted { lhs, rhs in
            if lhs.pinned != rhs.pinned { return lhs.pinned }
            return lhs.id.localizedStandardCompare(rhs.id) == .orderedAscending
        }
    }

    func start() async {
        guard !demoMode, !invalidated else { return }
        Task { await self.checkFilesystemExtension() }
        if let startupTask { await startupTask.value; return }
        let task = Task { await self.startService() }
        startupTask = task
        await task.value
        startupTask = nil
    }

    private func startService() async {
        guard !demoMode, !invalidated else { return }
        isStarting = true
        defer { isStarting = false }
        do {
            if let existing: EngineStatus = try? await service.client.request("GET", path: "/v1/status", timeout: 3) {
                apply(existing)
            } else {
                let defaultRoot = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Repositories", isDirectory: true).path
                let seedRoot = ProcessInfo.processInfo.environment["REPOREACH_MOUNT_ROOT"] ?? (service.isIsolated ? defaultRoot : UserDefaults.standard.string(forKey: "mountRoot")) ?? defaultRoot
                try service.start(mountRoot: seedRoot)
                var ready: EngineStatus?
                for _ in 0..<30 {
                    if invalidated { return }
                    if let loaded: EngineStatus = try? await service.client.request("GET", path: "/v1/status", timeout: 3) { ready = loaded; break }
                    try await Task.sleep(nanoseconds: 200_000_000)
                }
                guard let ready else { throw EngineFailure(message: "The repository service did not start. Reopen EnoughRepos, or inspect the service log in Application Support/RepoReach.") }
                apply(ready)
            }
            errorMessage = nil
            startPolling()
        } catch { show(error) }
    }

    func refreshStatus() async {
        guard !demoMode else { return }
        if !serviceRunning { await start(); return }
        let revision = actionRevision
        do {
            let loaded: EngineStatus = try await service.client.request("GET", path: "/v1/status", timeout: 6)
            // An older poll must not erase an operation accepted while its
            // response was in flight. The next poll sees the accepted work.
            guard revision == actionRevision else { return }
            apply(loaded)
        } catch {
            guard revision == actionRevision else { return }
            serviceRunning = false; updateRepositoryActivity(); show(error)
        }
    }

    private func startPolling() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 3_000_000_000)
                guard let self, !Task.isCancelled, !self.invalidated else { return }
                await self.refreshStatus()
                if self.authSession?.pending == true {
                    do {
                        let session: AuthSession = try await self.service.client.request("GET", path: "/v1/auth/status", timeout: 65)
                        self.authSession = session
                        if let error = session.error, !error.isEmpty { self.errorMessage = EngineClient.redact(error) }
                        if session.authenticated && !self.completingSignIn {
                            self.completingSignIn = true
                            await self.discover()
                            self.authSession = nil
                            self.completingSignIn = false
                        }
                    } catch { self.show(error) }
                }
            }
        }
    }

    private func apply(_ loaded: EngineStatus) {
        status = loaded
        if loaded.mounted { mountRecoveryError = nil }
        serviceRunning = true
        updateRepositoryActivity()
        if !service.isIsolated { UserDefaults.standard.set(loaded.mountRoot, forKey: "mountRoot") }
        if let selectedRepositoryID, !loaded.repositories.contains(where: { $0.id == selectedRepositoryID }) { self.selectedRepositoryID = nil }
        if let ownerFilter, !owners.contains(where: { $0.caseInsensitiveCompare(ownerFilter) == .orderedSame }) { self.ownerFilter = nil }
        let snapshot = loaded.finderSnapshot
        if snapshot != finderSnapshot {
            do {
                let cache = service.isIsolated ? FinderStatusCache(fileURL: service.stateDirectory.appendingPathComponent("finder-status.json")) : FinderStatusCache()
                try cache.write(snapshot)
                finderSnapshot = snapshot
                if !service.isIsolated {
                    DistributedNotificationCenter.default().postNotificationName(NSNotification.Name(FinderStatusCache.statusChangedNotificationName), object: nil, userInfo: nil, deliverImmediately: true)
                }
            } catch {
                // Finder status is a convenience; a failed cache write must not interrupt Git work.
            }
        }
    }

    func adoptRepository(remoteURL: String, owner: String?, name: String?, branch: String?) async -> Bool {
        guard !demoMode else { return false }
        var accepted = false
        await perform {
            let validated = try RepositoryInput.validateRemoteURL(remoteURL)
            let previousIDs = Set(self.repositories.map(\.id))
            let request = RepositoryAdoptionRequest(remoteURL: validated, owner: owner, name: name, branch: branch)
            let loaded: EngineStatus = try await self.service.client.request("POST", path: "/v1/repositories/adopt", body: request, timeout: 125)
            self.apply(loaded)
            if let added = loaded.repositories.first(where: { !previousIDs.contains($0.id) }) {
                self.selectedRepositoryID = added.id
            }
            self.ownerFilter = nil
            self.search = ""
            accepted = true
        }
        return accepted
    }

    func setOrganizationEnabled(_ owner: String, _ enabled: Bool) async {
        guard !demoMode else { return }
        await perform {
            let request = OrganizationSettingsRequest(owner: owner, enabled: enabled)
            let loaded: EngineStatus = try await self.service.client.request("POST", path: "/v1/organizations/settings", body: request)
            self.apply(loaded)
        }
    }

    func setRepositoryEnabled(_ repository: RepositoryRecord, _ enabled: Bool) async {
        guard !demoMode else { return }
        await perform {
            let request = RepositoryVisibilityRequest(id: repository.id, enabled: enabled)
            let loaded: EngineStatus = try await self.service.client.request("POST", path: "/v1/repositories/visibility", body: request)
            self.apply(loaded)
        }
    }

    func discover() async {
        guard !demoMode else { return }
        await perform {
            let loaded: EngineStatus = try await self.service.client.request("POST", path: "/v1/discover", timeout: 365)
            self.apply(loaded)
        }
    }

    func signIn() async {
        guard !demoMode else { return }
        await perform {
            let session: AuthSession = try await self.service.client.request("POST", path: "/v1/auth/start", timeout: 65)
            self.authSession = session
            if let error = session.error, !error.isEmpty { throw EngineFailure(message: error) }
            if let value = session.authorizationURL, let url = URL(string: value), url.scheme == "https", url.host == "github.com" {
                NSWorkspace.shared.open(url)
            }
            if session.authenticated { await self.discover(); self.authSession = nil }
        }
    }

    func action(_ repository: RepositoryRecord, _ action: RepositoryAction) async {
        guard !demoMode else { return }
        pendingRepositoryActions += 1
        updateRepositoryActivity()
        defer { pendingRepositoryActions -= 1; updateRepositoryActivity() }
        await perform {
            let response: RepositoryActionResponse = try await self.service.client.request("POST", path: "/v1/repositories/action", body: ["id": repository.id, "action": action.rawValue])
            self.actionRevision &+= 1
            // Publish accepted work immediately, including time waiting for a
            // repository lock or its preview before its state starts changing.
            if let status = self.status { self.apply(status.recording(response.operation)) }
            await self.refreshStatus()
        }
    }

    func handleActionURL(_ url: URL) async {
        guard let route = ActionRoute(url: url) else { errorMessage = "This EnoughRepos action is invalid."; return }
        if status == nil || !serviceRunning { await start() }
        guard let repository = repositories.first(where: { $0.id == route.repo }), let action = RepositoryAction(rawValue: route.action.rawValue) else {
            errorMessage = "This repository is no longer in your catalogue. Refresh your repositories in EnoughRepos."
            return
        }
        await self.action(repository, action)
    }

    func mount() async {
        guard !demoMode else { return }
        await perform {
            #if REPOREACH_NATIVE_FSKIT
            await self.checkFilesystemExtension()
            guard self.filesystemExtension == .enabled else { return }
            #endif
            let _: EmptyResponse = try await self.service.client.request("POST", path: "/v1/mount", timeout: 45)
            await self.refreshStatus()
        }
    }

    func recoverVirtualFolders() async {
        guard !demoMode, !invalidated, !isBusy, !isRecoveringVirtualFolders,
              status?.canRecoverVirtualFolders == true else { return }
        isRecoveringVirtualFolders = true
        mountRecoveryError = nil
        actionRevision &+= 1
        updateRepositoryActivity()
        defer {
            isRecoveringVirtualFolders = false
            updateRepositoryActivity()
        }
        await perform {
            do {
                let loaded = try await self.service.client.recoverVirtualFolders()
                guard !self.invalidated else { return }
                // A poll started during recovery must not overwrite its result.
                self.actionRevision &+= 1
                self.apply(loaded)
            } catch {
                guard !self.invalidated else { return }
                self.actionRevision &+= 1
                self.mountRecoveryError = EngineClient.recoveryFailureMessage(error)
            }
        }
    }

    func unmount() async {
        guard !demoMode else { return }
        await perform {
            let _: EmptyResponse = try await self.service.client.request("POST", path: "/v1/unmount", timeout: 45)
            await self.refreshStatus()
        }
    }

    func chooseMountFolder() {
        guard !demoMode else { return }
        let panel = NSOpenPanel()
        panel.title = "Choose your repository folder"
        panel.message = "Choose where EnoughRepos groups your repositories. Existing local checkouts stay in their original folders."
        panel.prompt = "Use Folder"; panel.canChooseDirectories = true; panel.canChooseFiles = false
        panel.canCreateDirectories = true; panel.allowsMultipleSelection = false
        if let current = status?.mountRoot { panel.directoryURL = URL(fileURLWithPath: current).deletingLastPathComponent() }
        panel.begin { [weak self] result in
            guard result == .OK, let path = panel.url?.path else { return }
            Task { @MainActor in
                guard let self else { return }
                await self.perform {
                    let _: EmptyResponse = try await self.service.client.request("POST", path: "/v1/settings", body: ["mountRoot": path])
                    await self.refreshStatus()
                }
            }
        }
    }

    func openFolder(_ repository: RepositoryRecord? = nil) {
        guard let status else { return }
        var url = URL(fileURLWithPath: status.mountRoot, isDirectory: true)
        if let repository {
            if !repository.isLocal {
                guard status.mounted || demoMode else { errorMessage = "Enable virtual folders to open this repository."; return }
                guard isRepositoryEnabled(repository) else { errorMessage = "Enable this repository and its group to show its folder in Finder."; return }
            }
            guard let folderURL = repository.folderURL(in: status.mountRoot) else { return }
            url = folderURL
        }
        if !NSWorkspace.shared.open(url), !demoMode { errorMessage = "Finder could not open this folder. Check that it is still available on your Mac." }
    }

    func showSettings() { showSettingsHandler?() }
    func checkFilesystemExtension() async {
        guard !demoMode, !invalidated else { return }
        if let filesystemCheckTask { await filesystemCheckTask.value; return }
        let task = Task {
            let availability = await FilesystemExtensionDiscovery.check()
            guard !self.invalidated else { return }
            self.filesystemExtension = availability
        }
        filesystemCheckTask = task
        await task.value
        filesystemCheckTask = nil
    }
    func showFinderExtensionSettings() {
        if let url = URL(string: "x-apple.systempreferences:com.apple.ExtensionsPreferences") { NSWorkspace.shared.open(url) }
    }
    func showFilesystemExtensionSettings() {
        if let url = URL(string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"), NSWorkspace.shared.open(url) { return }
        NSWorkspace.shared.open(URL(fileURLWithPath: "/System/Applications/System Settings.app"))
    }
    func openDependencyPage() {
        #if REPOREACH_NATIVE_FSKIT
        let document = "native-fskit.md"
        #else
        let document = "platform-setup.md"
        #endif
        let revision = Bundle.main.object(forInfoDictionaryKey: "RepoReachSourceRevision") as? String ?? "codex/native-fskit"
        if let url = URL(string: "https://github.com/enoughtools/reporeach/blob/\(revision)/docs/reporeach/\(document)") {
            NSWorkspace.shared.open(url)
        }
    }

    func setLaunchAtLogin(_ enabled: Bool) async {
        guard !demoMode else { return }
        do {
            if enabled { try SMAppService.mainApp.register() } else { try await SMAppService.mainApp.unregister() }
            launchAtLogin = SMAppService.mainApp.status == .enabled
            if enabled && SMAppService.mainApp.status == .requiresApproval {
                errorMessage = "Allow EnoughRepos in System Settings → General → Login Items to finish enabling startup."
                SMAppService.openSystemSettingsLoginItems()
            }
        } catch { show(error); launchAtLogin = SMAppService.mainApp.status == .enabled }
    }

    func dismissError() { errorMessage = nil }
    // Prove detachment before terminating the helper that serves file operations.
    // A busy volume must keep its service and stores alive.
    func prepareToQuit() async -> Bool {
        guard !demoMode else { return true }
        if isStarting { await startupTask?.value }
        guard serviceRunning || service.isRunning else { return true }
        do {
            let loaded: EngineStatus = try await service.client.request("POST", path: "/v1/prepare-quit", timeout: 45)
            apply(loaded)
            guard !loaded.mounted else { throw EngineFailure(message: "The repository folder is still in use. Close files and terminals using it before quitting EnoughRepos.") }
            return true
        } catch {
            show(error)
            return false
        }
    }

    func stop() {
        invalidated = true
        pollTask?.cancel(); pollTask = nil
        startupTask?.cancel(); startupTask = nil
        filesystemCheckTask?.cancel(); filesystemCheckTask = nil
        if let repositoryActivity { ProcessInfo.processInfo.endActivity(repositoryActivity); self.repositoryActivity = nil }
        service.stop()
    }

    private func updateRepositoryActivity() {
        // Keep background status/progress updates responsive while an accepted
        // operation runs. Ordinary idle browsing remains eligible for App Nap.
        let active = !invalidated && (pendingRepositoryActions > 0 ||
            isRecoveringVirtualFolders ||
            serviceRunning && status?.operations.contains(where: \.isRunning) == true)
        if active, repositoryActivity == nil {
            repositoryActivity = ProcessInfo.processInfo.beginActivity(options: .userInitiatedAllowingIdleSystemSleep,
                                                                       reason: "Updating repository downloads")
        } else if !active, let repositoryActivity {
            ProcessInfo.processInfo.endActivity(repositoryActivity)
            self.repositoryActivity = nil
        }
    }

    private func perform(_ work: () async throws -> Void) async {
        busyCount += 1; isBusy = true; errorMessage = nil
        defer { busyCount -= 1; isBusy = busyCount > 0 }
        do { try await work() } catch { show(error) }
    }
    private func show(_ error: Error) { errorMessage = EngineClient.redact(error.localizedDescription) }
}
