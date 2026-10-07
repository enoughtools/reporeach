import AppKit
import Foundation

// This tool reads registration metadata only. It never opens, resolves through,
// launches or unregisters any application URL returned by NSWorkspace.
private let productionBundleIdentifier = "com.enoughtools.reporeach"

private func hasCanonicalAbsoluteSyntax(_ path: String, maximumBytes: Int) -> Bool {
    guard path.hasPrefix("/"), path.utf8.count <= maximumBytes, !path.contains("\0") else {
        return false
    }
    return path.split(separator: "/", omittingEmptySubsequences: false).dropFirst().allSatisfy {
        !$0.isEmpty && $0 != "." && $0 != ".."
    }
}

private struct RegistrationReport: Encodable {
    let ok: Bool
    let bundle_identifier: String
    let expected_app_path: String
    let exact_app_registered: Bool?
    let application_count: Int?
    let unknowns: [String]
    let output_truncated: Bool
}

@main
private struct InspectBuiltAppRegistration {
    @MainActor
    static func main() throws {
        let arguments = CommandLine.arguments
        let expected = arguments.count == 2 ? arguments[1] : ""
        var unknowns: [String] = []
        var found: Bool?
        var count: Int?
        if !hasCanonicalAbsoluteSyntax(expected, maximumBytes: 4096) || !(expected.hasSuffix("/EnoughRepos.app") || expected.hasSuffix("/RepoReach.app")) {
            unknowns.append("invalid_expected_app_path")
        } else {
            // AppKit documents this as all copies, with an empty array when
            // there is no application with the specified bundle identifier.
            let applications = NSWorkspace.shared.urlsForApplications(withBundleIdentifier: productionBundleIdentifier)
            if applications.count > 256 {
                unknowns.append("application_inventory_exceeds_limit")
            } else {
                count = applications.count
                found = false
                for application in applications {
                    if !application.isFileURL || (application as NSURL).isFileReferenceURL() ||
                        application.baseURL != nil || application.user != nil || application.password != nil ||
                        application.query != nil || application.fragment != nil || application.port != nil ||
                        !(application.host == nil || application.host == "" || application.host == "localhost") ||
                        !hasCanonicalAbsoluteSyntax(application.path, maximumBytes: 32768) {
                        unknowns.append("invalid_application_url_metadata")
                        found = nil
                        break
                    }
                    if application.path == expected {
                        found = true
                    }
                }
            }
        }
        let report = RegistrationReport(ok: unknowns.isEmpty, bundle_identifier: productionBundleIdentifier,
                                        expected_app_path: expected, exact_app_registered: found,
                                        application_count: count, unknowns: unknowns, output_truncated: false)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        FileHandle.standardOutput.write(try encoder.encode(report) + Data([0x0a]))
        if !unknowns.isEmpty {
            exit(1)
        }
    }
}
