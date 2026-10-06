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
    let source: String
    let disabled: Bool
    let localPath: String?
    let localKind: String?

    enum CodingKeys: String, CodingKey {
        case id, owner, name, description, defaultBranch, htmlURL, cloneURL, state, pinned, downloadedBytes, error, source, disabled, localPath, localKind
        case privateRepository = "private"
    }

    init(id: String, owner: String, name: String, description: String, defaultBranch: String = "main",
         privateRepository: Bool = false, htmlURL: String = "", cloneURL: String = "", state: String = "virtual",
         pinned: Bool = false, downloadedBytes: Int64 = 0, error: String? = nil, source: String = "github", disabled: Bool = false,
         localPath: String? = nil, localKind: String? = nil) {
        self.id = id; self.owner = owner; self.name = name; self.description = description
        self.defaultBranch = defaultBranch; self.privateRepository = privateRepository
        self.htmlURL = htmlURL; self.cloneURL = cloneURL; self.state = state; self.pinned = pinned
        self.downloadedBytes = downloadedBytes; self.error = error
        self.source = source; self.disabled = disabled
        self.localPath = localPath; self.localKind = localKind
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
        source = try value.decodeIfPresent(String.self, forKey: .source) ?? "github"
        disabled = try value.decodeIfPresent(Bool.self, forKey: .disabled) ?? false
        localPath = try value.decodeIfPresent(String.self, forKey: .localPath)
        localKind = try value.decodeIfPresent(String.self, forKey: .localKind)
    }

    var displayState: String {
        if !isWorking && state != "error", isLocal {
            return isAdopted ? "Adopted checkout" : "Local checkout"
        }
        switch state {
        case "preparing": return "Preparing"
        case "downloading", "hydrating", "pinning": return "Downloading"
        case "pinned": return "Kept downloaded"
        case "available", "ready": return pinned ? "Kept downloaded" : "Available on demand"
        case "error": return "Needs attention"
        default: return "Online only"
        }
    }
    var isWorking: Bool { ["preparing", "downloading", "hydrating", "pinning"].contains(state) }
    var canFreeStorage: Bool {
        localKind != "adopted" && !isWorking &&
            (isLocal || pinned || downloadedBytes > 0 || ["available", "ready", "pinned"].contains(state))
    }
    var isManual: Bool { source == "manual" }
    var isLocal: Bool { localURL != nil }
    var isAdopted: Bool { localKind == "adopted" && isLocal }
    var localURL: URL? {
        guard ["adopted", "materialized"].contains(localKind ?? ""), let localPath,
              localPath.hasPrefix("/"),
              !localPath.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else { return nil }
        let url = URL(fileURLWithPath: localPath, isDirectory: true).standardizedFileURL
        return url.path == "/" ? nil : url
    }
    func folderURL(in catalogueRoot: String) -> URL? {
        if let localURL { return localURL }
        guard ActionRoute.isValidRepositoryID(id), catalogueRoot.hasPrefix("/"),
              !catalogueRoot.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else { return nil }
        let root = URL(fileURLWithPath: catalogueRoot, isDirectory: true).standardizedFileURL
        guard root.path != "/" else { return nil }
        return root.appendingPathComponent(id, isDirectory: true)
    }
    func isEnabled(in groups: [OrganizationRecord]) -> Bool {
        !disabled && (groups.first(where: { $0.name.caseInsensitiveCompare(owner) == .orderedSame })?.enabled ?? true)
    }
}

struct OrganizationRecord: Codable, Identifiable, Equatable {
    let name: String
    let enabled: Bool
    var id: String { name }
}

struct RepositoryAdoptionRequest: Encodable {
    let remoteURL: String
    let owner: String?
    let name: String?
    let branch: String?
}

struct OrganizationSettingsRequest: Encodable {
    let owner: String
    let enabled: Bool
}

struct RepositoryVisibilityRequest: Encodable {
    let id: String
    let enabled: Bool
}

/// Prevent a credential-bearing value from ever reaching CLI arguments.
/// The engine validates supported Git transports, branches, and identities.
enum RepositoryInput {
    static func validateRemoteURL(_ rawValue: String) throws -> String {
        let value = rawValue.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty, value.utf8.count <= 4096,
              !value.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else {
            throw EngineFailure(message: "Enter a Git remote URL or an existing checkout folder.")
        }
        if value.contains("://") {
            guard let components = URLComponents(string: value), components.password == nil,
                  components.query == nil, components.fragment == nil,
                  components.user == nil || components.scheme?.lowercased() == "ssh" && components.user.map(isSSHUsername) == true else {
                throw EngineFailure(message: "Remove credentials, query parameters, and fragments from the remote URL. Use SSH or your existing Git credential helper.")
            }
        } else if !value.hasPrefix("/"), ["http:", "https:", "ssh:", "git:", "file:"].contains(where: value.lowercased().hasPrefix) {
            throw EngineFailure(message: "Enter a standard Git remote URL or choose an existing local checkout folder.")
        } else if !value.hasPrefix("/"), let at = value.firstIndex(of: "@"),
                  value[..<at].firstIndex(of: "/") == nil {
            // SCP remotes carry the SSH user before @. Reject password-like or
            // token-shaped authorities before encoding them in a CLI body.
            guard isSSHUsername(String(value[..<at])) else {
                throw EngineFailure(message: "Remove credentials from the remote URL. Use SSH or your existing Git credential helper.")
            }
        }
        // Diagnostics are bounded separately; a long, safe local path must not
        // be mistaken for a credential just because it exceeds that log limit.
        guard !value.contains("?"), !value.contains("#"), EngineClient.redact(value, limit: value.count, redactUserInfo: false) == value else {
            throw EngineFailure(message: "Remove the access token from the remote URL. Use SSH or your existing Git credential helper.")
        }
        return value
    }

    private static func isSSHUsername(_ value: String) -> Bool {
        let lower = value.lowercased()
        guard !value.isEmpty, value.utf8.count <= 100, !value.hasPrefix("-"),
              value != ".", value != "..", lower != "oauth2", lower != "x-token-auth",
              !["ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-"].contains(where: lower.hasPrefix) else { return false }
        return value.utf8.allSatisfy { byte in
            (65...90).contains(byte) || (97...122).contains(byte) || (48...57).contains(byte) || [45, 46, 95].contains(byte)
        }
    }
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
    let virtualRoot: String?
    let mounted: Bool
    let dependencyReady: Bool
    let account: GitHubAccount?
    let repositories: [RepositoryRecord]
    let operations: [EngineOperation]
    let organizations: [OrganizationRecord]
    let message: String?

    init(version: String = "0.1.0", mountRoot: String, mounted: Bool = false, dependencyReady: Bool = false,
         account: GitHubAccount? = nil, repositories: [RepositoryRecord] = [], operations: [EngineOperation] = [], organizations: [OrganizationRecord] = [], message: String? = nil,
         virtualRoot: String? = nil) {
        self.version = version; self.mountRoot = mountRoot; self.mounted = mounted
        self.dependencyReady = dependencyReady; self.account = account; self.repositories = repositories
        self.operations = operations; self.organizations = organizations; self.message = message
        self.virtualRoot = virtualRoot
    }

    enum CodingKeys: String, CodingKey { case version, mountRoot, virtualRoot, mounted, dependencyReady, account, repositories, operations, organizations, message }
    init(from decoder: Decoder) throws {
        let value = try decoder.container(keyedBy: CodingKeys.self)
        version = try value.decodeIfPresent(String.self, forKey: .version) ?? "0.1.0"
        mountRoot = try value.decode(String.self, forKey: .mountRoot)
        virtualRoot = try value.decodeIfPresent(String.self, forKey: .virtualRoot)
        mounted = try value.decodeIfPresent(Bool.self, forKey: .mounted) ?? false
        dependencyReady = try value.decodeIfPresent(Bool.self, forKey: .dependencyReady) ?? false
        account = try value.decodeIfPresent(GitHubAccount.self, forKey: .account)
        repositories = try value.decodeIfPresent([RepositoryRecord].self, forKey: .repositories) ?? []
        operations = try value.decodeIfPresent([EngineOperation].self, forKey: .operations) ?? []
        organizations = try value.decodeIfPresent([OrganizationRecord].self, forKey: .organizations) ?? []
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
