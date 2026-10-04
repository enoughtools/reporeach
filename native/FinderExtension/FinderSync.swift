import AppKit
import FinderSync

/// Adds repository actions to Finder without reading or hydrating repo contents.
final class FinderSync: FIFinderSync {
    private let cache = FinderStatusCache()
    private let lock = NSLock()
    private var snapshot: FinderStatusSnapshot?
    private var observedDirectories = Set<URL>()

    override init() {
        super.init()
        installBadgeImages()
        reloadStatus()
        DistributedNotificationCenter.default().addObserver(
            self,
            selector: #selector(statusChanged(_:)),
            name: Notification.Name(FinderStatusCache.statusChangedNotificationName),
            object: nil,
            suspensionBehavior: .deliverImmediately
        )
    }

    deinit {
        DistributedNotificationCenter.default().removeObserver(self)
    }

    override func beginObservingDirectory(at url: URL) {
        lock.lock()
        observedDirectories.insert(url)
        lock.unlock()
        reloadStatus()
    }

    override func endObservingDirectory(at url: URL) {
        lock.lock()
        observedDirectories.remove(url)
        lock.unlock()
    }

    override func requestBadgeIdentifier(for url: URL) {
        guard let snapshot = currentSnapshot(),
              let id = FinderStatusCache.repositoryID(for: url, in: snapshot),
              let repository = snapshot.repositories.first(where: { $0.id == id })
        else { return }
        FIFinderSyncController.default().setBadgeIdentifier(badgeIdentifier(for: repository), for: url)
    }

    override func menu(for menuKind: FIMenuKind) -> NSMenu? {
        guard let snapshot = currentSnapshot(),
              let repo = selectedRepository(in: snapshot),
              let status = snapshot.repositories.first(where: { $0.id == repo })
        else { return nil }

        let menu = NSMenu(title: "RepoReach")
        addItem("Keep Downloaded", action: .keep, repo: repo, to: menu, enabled: !status.pinned)
        addItem("Free Up Space", action: .free, repo: repo, to: menu)
        menu.addItem(.separator())
        addItem("Refresh Repository", action: .refresh, repo: repo, to: menu)
        return menu
    }

    @objc private func statusChanged(_ notification: Notification) {
        reloadStatus()
    }

    private func currentSnapshot() -> FinderStatusSnapshot? {
        lock.lock()
        defer { lock.unlock() }
        return snapshot
    }

    private func reloadStatus() {
        let next = cache.read()
        lock.lock()
        snapshot = next
        let directories = observedDirectories
        lock.unlock()

        let controller = FIFinderSyncController.default()
        if let next, let root = FinderStatusCache.mountRootURL(in: next) {
            controller.directoryURLs = [root]
            // Rebadge repository roots already visible in Finder after an action.
            // requestBadgeIdentifier supplies child badges as Finder asks for them.
            for repository in next.repositories {
                let url = root.appendingPathComponent(repository.id, isDirectory: true)
                if directories.contains(where: { directory in
                    url.pathComponents.starts(with: directory.standardizedFileURL.pathComponents)
                }) {
                    controller.setBadgeIdentifier(badgeIdentifier(for: repository), for: url)
                }
            }
        } else {
            controller.directoryURLs = []
        }
    }

    private func selectedRepository(in snapshot: FinderStatusSnapshot) -> String? {
        let controller = FIFinderSyncController.default()
        let selections = controller.selectedItemURLs() ?? []
        if !selections.isEmpty {
            let ids = selections.compactMap { FinderStatusCache.repositoryID(for: $0, in: snapshot) }
            // Ambiguous multi-repository selections get no destructive action.
            guard ids.count == selections.count, Set(ids).count == 1 else { return nil }
            return ids.first
        }
        guard let target = controller.targetedURL() else { return nil }
        return FinderStatusCache.repositoryID(for: target, in: snapshot)
    }

    private func addItem(
        _ title: String,
        action: ActionRoute.Action,
        repo: String,
        to menu: NSMenu,
        enabled: Bool = true
    ) {
        guard let route = ActionRoute(repo: repo, action: action) else { return }
        let item = NSMenuItem(title: title, action: #selector(performRepositoryAction(_:)), keyEquivalent: "")
        item.target = self
        item.representedObject = route.url
        item.isEnabled = enabled
        menu.addItem(item)
    }

    @objc private func performRepositoryAction(_ sender: NSMenuItem) {
        guard let url = sender.representedObject as? URL, ActionRoute(url: url) != nil else { return }
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = false
        NSWorkspace.shared.open(url, configuration: configuration) { _, error in
            if error != nil {
                // The Finder extension never logs repository metadata or credentials.
                NSLog("RepoReach could not open the repository action. Open the app and try again.")
            }
        }
    }

    private func badgeIdentifier(for repository: FinderRepositoryStatus) -> String {
        if repository.error?.isEmpty == false || ["error", "failed"].contains(repository.state) {
            return "error"
        }
        if ["preparing", "downloading", "hydrating", "pinning"].contains(repository.state) {
            return "downloading"
        }
        if repository.pinned { return "pinned" }
        if ["available", "ready", "mounted", "cached", "downloaded"].contains(repository.state) { return "ready" }
        return "virtual"
    }

    private func installBadgeImages() {
        let badges: [(String, String, String, NSColor)] = [
            ("virtual", "Available on GitHub", "cloud", .systemGray),
            ("ready", "Available on demand", "checkmark.circle.fill", .systemBlue),
            ("pinned", "Kept downloaded", "checkmark.circle.fill", .systemGreen),
            ("downloading", "Downloading repository", "arrow.down.circle.fill", .systemBlue),
            ("error", "RepoReach needs attention", "exclamationmark.circle.fill", .systemOrange)
        ]
        let controller = FIFinderSyncController.default()
        for (identifier, label, symbolName, color) in badges {
            guard let symbol = NSImage(systemSymbolName: symbolName, accessibilityDescription: label) else { continue }
            let configuration = NSImage.SymbolConfiguration(pointSize: 18, weight: .semibold)
                .applying(NSImage.SymbolConfiguration(paletteColors: [color]))
            let image = symbol.withSymbolConfiguration(configuration) ?? symbol
            image.isTemplate = false
            controller.setBadgeImage(image, label: label, forBadgeIdentifier: identifier)
        }
    }
}
