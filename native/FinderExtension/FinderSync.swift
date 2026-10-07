import AppKit
import FinderSync

/// Adds repository actions to Finder without reading or hydrating repo contents.
final class FinderSync: FIFinderSync {
    private let cache = FinderStatusCache()
    private let lock = NSLock()
    private var snapshot: FinderStatusSnapshot?
    private var badgeTracking = FinderBadgeTracking()

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
        badgeTracking.beginObserving(url)
        lock.unlock()
        reloadStatus()
    }

    override func endObservingDirectory(at url: URL) {
        lock.lock()
        badgeTracking.endObserving(url)
        lock.unlock()
    }

    override func requestBadgeIdentifier(for url: URL) {
        lock.lock()
        badgeTracking.recordBadgeRequest(url)
        lock.unlock()
        guard let snapshot = currentSnapshot(),
              let id = FinderStatusCache.repositoryID(for: url, in: snapshot),
              let repository = snapshot.repositories.first(where: { $0.id == id })
        else { return }
        FIFinderSyncController.default().setBadgeIdentifier(repository.badgeIdentifier, for: url)
    }

    override func menu(for menuKind: FIMenuKind) -> NSMenu? {
        guard let snapshot = currentSnapshot(),
              let repo = selectedRepository(in: snapshot),
              let status = snapshot.repositories.first(where: { $0.id == repo })
        else { return nil }

        let menu = NSMenu(title: "RepoReach")
        if status.isWorking {
            addStatusItem(status.operation?.stageDescription ?? "Working", to: menu)
            if let detail = status.operation?.progressDescription { addStatusItem(detail, to: menu) }
            menu.addItem(.separator())
        } else if status.error?.isEmpty == false || status.operation?.error?.isEmpty == false {
            addStatusItem("Could not finish. Open RepoReach for details.", to: menu)
            menu.addItem(.separator())
        }
        addItem("Keep Downloaded", selector: #selector(keepDownloaded(_:)), to: menu, enabled: !status.pinned && !status.isWorking)
        addItem("Free Up Space", selector: #selector(freeUpSpace(_:)), to: menu, enabled: status.canFreeStorage)
        if status.isAdopted {
            let retained = NSMenuItem(title: "Original checkout retained", action: nil, keyEquivalent: "")
            retained.isEnabled = false
            menu.addItem(retained)
        }
        menu.addItem(.separator())
        addItem("Refresh Repository", selector: #selector(refreshRepository(_:)), to: menu, enabled: !status.isWorking)
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
        let urls = next.map { badgeTracking.urlsToRebadge(in: $0) } ?? []
        lock.unlock()

        let controller = FIFinderSyncController.default()
        if let next, FinderStatusCache.mountRootURL(in: next) != nil {
            controller.directoryURLs = FinderStatusCache.observedDirectoryURLs(in: next)
            let repositories = Dictionary(uniqueKeysWithValues: next.repositories.map { ($0.id, $0) })
            // Finder does not necessarily request child badges again when the
            // repository changes. Repaint URLs it already requested, including
            // descendants of the current window rather than just repo roots.
            for url in urls {
                let repository = FinderStatusCache.repositoryID(for: url, in: next).flatMap { repositories[$0] }
                controller.setBadgeIdentifier(repository?.badgeIdentifier ?? "", for: url)
            }
        } else {
            controller.directoryURLs = []
        }
    }

    private func selectedRepository(in snapshot: FinderStatusSnapshot) -> String? {
        let controller = FIFinderSyncController.default()
        return FinderStatusCache.selectedRepositoryID(for: controller.selectedItemURLs() ?? [],
                                                     targetedURL: controller.targetedURL(), in: snapshot)
    }

    private func addItem(
        _ title: String,
        selector: Selector,
        to menu: NSMenu,
        enabled: Bool = true
    ) {
        let item = NSMenuItem(title: title, action: selector, keyEquivalent: "")
        item.target = self
        item.isEnabled = enabled
        menu.addItem(item)
    }

    private func addStatusItem(_ title: String, to menu: NSMenu) {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        menu.addItem(item)
    }

    @objc private func keepDownloaded(_ sender: Any?) { performRepositoryAction(.keep) }
    @objc private func freeUpSpace(_ sender: Any?) { performRepositoryAction(.free) }
    @objc private func refreshRepository(_ sender: Any?) { performRepositoryAction(.refresh) }

    private func performRepositoryAction(_ action: ActionRoute.Action) {
        // Finder reconstructs menu items. Its callback selection is the action
        // context; representedObject and arbitrary sender properties are not.
        let controller = FIFinderSyncController.default()
        guard let snapshot = cache.read(),
              let route = FinderStatusCache.actionRoute(for: action, selectionURLs: controller.selectedItemURLs() ?? [],
                                                       targetedURL: controller.targetedURL(), in: snapshot),
              let applicationURL = ActionRoute.containingApplicationURL(forFinderExtensionURL: Bundle.main.bundleURL),
              Bundle(url: applicationURL)?.bundleIdentifier == "com.enoughtools.reporeach" else { return }
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = false
        configuration.allowsRunningApplicationSubstitution = false
        NSWorkspace.shared.open([route.url], withApplicationAt: applicationURL, configuration: configuration) { _, error in
            if error != nil {
                // The Finder extension never logs repository metadata or credentials.
                NSLog("RepoReach could not open the repository action. Open the app and try again.")
            }
        }
    }

    private func installBadgeImages() {
        let badges: [(String, String, String, NSColor)] = [
            ("virtual", "Available on demand", "cloud", .systemGray),
            ("ready", "Available on demand", "checkmark.circle.fill", .systemBlue),
            ("local", "Local checkout", "checkmark.circle.fill", .systemGreen),
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
        for step in 0...10 {
            let fraction = Double(step) / 10
            let image = NSImage(size: NSSize(width: 24, height: 24), flipped: false) { _ in
                let center = NSPoint(x: 12, y: 12)
                let track = NSBezierPath(ovalIn: NSRect(x: 2, y: 2, width: 20, height: 20))
                track.lineWidth = 2
                NSColor.systemBlue.withAlphaComponent(0.2).setStroke()
                track.stroke()
                if step > 0 {
                    let ring = NSBezierPath()
                    ring.lineWidth = 2
                    ring.appendArc(withCenter: center, radius: 10, startAngle: 90,
                                   endAngle: 90 - CGFloat(fraction * 360), clockwise: true)
                    NSColor.systemBlue.setStroke()
                    ring.stroke()
                }
                let symbol = NSImage(systemSymbolName: "arrow.down", accessibilityDescription: nil)?.withSymbolConfiguration(
                    .init(paletteColors: [.systemBlue]))
                symbol?.draw(in: NSRect(x: 7, y: 6, width: 10, height: 12))
                return true
            }
            let label = step == 10 ? "Creating local checkout" : "Downloading: \(step * 10)%"
            controller.setBadgeImage(image, label: label, forBadgeIdentifier: "downloading-\(step)")
        }
    }
}
