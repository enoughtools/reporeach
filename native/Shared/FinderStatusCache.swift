import Foundation
import Darwin

struct FinderRepositoryStatus: Codable, Equatable {
    let id: String
    let state: String
    let pinned: Bool
    let error: String?
    let localPath: String?
    let localKind: String?
    let downloadedBytes: Int64

    enum CodingKeys: String, CodingKey { case id, state, pinned, error, localPath, localKind, downloadedBytes }

    init(id: String, state: String, pinned: Bool, error: String? = nil, localPath: String? = nil, localKind: String? = nil,
         downloadedBytes: Int64 = 0) {
        self.id = id
        self.state = state
        self.pinned = pinned
        self.error = error
        self.localPath = localPath
        self.localKind = localKind
        self.downloadedBytes = downloadedBytes
    }

    init(from decoder: Decoder) throws {
        let value = try decoder.container(keyedBy: CodingKeys.self)
        id = try value.decode(String.self, forKey: .id)
        state = try value.decode(String.self, forKey: .state)
        pinned = try value.decode(Bool.self, forKey: .pinned)
        error = try value.decodeIfPresent(String.self, forKey: .error)
        localPath = try value.decodeIfPresent(String.self, forKey: .localPath)
        localKind = try value.decodeIfPresent(String.self, forKey: .localKind)
        downloadedBytes = try value.decodeIfPresent(Int64.self, forKey: .downloadedBytes) ?? 0
    }

    var isAdopted: Bool { localKind == "adopted" && localURL != nil }
    var isVirtual: Bool { localPath == nil && localKind == nil }
    var isWorking: Bool { ["preparing", "downloading", "hydrating", "pinning"].contains(state) }
    var canFreeStorage: Bool {
        localKind != "adopted" && !isWorking &&
            (localURL != nil || pinned || downloadedBytes > 0 || ["available", "ready", "pinned"].contains(state))
    }
    var localURL: URL? {
        guard ["adopted", "materialized"].contains(localKind ?? ""), let localPath else { return nil }
        return FinderStatusCache.directoryURL(for: localPath)
    }
}

struct FinderStatusSnapshot: Codable, Equatable {
    let mountRoot: String
    let repositories: [FinderRepositoryStatus]
    let virtualRoot: String?

    init(mountRoot: String, repositories: [FinderRepositoryStatus], virtualRoot: String? = nil) {
        self.mountRoot = mountRoot
        self.repositories = repositories
        self.virtualRoot = virtualRoot
    }
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
        // Foundation remaps even the named user's home to the extension's
        // container. The account database supplies the shared host location;
        // the existing file-only entitlement controls read access to it.
        let home = accountHomeDirectory()
            ?? FileManager.default.homeDirectoryForCurrentUser
        return home
            .appendingPathComponent("Library/Application Support/RepoReach", isDirectory: true)
            .appendingPathComponent("finder-status.json", isDirectory: false)
    }

    static func accountHomeDirectory(forUserID userID: uid_t = getuid()) -> URL? {
        let maximumBufferSize = 1_024 * 1_024
        let recommended = sysconf(_SC_GETPW_R_SIZE_MAX)
        var bufferSize = recommended > 0 ? min(Int(recommended), maximumBufferSize) : 16_384
        while true {
            var buffer = [CChar](repeating: 0, count: bufferSize)
            let result: (status: Int32, path: String?) = buffer.withUnsafeMutableBufferPointer { storage in
                var entry = passwd()
                var found: UnsafeMutablePointer<passwd>?
                let status = getpwuid_r(userID, &entry, storage.baseAddress, storage.count, &found)
                guard status == 0, found != nil, let directory = entry.pw_dir else { return (status, nil) }
                // Copy while the reentrant lookup's backing buffer is alive.
                return (status, String(validatingCString: directory))
            }
            if result.status == ERANGE {
                guard bufferSize < maximumBufferSize else { return nil }
                bufferSize = min(bufferSize * 2, maximumBufferSize)
                continue
            }
            guard result.status == 0, let path = result.path else { return nil }
            return directoryURL(for: path)
        }
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
        roots(in: snapshot)?.catalogue
    }

    static func virtualRootURL(in snapshot: FinderStatusSnapshot) -> URL? {
        roots(in: snapshot)?.virtual
    }

    static func directoryURL(for path: String) -> URL? {
        guard path.hasPrefix("/"),
              !path.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }),
              !path.split(separator: "/").contains(where: { $0 == "." || $0 == ".." }) else { return nil }
        let url = URL(fileURLWithPath: path, isDirectory: true).standardizedFileURL
        return url.path == "/" ? nil : url
    }

    static func repositoryURLs(for repository: FinderRepositoryStatus, in snapshot: FinderStatusSnapshot) -> [URL] {
        guard ActionRoute.isValidRepositoryID(repository.id), let roots = roots(in: snapshot) else { return [] }
        var urls = [roots.catalogue.appendingPathComponent(repository.id, isDirectory: true)]
        if let localURL = repository.localURL, !urls.contains(localURL) { urls.append(localURL) }
        if repository.isVirtual, let virtualRoot = roots.virtual {
            urls.append(virtualRoot.appendingPathComponent(repository.id, isDirectory: true))
        }
        return urls
    }

    static func observedDirectoryURLs(in snapshot: FinderStatusSnapshot) -> Set<URL> {
        guard let roots = roots(in: snapshot) else { return [] }
        var urls = Set([roots.catalogue] + snapshot.repositories.compactMap(\.localURL))
        if snapshot.repositories.contains(where: \.isVirtual), let virtualRoot = roots.virtual { urls.insert(virtualRoot) }
        return urls
    }

    /// Resolve the containing repository from a selection without reading files.
    /// Component comparison keeps /Repos-other outside a /Repos catalogue.
    static func repositoryID(for selectionURL: URL, in snapshot: FinderStatusSnapshot) -> String? {
        guard selectionURL.isFileURL, let roots = roots(in: snapshot) else { return nil }
        let selectedComponents = selectionURL.standardizedFileURL.pathComponents
        var matches: [(id: String, depth: Int)] = []
        var catalogueRoots = [(roots.catalogue, false)]
        if let virtualRoot = roots.virtual { catalogueRoots.append((virtualRoot, true)) }
        for (root, virtualOnly) in catalogueRoots {
            let components = root.pathComponents
            if selectedComponents.count >= components.count + 2, selectedComponents.starts(with: components) {
                let id = "\(selectedComponents[components.count])/\(selectedComponents[components.count + 1])"
                if ActionRoute.isValidRepositoryID(id), snapshot.repositories.contains(where: { $0.id == id && (!virtualOnly || $0.isVirtual) }) {
                    matches.append((id, components.count + 2))
                }
            }
        }
        for repository in snapshot.repositories {
            if let url = repository.localURL, ActionRoute.isValidRepositoryID(repository.id) {
                let components = url.pathComponents
                if selectedComponents.starts(with: components) { matches.append((repository.id, components.count)) }
            }
        }
        // The deepest registered checkout owns a selection. An overlapping
        // equal-depth registration is ambiguous and must offer no action.
        guard let depth = matches.map(\.depth).max() else { return nil }
        let ids = Set(matches.filter { $0.depth == depth }.map(\.id))
        return ids.count == 1 ? ids.first : nil
    }

    private static func isValid(_ snapshot: FinderStatusSnapshot) -> Bool {
        guard mountRootURL(in: snapshot) != nil, snapshot.repositories.count <= 100_000 else { return false }
        var identifiers = Set<String>()
        return snapshot.repositories.allSatisfy { repository in
            let localMetadataValid = repository.localPath == nil && repository.localKind == nil || repository.localURL != nil
            return localMetadataValid && ActionRoute.isValidRepositoryID(repository.id) && identifiers.insert(repository.id).inserted
        }
    }

    private static func roots(in snapshot: FinderStatusSnapshot) -> (catalogue: URL, virtual: URL?)? {
        guard let catalogue = directoryURL(for: snapshot.mountRoot) else { return nil }
        guard let path = snapshot.virtualRoot else { return (catalogue, nil) }
        guard let virtual = directoryURL(for: path),
              !catalogue.pathComponents.starts(with: virtual.pathComponents),
              !virtual.pathComponents.starts(with: catalogue.pathComponents) else { return nil }
        return (catalogue, virtual)
    }

    enum CacheError: Error {
        case invalidSnapshot
        case tooLarge
        case cannotCreateTemporaryFile
    }
}
