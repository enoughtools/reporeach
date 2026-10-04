import Foundation

/// A small, explicitly validated command surface shared by the app and Finder.
struct ActionRoute: Equatable {
    enum Action: String, Codable, CaseIterable {
        case keep
        case free
        case refresh
        case prepare
    }

    let repo: String
    let action: Action

    init?(repo: String, action: Action) {
        guard Self.isValidRepositoryID(repo) else { return nil }
        self.repo = repo
        self.action = action
    }

    init?(url: URL) {
        guard let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              components.scheme == "reporeach",
              components.host == "action",
              components.path.isEmpty,
              components.user == nil,
              components.password == nil,
              components.port == nil,
              components.fragment == nil,
              let items = components.queryItems,
              items.count == 2,
              items.filter({ $0.name == "repo" }).count == 1,
              items.filter({ $0.name == "action" }).count == 1,
              let repo = items.first(where: { $0.name == "repo" })?.value,
              let rawAction = items.first(where: { $0.name == "action" })?.value,
              let action = Action(rawValue: rawAction)
        else { return nil }
        self.init(repo: repo, action: action)
    }

    var url: URL {
        var components = URLComponents()
        components.scheme = "reporeach"
        components.host = "action"
        components.queryItems = [
            URLQueryItem(name: "repo", value: repo),
            URLQueryItem(name: "action", value: action.rawValue)
        ]
        // All fields above are fixed or validated ASCII, so this is representable.
        return components.url!
    }

    static func isValidRepositoryID(_ value: String) -> Bool {
        let pieces = value.split(separator: "/", omittingEmptySubsequences: false)
        guard pieces.count == 2 else { return false }
        let owner = pieces[0]
        let name = pieces[1]
        guard (1...100).contains(owner.utf8.count),
              (1...100).contains(name.utf8.count),
              owner != ".", owner != "..", name != ".", name != "..",
              owner.utf8.allSatisfy({ isASCIIAlphanumeric($0) || $0 == 45 || $0 == 46 || $0 == 95 }),
              name.utf8.allSatisfy({ isASCIIAlphanumeric($0) || $0 == 45 || $0 == 46 || $0 == 95 })
        else { return false }
        return true
    }

    private static func isASCIIAlphanumeric(_ byte: UInt8) -> Bool {
        (48...57).contains(byte) || (65...90).contains(byte) || (97...122).contains(byte)
    }
}
