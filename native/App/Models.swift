import Foundation

struct GitHubAccount: Codable, Equatable {
    let login: String
    let avatarURL: String?
}

enum RepositoryAction: String, Codable {
    case keep, free, refresh, prepare, cancel
}

struct RepositoryRecord: Codable, Identifiable, Equatable {
    let id: String
    let owner: String
    let name: String
    let description: String
    let defaultBranch: String
    let privateRepository: Bool
    let htmlURL: String
    let cloneURL: String
    let state: String
    let pinned: Bool
    let downloadedBytes: Int64
    let error: String?

    enum CodingKeys: String, CodingKey {
        case id, owner, name, description, defaultBranch, htmlURL, cloneURL, state, pinned, downloadedBytes, error
        case privateRepository = "private"
    }

    init(id: String, owner: String, name: String, description: String, defaultBranch: String = "main",
         privateRepository: Bool = false, htmlURL: String = "", cloneURL: String = "", state: String = "virtual",
         pinned: Bool = false, downloadedBytes: Int64 = 0, error: String? = nil) {
        self.id = id; self.owner = owner; self.name = name; self.description = description
        self.defaultBranch = defaultBranch; self.privateRepository = privateRepository
        self.htmlURL = htmlURL; self.cloneURL = cloneURL; self.state = state; self.pinned = pinned
        self.downloadedBytes = downloadedBytes; self.error = error
    }

    init(from decoder: Decoder) throws {
        let value = try decoder.container(keyedBy: CodingKeys.self)
        id = try value.decode(String.self, forKey: .id)
        owner = try value.decode(String.self, forKey: .owner)
        name = try value.decode(String.self, forKey: .name)
        description = try value.decodeIfPresent(String.self, forKey: .description) ?? ""
        defaultBranch = try value.decodeIfPresent(String.self, forKey: .defaultBranch) ?? "main"
        privateRepository = try value.decodeIfPresent(Bool.self, forKey: .privateRepository) ?? false
        htmlURL = try value.decodeIfPresent(String.self, forKey: .htmlURL) ?? ""
        cloneURL = try value.decodeIfPresent(String.self, forKey: .cloneURL) ?? ""
        state = try value.decodeIfPresent(String.self, forKey: .state) ?? "virtual"
        pinned = try value.decodeIfPresent(Bool.self, forKey: .pinned) ?? false
        downloadedBytes = try value.decodeIfPresent(Int64.self, forKey: .downloadedBytes) ?? 0
        error = try value.decodeIfPresent(String.self, forKey: .error)
    }

    var displayState: String {
        switch state {
        case "preparing": return "Preparing"
        case "downloading": return "Downloading"
        case "pinned": return "Kept downloaded"
        case "available", "ready": return pinned ? "Kept downloaded" : "Available on demand"
        case "error": return "Needs attention"
        default: return "Online only"
        }
    }
    var isWorking: Bool { state == "preparing" || state == "downloading" }
}

struct EngineOperation: Codable, Identifiable, Equatable {
    let id: String
    let repositoryID: String?
    let action: String
    let status: String
    let completedBlobs: Int64?
    let totalBlobs: Int64?
    let downloadedBytes: Int64?
    let totalBytes: Int64?
    let currentPath: String?
    let error: String?

    var isRunning: Bool { status == "running" }
    var progress: Double? {
        guard let total = totalBlobs, total > 0, let completed = completedBlobs else { return nil }
        return min(1, max(0, Double(completed) / Double(total)))
    }
    var message: String? {
        if let currentPath, !currentPath.isEmpty { return currentPath }
        if let total = totalBlobs, let completed = completedBlobs, total > 0 {
            return "\(completed) of \(total) files"
        }
        return nil
    }
}

struct EngineStatus: Codable, Equatable {
    let version: String
    let mountRoot: String
    let mounted: Bool
    let dependencyReady: Bool
    let account: GitHubAccount?
    let repositories: [RepositoryRecord]
    let operations: [EngineOperation]
    let message: String?

    init(version: String = "0.1.0", mountRoot: String, mounted: Bool = false, dependencyReady: Bool = false,
         account: GitHubAccount? = nil, repositories: [RepositoryRecord] = [], operations: [EngineOperation] = [], message: String? = nil) {
        self.version = version; self.mountRoot = mountRoot; self.mounted = mounted
        self.dependencyReady = dependencyReady; self.account = account; self.repositories = repositories
        self.operations = operations; self.message = message
    }

    enum CodingKeys: String, CodingKey { case version, mountRoot, mounted, dependencyReady, account, repositories, operations, message }
    init(from decoder: Decoder) throws {
        let value = try decoder.container(keyedBy: CodingKeys.self)
        version = try value.decodeIfPresent(String.self, forKey: .version) ?? "0.1.0"
        mountRoot = try value.decode(String.self, forKey: .mountRoot)
        mounted = try value.decodeIfPresent(Bool.self, forKey: .mounted) ?? false
        dependencyReady = try value.decodeIfPresent(Bool.self, forKey: .dependencyReady) ?? false
        account = try value.decodeIfPresent(GitHubAccount.self, forKey: .account)
        repositories = try value.decodeIfPresent([RepositoryRecord].self, forKey: .repositories) ?? []
        operations = try value.decodeIfPresent([EngineOperation].self, forKey: .operations) ?? []
        message = try value.decodeIfPresent(String.self, forKey: .message)
    }
}

struct AuthSession: Codable, Equatable {
    let authenticated: Bool
    let account: GitHubAccount?
    let pending: Bool
    let authorizationURL: String?
    let deviceCode: String?
    let error: String?

    enum CodingKeys: String, CodingKey { case authenticated, account, pending, authorizationURL, deviceCode, error }
    init(from decoder: Decoder) throws {
        let value = try decoder.container(keyedBy: CodingKeys.self)
        authenticated = try value.decodeIfPresent(Bool.self, forKey: .authenticated) ?? false
        account = try value.decodeIfPresent(GitHubAccount.self, forKey: .account)
        pending = try value.decodeIfPresent(Bool.self, forKey: .pending) ?? false
        authorizationURL = try value.decodeIfPresent(String.self, forKey: .authorizationURL)
        deviceCode = try value.decodeIfPresent(String.self, forKey: .deviceCode)
        error = try value.decodeIfPresent(String.self, forKey: .error)
    }
}

struct EmptyResponse: Codable {}

extension EngineStatus {
    // Demo data is loaded only by an explicit --demo launch argument. No production state is seeded.
    static let demo = EngineStatus(mountRoot: "/Users/you/Repositories", mounted: true, dependencyReady: true,
        account: GitHubAccount(login: "enoughtools", avatarURL: nil), repositories: [
            RepositoryRecord(id: "enoughtools/reporeach", owner: "enoughtools", name: "reporeach", description: "All your repositories. Within reach.", state: "pinned", pinned: true, downloadedBytes: 8_400_000),
            RepositoryRecord(id: "enoughtools/enough-ui", owner: "enoughtools", name: "enough-ui", description: "A quiet design system for tools that get out of your way.", state: "available", downloadedBytes: 1_200_000),
            RepositoryRecord(id: "enoughtools/minutes", owner: "enoughtools", name: "minutes", description: "A little more time for the things that matter.", state: "virtual"),
            RepositoryRecord(id: "cloudflare/artifact-fs", owner: "cloudflare", name: "artifact-fs", description: "Git repositories, available as local working trees.", state: "virtual")])
}
