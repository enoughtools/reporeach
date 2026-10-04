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
    @Published var selectedRepositoryID: String?
    let demoMode: Bool
    var showSettingsHandler: (() -> Void)?

    private let service = EngineService()
    private var pollTask: Task<Void, Never>?
    private var startupTask: Task<Void, Never>?
    private var busyCount = 0
    private var finderSnapshot: FinderStatusSnapshot?
    private var completingSignIn = false
    private var invalidated = false

    init(demoMode: Bool = false) {
        self.demoMode = demoMode
        launchAtLogin = SMAppService.mainApp.status == .enabled
        if demoMode { status = .demo; selectedRepositoryID = "enoughtools/reporeach"; isStarting = false; serviceRunning = true }
    }

    var repositories: [RepositoryRecord] { status?.repositories ?? [] }
    var owners: [String] { Array(Set(repositories.map(\.owner))).sorted { $0.localizedStandardCompare($1) == .orderedAscending } }
    var filteredRepositories: [RepositoryRecord] {
        repositories.filter { repository in
            (ownerFilter == nil || repository.owner == ownerFilter) &&
            (search.isEmpty || repository.id.localizedCaseInsensitiveContains(search) || repository.description.localizedCaseInsensitiveContains(search))
        }.sorted { lhs, rhs in
            if lhs.pinned != rhs.pinned { return lhs.pinned }
            return lhs.id.localizedStandardCompare(rhs.id) == .orderedAscending
        }
    }

    func start() async {
        guard !demoMode, !invalidated else { return }
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
                guard let ready else { throw EngineFailure(message: "The repository service did not start. Reopen RepoReach, or inspect the service log in Application Support/RepoReach.") }
                apply(ready)
            }
            errorMessage = nil
            startPolling()
        } catch { show(error) }
    }

    func refreshStatus() async {
        guard !demoMode else { return }
        if !serviceRunning { await start(); return }
        do {
            let loaded: EngineStatus = try await service.client.request("GET", path: "/v1/status", timeout: 6)
            apply(loaded)
        } catch { serviceRunning = false; show(error) }
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
        serviceRunning = true
        if !service.isIsolated { UserDefaults.standard.set(loaded.mountRoot, forKey: "mountRoot") }
        if let selectedRepositoryID, !loaded.repositories.contains(where: { $0.id == selectedRepositoryID }) { self.selectedRepositoryID = nil }
        if let ownerFilter, !owners.contains(ownerFilter) { self.ownerFilter = nil }
        let snapshot = FinderStatusSnapshot(mountRoot: loaded.mountRoot, repositories: loaded.repositories.map {
            FinderRepositoryStatus(id: $0.id, state: $0.state, pinned: $0.pinned, error: $0.error)
        })
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
        await perform {
            let _: EmptyResponse = try await self.service.client.request("POST", path: "/v1/repositories/action", body: ["id": repository.id, "action": action.rawValue])
            await self.refreshStatus()
        }
    }

    func handleActionURL(_ url: URL) async {
        guard let route = ActionRoute(url: url) else { errorMessage = "This RepoReach action is invalid."; return }
        if status == nil || !serviceRunning { await start() }
        guard let repository = repositories.first(where: { $0.id == route.repo }), let action = RepositoryAction(rawValue: route.action.rawValue) else {
            errorMessage = "This repository is no longer in your catalogue. Refresh your repositories in RepoReach."
            return
        }
        await self.action(repository, action)
    }

    func mount() async {
        guard !demoMode else { return }
        await perform {
            let _: EmptyResponse = try await self.service.client.request("POST", path: "/v1/mount", timeout: 45)
            await self.refreshStatus()
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
        panel.message = "Choose an empty folder, or create a new one. RepoReach will display your GitHub repositories here."
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
        guard status.mounted || demoMode else { errorMessage = "Connect your repository folder first."; return }
        var url = URL(fileURLWithPath: status.mountRoot, isDirectory: true)
        if let repository {
            guard ActionRoute.isValidRepositoryID(repository.id) else { return }
            url.appendPathComponent(repository.owner, isDirectory: true); url.appendPathComponent(repository.name, isDirectory: true)
        }
        if !NSWorkspace.shared.open(url), !demoMode { errorMessage = "Finder could not open this folder. Check that your repository folder is connected." }
    }

    func showSettings() { showSettingsHandler?() }
    func showFinderExtensionSettings() {
        if let url = URL(string: "x-apple.systempreferences:com.apple.ExtensionsPreferences") { NSWorkspace.shared.open(url) }
    }
    func openDependencyPage() { NSWorkspace.shared.open(URL(string: "https://github.com/enoughtools/reporeach/blob/main/docs/reporeach/platform-setup.md")!) }

    func setLaunchAtLogin(_ enabled: Bool) async {
        guard !demoMode else { return }
        do {
            if enabled { try SMAppService.mainApp.register() } else { try await SMAppService.mainApp.unregister() }
            launchAtLogin = SMAppService.mainApp.status == .enabled
            if enabled && SMAppService.mainApp.status == .requiresApproval {
                errorMessage = "Allow RepoReach in System Settings → General → Login Items to finish enabling startup."
                SMAppService.openSystemSettingsLoginItems()
            }
        } catch { show(error); launchAtLogin = SMAppService.mainApp.status == .enabled }
    }

    func dismissError() { errorMessage = nil }
    func stop() { invalidated = true; pollTask?.cancel(); pollTask = nil; startupTask?.cancel(); startupTask = nil; service.stop() }

    private func perform(_ work: () async throws -> Void) async {
        busyCount += 1; isBusy = true; errorMessage = nil
        defer { busyCount -= 1; isBusy = busyCount > 0 }
        do { try await work() } catch { show(error) }
    }
    private func show(_ error: Error) { errorMessage = EngineClient.redact(error.localizedDescription) }
}
