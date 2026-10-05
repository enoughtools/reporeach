import Darwin
import Foundation
import Security

/// The filesystem's Unix socket must live in a container authorized for both
/// signed processes. A security-scoped resource folder alone cannot authorize
/// a sandboxed extension to connect to a Unix socket.
struct FSBridgeContainer: Sendable {
    let groupIdentifier: String
    let directory: URL

    enum Failure: Error, LocalizedError {
        case unavailable

        var errorDescription: String? {
            "RepoReach's private filesystem connection is unavailable. Reinstall the complete signed app."
        }
    }

    static func resolve(bundle: Bundle = .main) throws -> FSBridgeContainer {
        let identifier = bundle.object(forInfoDictionaryKey: "RepoReachAppGroupIdentifier") as? String
        guard let task = SecTaskCreateFromSelf(nil) else { throw Failure.unavailable }
        let groups = SecTaskCopyValueForEntitlement(task, "com.apple.security.application-groups" as CFString, nil)
        var code: SecCode?
        var staticCode: SecStaticCode?
        var information: CFDictionary?
        guard SecCodeCopySelf(SecCSFlags(), &code) == errSecSuccess, let code,
              SecCodeCopyStaticCode(code, SecCSFlags(), &staticCode) == errSecSuccess, let staticCode,
              SecCodeCopySigningInformation(staticCode, SecCSFlags(rawValue: kSecCSSigningInformation), &information) == errSecSuccess,
              let team = (information as? [String: Any])?[kSecCodeInfoTeamIdentifier as String] as? String else {
            throw Failure.unavailable
        }
        // Foundation can return a URL for an unclaimed group. Check the actual
        // running process's signed entitlement before requesting its container.
        guard let identifier, validIdentifier(identifier), identifier == team + ".rr",
              let signedGroups = groups as? [String], signedGroups.contains(identifier) else {
            throw Failure.unavailable
        }
        return try validated(groupIdentifier: identifier, signedGroups: signedGroups, teamIdentifier: team,
                             containerURL: FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: identifier))
    }

    /// Validation is separate so tests can exercise a non-nil Foundation result
    /// without giving the test runner an application-group entitlement.
    static func validated(groupIdentifier: String?, signedGroups: Any?, teamIdentifier: String?, containerURL: URL?) throws -> FSBridgeContainer {
        guard let groupIdentifier, validIdentifier(groupIdentifier),
              let teamIdentifier, groupIdentifier == teamIdentifier + ".rr",
              let signedGroups = signedGroups as? [String], signedGroups.contains(groupIdentifier),
              let containerURL, containerURL.isFileURL else { throw Failure.unavailable }
        let path = containerURL.path
        guard path.hasPrefix("/"), !path.utf8.contains(0),
              let canonical = realpath(path, nil) else { throw Failure.unavailable }
        defer { free(canonical) }
        guard String(cString: canonical) == path else { throw Failure.unavailable }
        let directory = Darwin.open(path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard directory >= 0 else { throw Failure.unavailable }
        defer { Darwin.close(directory) }
        var info = stat()
        guard fstat(directory, &info) == 0, info.st_uid == getuid(),
              info.st_mode & mode_t(S_IFMT) == mode_t(S_IFDIR),
              info.st_mode & 0o777 == 0o700 else { throw Failure.unavailable }
        // Leave room for "/b" plus the 16-byte hexadecimal state identifier
        // and the terminating NUL. Do not rely on character counts for UTF-8.
        guard path.utf8.count + 18 < MemoryLayout.size(ofValue: sockaddr_un().sun_path) else {
            throw Failure.unavailable
        }
        try verifyAccess(directory: directory)
        return FSBridgeContainer(groupIdentifier: groupIdentifier, directory: containerURL)
    }

    func validateSocket(path: String) throws {
        let prefix = directory.path + "/"
        guard path.hasPrefix(prefix), !path.utf8.contains(0),
              path.utf8.count < MemoryLayout.size(ofValue: sockaddr_un().sun_path) else {
            throw Failure.unavailable
        }
        let name = Array(path.dropFirst(prefix.count).utf8)
        guard name.count == 17, name.first == 98,
              name.dropFirst().allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }) else {
            throw Failure.unavailable
        }
        var info = stat()
        guard lstat(path, &info) == 0, info.st_uid == getuid(),
              info.st_mode & mode_t(S_IFMT) == mode_t(S_IFSOCK),
              info.st_mode & 0o777 == 0o600 else { throw Failure.unavailable }
    }

    private static func validIdentifier(_ identifier: String) -> Bool {
        let bytes = Array(identifier.utf8)
        return bytes.count == 13 && bytes.suffix(3).elementsEqual(".rr".utf8) &&
            bytes.prefix(10).allSatisfy({ (48...57).contains($0) || (65...90).contains($0) })
    }

    private static func verifyAccess(directory: Int32) throws {
        let name = ".rr-access-" + UUID().uuidString
        let file = openat(directory, name, O_RDWR | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600)
        guard file >= 0 else { throw Failure.unavailable }
        var needsCleanup = true
        defer {
            if needsCleanup { _ = unlinkat(directory, name, 0) }
            Darwin.close(file)
        }
        var info = stat()
        var written: UInt8 = 0x52
        var received: UInt8 = 0
        guard fstat(file, &info) == 0, info.st_uid == getuid(),
              info.st_mode & mode_t(S_IFMT) == mode_t(S_IFREG), info.st_mode & 0o777 == 0o600,
              Darwin.write(file, &written, 1) == 1, pread(file, &received, 1, 0) == 1,
              received == written, unlinkat(directory, name, 0) == 0 else { throw Failure.unavailable }
        needsCleanup = false
    }
}
