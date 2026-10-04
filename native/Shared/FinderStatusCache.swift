import Foundation

struct FinderRepositoryStatus: Codable, Equatable {
    let id: String
    let state: String
    let pinned: Bool
    let error: String?

    init(id: String, state: String, pinned: Bool, error: String? = nil) {
        self.id = id
        self.state = state
        self.pinned = pinned
        self.error = error
    }
}

struct FinderStatusSnapshot: Codable, Equatable {
    let mountRoot: String
    let repositories: [FinderRepositoryStatus]
}

/// Metadata only. Finder never needs to open a repository to decide its badge.
struct FinderStatusCache {
    static let statusChangedNotificationName = "tools.enough.reporeach.statusChanged"
    static let maximumCacheBytes = 16 * 1_024 * 1_024

    let fileURL: URL

    init(fileURL: URL = FinderStatusCache.defaultFileURL) {
        self.fileURL = fileURL
    }

    static var defaultFileURL: URL {
        // The Finder extension is sandboxed. Asking for the named user's home
        // avoids placing this shared, read-only metadata in its private container.
        let home = FileManager.default.homeDirectory(forUser: NSUserName())
            ?? FileManager.default.homeDirectoryForCurrentUser
        return home
            .appendingPathComponent("Library/Application Support/RepoReach", isDirectory: true)
            .appendingPathComponent("finder-status.json", isDirectory: false)
    }

    func read() -> FinderStatusSnapshot? {
        guard let handle = try? FileHandle(forReadingFrom: fileURL) else { return nil }
        defer { try? handle.close() }
        guard let data = try? handle.read(upToCount: Self.maximumCacheBytes + 1),
              data.count <= Self.maximumCacheBytes,
              let snapshot = try? JSONDecoder().decode(FinderStatusSnapshot.self, from: data),
              Self.isValid(snapshot)
        else { return nil }
        return snapshot
    }

    func write(_ snapshot: FinderStatusSnapshot) throws {
        guard Self.isValid(snapshot) else { throw CacheError.invalidSnapshot }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(snapshot)
        guard data.count <= Self.maximumCacheBytes else { throw CacheError.tooLarge }
        let directory = fileURL.deletingLastPathComponent()
        try FileManager.default.createDirectory(
            at: directory,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        let temporary = directory.appendingPathComponent(".finder-status-\(UUID().uuidString).json")
        guard FileManager.default.createFile(
            atPath: temporary.path,
            contents: nil,
            attributes: [.posixPermissions: 0o600]
        ) else { throw CacheError.cannotCreateTemporaryFile }
        defer { try? FileManager.default.removeItem(at: temporary) }
        let handle = try FileHandle(forWritingTo: temporary)
        do {
            try handle.write(contentsOf: data)
            try handle.synchronize()
            try handle.close()
        } catch {
            try? handle.close()
            throw error
        }
        if FileManager.default.fileExists(atPath: fileURL.path) {
            _ = try FileManager.default.replaceItemAt(fileURL, withItemAt: temporary, options: .usingNewMetadataOnly)
        } else {
            try FileManager.default.moveItem(at: temporary, to: fileURL)
        }
    }

    static func mountRootURL(in snapshot: FinderStatusSnapshot) -> URL? {
        guard snapshot.mountRoot.hasPrefix("/"), !snapshot.mountRoot.contains("\0") else { return nil }
        let root = URL(fileURLWithPath: snapshot.mountRoot, isDirectory: true).standardizedFileURL
        guard root.path != "/" else { return nil }
        return root
    }

    /// Resolve the containing repository from a selection without reading files.
    /// Component comparison keeps /Repos-other outside a /Repos catalogue.
    static func repositoryID(for selectionURL: URL, in snapshot: FinderStatusSnapshot) -> String? {
        guard selectionURL.isFileURL, let root = mountRootURL(in: snapshot) else { return nil }
        let rootComponents = root.pathComponents
        let selectedComponents = selectionURL.standardizedFileURL.pathComponents
        guard selectedComponents.count >= rootComponents.count + 2,
              selectedComponents.starts(with: rootComponents)
        else { return nil }
        let owner = selectedComponents[rootComponents.count]
        let name = selectedComponents[rootComponents.count + 1]
        let id = "\(owner)/\(name)"
        guard ActionRoute.isValidRepositoryID(id), snapshot.repositories.contains(where: { $0.id == id })
        else { return nil }
        return id
    }

    private static func isValid(_ snapshot: FinderStatusSnapshot) -> Bool {
        guard mountRootURL(in: snapshot) != nil, snapshot.repositories.count <= 100_000 else { return false }
        var identifiers = Set<String>()
        return snapshot.repositories.allSatisfy { repository in
            ActionRoute.isValidRepositoryID(repository.id) && identifiers.insert(repository.id).inserted
        }
    }

    enum CacheError: Error {
        case invalidSnapshot
        case tooLarge
        case cannotCreateTemporaryFile
    }
}
