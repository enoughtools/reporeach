import Foundation
import Darwin

struct EngineFailure: LocalizedError, Equatable {
    enum Origin: Equatable { case client, recoveryService }
    let message: String
    let origin: Origin
    init(message: String, origin: Origin = .client) {
        self.message = message
        self.origin = origin
    }
    var errorDescription: String? { message }
}

struct CommandResult {
    let status: Int32
    let output: Data
    let errorOutput: Data
}

/// All GitHub credentials stay inside gh/ArtifactFS; the app passes only control arguments.
enum CommandRunner {
    static func run(executable: URL, arguments: [String], timeout: TimeInterval = 45) async throws -> CommandResult {
        try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                let process = Process()
                process.executableURL = executable
                process.arguments = arguments
                process.environment = ProcessInfo.processInfo.environment
                let output = Pipe(), errorOutput = Pipe()
                process.standardOutput = output; process.standardError = errorOutput
                do { try process.run() }
                catch { continuation.resume(throwing: EngineFailure(message: "Could not start the repository service: \(error.localizedDescription)")); return }
                let outputBox = OutputBox(), errorBox = OutputBox()
                let readers = DispatchGroup()
                readers.enter()
                DispatchQueue.global(qos: .utility).async {
                    outputBox.data = readBounded(output.fileHandleForReading)
                    readers.leave()
                }
                readers.enter()
                DispatchQueue.global(qos: .utility).async {
                    errorBox.data = readBounded(errorOutput.fileHandleForReading)
                    readers.leave()
                }
                let deadline = DispatchWorkItem {
                    if process.isRunning { process.terminate() }
                }
                DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + timeout, execute: deadline)
                let forceStop = DispatchWorkItem {
                    if process.isRunning { kill(process.processIdentifier, SIGKILL) }
                }
                DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + timeout + 2, execute: forceStop)
                process.waitUntilExit()
                deadline.cancel()
                forceStop.cancel()
                readers.wait()
                continuation.resume(returning: CommandResult(status: process.terminationStatus, output: outputBox.data, errorOutput: errorBox.data))
            }
        }
    }

    private final class OutputBox: @unchecked Sendable { var data = Data() }
    private static func readBounded(_ handle: FileHandle) -> Data {
        var result = Data()
        while true {
            let chunk = handle.availableData
            if chunk.isEmpty { break }
            if result.count < 32 * 1024 * 1024 { result.append(chunk.prefix(32 * 1024 * 1024 - result.count)) }
        }
        return result
    }
}

struct EngineClient {
    let executable: URL
    let socket: URL

    /// Recovery is an explicit operation. A normal mount request never invokes
    /// this endpoint or retries it after an error.
    func recoverVirtualFolders() async throws -> EngineStatus {
        try await request("POST", path: "/v1/mount/recover", timeout: 90)
    }

    static func recoveryFailureMessage(_ error: Error) -> String {
        if let failure = error as? EngineFailure, failure.origin == .recoveryService {
            return "Virtual folders couldn't be recovered. " + redact(failure.message)
        }
        // Transport and malformed-response errors cannot establish whether
        // recovery reached the service. Do not suggest a restart or retry it.
        return "EnoughRepos couldn't confirm recovery. Check the service status, and close files, terminals and Finder windows using virtual folders before trying Recover Virtual Folders again."
    }

    func request<T: Decodable>(_ method: String, path: String, body: [String: String]? = nil, timeout: TimeInterval = 45) async throws -> T {
        try await execute(method, path: path, encodedBody: body.map { try Self.encodeBody($0) }, timeout: timeout)
    }

    func request<T: Decodable, Body: Encodable>(_ method: String, path: String, body: Body, timeout: TimeInterval = 45) async throws -> T {
        try await execute(method, path: path, encodedBody: Self.encodeBody(body), timeout: timeout)
    }

    static func encodeBody<Body: Encodable>(_ body: Body) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        return try encoder.encode(body)
    }

    private func execute<T: Decodable>(_ method: String, path: String, encodedBody: Data?, timeout: TimeInterval) async throws -> T {
        var arguments = ["desktop", "request", "--socket", socket.path, "--method", method, "--path", path]
        if let encodedBody {
            guard let json = String(data: encodedBody, encoding: .utf8) else { throw EngineFailure(message: "Could not encode the request.") }
            arguments += ["--body", json]
        }
        let response = try await CommandRunner.run(executable: executable, arguments: arguments, timeout: timeout)
        return try Self.decode(response)
    }

    static func decode<T: Decodable>(_ response: CommandResult) throws -> T {
        if response.status != 0 {
            if let failure = try? JSONDecoder().decode(ErrorResponse.self, from: response.output) {
                // CLI transport failures also use {error:...}. Only the
                // recovery route's explicit marker proves a service response.
                throw EngineFailure(message: redact(failure.error), origin: failure.recoveryError == true ? .recoveryService : .client)
            }
            let detail = String(data: response.errorOutput, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            throw EngineFailure(message: detail.isEmpty ? "The repository service is unavailable. Reopen EnoughRepos to start it again." : redact(detail))
        }
        do { return try JSONDecoder().decode(T.self, from: response.output) }
        catch { throw EngineFailure(message: "The repository service returned an unreadable response. Update EnoughRepos and try again.") }
    }

    private struct ErrorResponse: Decodable {
        let error: String
        let recoveryError: Bool?
    }

    static func redact(_ value: String, limit: Int = 1800, redactUserInfo: Bool = true) -> String {
        var result = value
        var expressions = ["(?i)gh[pousr]_[A-Za-z0-9_]+", "(?i)github_pat_[A-Za-z0-9_]+", "(?i)glpat-[A-Za-z0-9_-]+", "(?i)Bearer\\s+[^\\s]+"]
        if redactUserInfo { expressions.append("(?i)(?:https?|ssh|git|file)://[^/\\s@]+@") }
        for expression in expressions {
            if let regex = try? NSRegularExpression(pattern: expression) {
                result = regex.stringByReplacingMatches(in: result, range: NSRange(result.startIndex..., in: result), withTemplate: "[redacted]")
            }
        }
        return String(result.prefix(limit))
    }
}
