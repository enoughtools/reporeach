import Foundation
import Darwin
import os

struct FSBridgeHTTPResponse: Sendable {
    let status: Int
    let headers: [String: String]
    let contentLength: Int?
    let body: Data
}

private final class FSBridgeCancellation: @unchecked Sendable {
    private let lock = NSLock()
    private var cancelled = false
    private var descriptor: Int32 = -1
    private var completion: ((Result<FSBridgeHTTPResponse, Error>) -> Void)?

    func cancel() {
        lock.lock()
        cancelled = true
        if descriptor >= 0 { Darwin.shutdown(descriptor, SHUT_RDWR) }
        let callback = completion
        completion = nil
        lock.unlock()
        callback?(.failure(FSBridgeError.cancelled))
    }

    func installCompletion(_ callback: @escaping (Result<FSBridgeHTTPResponse, Error>) -> Void) {
        lock.lock()
        let wasCancelled = cancelled
        if !wasCancelled { completion = callback }
        lock.unlock()
        if wasCancelled { callback(.failure(FSBridgeError.cancelled)) }
    }

    func finish(_ result: Result<FSBridgeHTTPResponse, Error>) {
        lock.lock()
        let callback = completion
        completion = nil
        lock.unlock()
        callback?(result)
    }

    func check() throws {
        lock.lock()
        let value = cancelled
        lock.unlock()
        if value { throw FSBridgeError.cancelled }
    }

    func own(_ fd: Int32) throws {
        lock.lock()
        defer { lock.unlock() }
        guard !cancelled else { throw FSBridgeError.cancelled }
        descriptor = fd
    }

    func release(_ fd: Int32) {
        // Closing while holding the lock prevents cancellation from shutting
        // down a reused descriptor belonging to a different request.
        lock.lock()
        descriptor = -1
        Darwin.close(fd)
        lock.unlock()
    }
}

final class FSBridgeTransport: @unchecked Sendable {
    // Only fixed operation names and numeric errno values belong in this log.
    private static let logger = Logger(subsystem: "com.enoughtools.reporeach.fsbridge", category: "Transport")
    private let configuration: FSBridgeConfiguration
    private let queue: OperationQueue
    private let lock = NSLock()
    private let pending = DispatchGroup()
    private var closed = false
    private var controls: [UUID: FSBridgeCancellation] = [:]

    init(configuration: FSBridgeConfiguration) {
        self.configuration = configuration
        queue = OperationQueue()
        queue.name = "com.enoughtools.reporeach.fsbridge"
        queue.maxConcurrentOperationCount = 8
        queue.qualityOfService = .userInitiated
    }

    func send(method: String, path: String, body: Data, contentType: String,
              maximumResponse: Int, timeout: TimeInterval) async throws -> FSBridgeHTTPResponse {
        guard ["GET", "POST", "PUT"].contains(method), path.hasPrefix("/v1/fs"),
              path.utf8.allSatisfy({ $0 > 32 && $0 < 127 }),
              ["application/json", "application/octet-stream"].contains(contentType),
              body.count <= FSBridgeClient.maximumChunkSize,
              maximumResponse >= 0, maximumResponse <= 4 * 1_048_576,
              timeout.isFinite, timeout > 0, timeout <= 600 else { throw FSBridgeError.invalidRequest }
        let control = FSBridgeCancellation()
        let id = UUID()
        return try await withTaskCancellationHandler(operation: {
            if Task.isCancelled { throw FSBridgeError.cancelled }
            return try await withCheckedThrowingContinuation { continuation in
                control.installCompletion { continuation.resume(with: $0) }
                lock.lock()
                guard !closed else {
                    lock.unlock()
                    control.finish(.failure(FSBridgeError.unavailable(ENOTCONN)))
                    return
                }
                // Bound both worker sockets and queued binary payloads. The
                // volume has an eight-operation gate, but admission remains
                // bounded if another caller bypasses that gate.
                guard controls.count < 32 else {
                    lock.unlock()
                    control.finish(.failure(FSBridgeError.unavailable(EAGAIN)))
                    return
                }
                controls[id] = control
                pending.enter()
                lock.unlock()
                let deadline = ProcessInfo.processInfo.systemUptime + timeout
                queue.addOperation {
                    let result: Result<FSBridgeHTTPResponse, Error>
                    do {
                        result = .success(try self.perform(method: method, path: path, body: body,
                                                           contentType: contentType, maximumResponse: maximumResponse,
                                                           deadline: deadline, control: control))
                    } catch { result = .failure(error) }
                    self.lock.lock()
                    self.controls.removeValue(forKey: id)
                    self.lock.unlock()
                    control.finish(result)
                    self.pending.leave()
                }
            }
        }, onCancel: { control.cancel() })
    }

    func close() async {
        let active = beginClose()
        active.forEach { $0.cancel() }
        await withCheckedContinuation { continuation in
            pending.notify(queue: .global(qos: .userInitiated)) { continuation.resume() }
        }
    }

    private func beginClose() -> [FSBridgeCancellation] {
        lock.lock()
        defer { lock.unlock() }
        closed = true
        return Array(controls.values)
    }

    private func perform(method: String, path: String, body: Data, contentType: String,
                         maximumResponse: Int, deadline: TimeInterval,
                         control: FSBridgeCancellation) throws -> FSBridgeHTTPResponse {
        try control.check()
        let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            let error = Darwin.errno
            Self.logger.error("operation=socket errno=\(error, privacy: .public)")
            throw FSBridgeError.unavailable(error)
        }
        do { try control.own(fd) }
        catch { Darwin.close(fd); throw error }
        defer { control.release(fd) }
        var noSigPipe: Int32 = 1
        guard setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size)) == 0 else {
            let error = Darwin.errno
            Self.logger.error("operation=setsockopt.SO_NOSIGPIPE errno=\(error, privacy: .public)")
            throw FSBridgeError.unavailable(error)
        }
        guard fcntl(fd, F_SETFD, FD_CLOEXEC) == 0 else {
            let error = Darwin.errno
            Self.logger.error("operation=fcntl.F_SETFD errno=\(error, privacy: .public)")
            throw FSBridgeError.unavailable(error)
        }
        guard fcntl(fd, F_SETFL, O_NONBLOCK) == 0 else {
            let error = Darwin.errno
            Self.logger.error("operation=fcntl.F_SETFL errno=\(error, privacy: .public)")
            throw FSBridgeError.unavailable(error)
        }

        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        let pathBytes = Array(configuration.socketPath.utf8) + [0]
        withUnsafeMutableBytes(of: &address.sun_path) { destination in
            destination.copyBytes(from: pathBytes)
        }
        let connected = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if connected != 0 {
            let connectError = Darwin.errno
            guard connectError == EINPROGRESS else {
                Self.logger.error("operation=connect errno=\(connectError, privacy: .public)")
                try control.check()
                throw FSBridgeError.unavailable(connectError)
            }
            try wait(fd, event: Int16(POLLOUT), deadline: deadline, control: control)
            var error: Int32 = 0
            var length = socklen_t(MemoryLayout<Int32>.size)
            guard getsockopt(fd, SOL_SOCKET, SO_ERROR, &error, &length) == 0 else {
                let optionError = Darwin.errno
                Self.logger.error("operation=getsockopt.SO_ERROR errno=\(optionError, privacy: .public)")
                throw FSBridgeError.unavailable(optionError)
            }
            guard error == 0 else {
                Self.logger.error("operation=SO_ERROR errno=\(error, privacy: .public)")
                throw FSBridgeError.unavailable(error)
            }
        }

        let head = "\(method) \(path) HTTP/1.1\r\nHost: localhost\r\nAuthorization: \(configuration.authorizationHeader)\r\nContent-Type: \(contentType)\r\nContent-Length: \(body.count)\r\nConnection: close\r\n\r\n"
        try send(Data(head.utf8), descriptor: fd, deadline: deadline, control: control)
        try send(body, descriptor: fd, deadline: deadline, control: control)
        let reader = FSBridgeSocketReader(descriptor: fd, deadline: deadline, control: control)
        let headerData = try reader.readHeader(maximum: 16_384)
        guard headerData.allSatisfy({ $0 < 128 }), let header = String(data: headerData, encoding: .ascii) else {
            throw FSBridgeError.malformedResponse
        }
        let lines = header.components(separatedBy: "\r\n")
        let statusParts = lines[0].split(separator: " ", maxSplits: 2)
        guard statusParts.count >= 2, ["HTTP/1.1", "HTTP/1.0"].contains(String(statusParts[0])),
              statusParts[1].count == 3, let status = Int(statusParts[1]), (200...599).contains(status) else {
            throw FSBridgeError.malformedResponse
        }
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":"), colon != line.startIndex else { throw FSBridgeError.malformedResponse }
            let name = line[..<colon].lowercased()
            guard name.utf8.allSatisfy({ (97...122).contains($0) || (48...57).contains($0) || $0 == 45 }),
                  headers[name] == nil else { throw FSBridgeError.malformedResponse }
            let value = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
            guard value.utf8.allSatisfy({ $0 >= 32 && $0 != 127 }) else { throw FSBridgeError.malformedResponse }
            headers[name] = value
        }
        let responseBody: Data
        let contentLength: Int?
        if let rawLength = headers["content-length"] {
            guard headers["transfer-encoding"] == nil, !rawLength.isEmpty,
                  rawLength.utf8.allSatisfy({ (48...57).contains($0) }), let count = Int(rawLength) else {
                throw FSBridgeError.malformedResponse
            }
            guard count <= maximumResponse else { throw FSBridgeError.responseTooLarge }
            contentLength = count
            responseBody = try reader.readExactly(count)
        } else if let transfer = headers["transfer-encoding"] {
            guard transfer.lowercased() == "chunked" else { throw FSBridgeError.malformedResponse }
            contentLength = nil
            responseBody = try reader.readChunked(maximum: maximumResponse)
        } else {
            // The request explicitly asks for Connection: close, so EOF is a
            // valid bounded HTTP/1 framing mode for JSON error responses.
            contentLength = nil
            responseBody = try reader.readToEnd(maximum: maximumResponse)
        }
        return FSBridgeHTTPResponse(status: status, headers: headers, contentLength: contentLength, body: responseBody)
    }

    private func send(_ data: Data, descriptor: Int32, deadline: TimeInterval,
                      control: FSBridgeCancellation) throws {
        var offset = 0
        while offset < data.count {
            try wait(descriptor, event: Int16(POLLOUT), deadline: deadline, control: control)
            let count = data.withUnsafeBytes { buffer in
                Darwin.send(descriptor, buffer.baseAddress!.advanced(by: offset), data.count - offset, 0)
            }
            if count < 0 && [EINTR, EAGAIN, EWOULDBLOCK].contains(Darwin.errno) { continue }
            guard count > 0 else { try control.check(); throw FSBridgeError.unavailable(Darwin.errno) }
            offset += count
        }
    }
}

private func wait(_ descriptor: Int32, event: Int16, deadline: TimeInterval,
                  control: FSBridgeCancellation) throws {
    while true {
        try control.check()
        let remaining = deadline - ProcessInfo.processInfo.systemUptime
        guard remaining > 0 else { throw FSBridgeError.timedOut }
        var descriptorState = pollfd(fd: descriptor, events: event, revents: 0)
        let result = Darwin.poll(&descriptorState, 1, Int32(min(remaining * 1_000 + 1, 100)))
        if result < 0 && Darwin.errno == EINTR { continue }
        guard result >= 0 else { try control.check(); throw FSBridgeError.unavailable(Darwin.errno) }
        if result == 0 { continue }
        try control.check()
        if descriptorState.revents & Int16(POLLNVAL) != 0 { throw FSBridgeError.unavailable(EBADF) }
        if descriptorState.revents & (event | Int16(POLLHUP) | Int16(POLLERR)) != 0 { return }
    }
}

private final class FSBridgeSocketReader {
    private let descriptor: Int32
    private let deadline: TimeInterval
    private let control: FSBridgeCancellation
    private var buffered = Data()
    private static let lineEnding = Data([13, 10])
    private static let headerEnding = Data([13, 10, 13, 10])

    init(descriptor: Int32, deadline: TimeInterval, control: FSBridgeCancellation) {
        self.descriptor = descriptor
        self.deadline = deadline
        self.control = control
    }

    func readHeader(maximum: Int) throws -> Data {
        while true {
            if let range = buffered.range(of: Self.headerEnding) {
                guard range.lowerBound <= maximum else { throw FSBridgeError.responseTooLarge }
                let result = Data(buffered[..<range.lowerBound])
                buffered.removeSubrange(..<range.upperBound)
                return result
            }
            guard buffered.count <= maximum else { throw FSBridgeError.responseTooLarge }
            guard try receive() else { throw FSBridgeError.malformedResponse }
        }
    }

    func readExactly(_ count: Int) throws -> Data {
        var output = Data()
        output.reserveCapacity(count)
        while output.count < count {
            if !buffered.isEmpty {
                let copied = min(buffered.count, count - output.count)
                output.append(buffered.prefix(copied))
                buffered.removeFirst(copied)
            } else if try !receive() { throw FSBridgeError.malformedResponse }
        }
        return output
    }

    func readToEnd(maximum: Int) throws -> Data {
        var output = Data()
        while true {
            guard buffered.count <= maximum - output.count else { throw FSBridgeError.responseTooLarge }
            output.append(buffered)
            buffered.removeAll(keepingCapacity: true)
            if try !receive() { return output }
        }
    }

    func readChunked(maximum: Int) throws -> Data {
        var output = Data()
        while true {
            let line = try readLine(maximum: 1_024)
            guard !line.isEmpty, line.allSatisfy({ (48...57).contains($0) || (65...70).contains($0) || (97...102).contains($0) }),
                  let value = String(data: line, encoding: .ascii), let count = Int(value, radix: 16) else {
                throw FSBridgeError.malformedResponse
            }
            if count == 0 {
                var trailerBytes = 0
                while true {
                    let trailer = try readLine(maximum: 16_384 - trailerBytes)
                    trailerBytes += trailer.count + 2
                    guard trailerBytes <= 16_384 else { throw FSBridgeError.responseTooLarge }
                    if trailer.isEmpty { return output }
                    // This private endpoint never uses trailers. Reject them
                    // rather than allowing late framing/auth header changes.
                    throw FSBridgeError.malformedResponse
                }
            }
            guard count <= maximum - output.count else { throw FSBridgeError.responseTooLarge }
            output.append(try readExactly(count))
            guard try readExactly(2) == Self.lineEnding else { throw FSBridgeError.malformedResponse }
        }
    }

    private func readLine(maximum: Int) throws -> Data {
        while true {
            if let range = buffered.range(of: Self.lineEnding) {
                guard range.lowerBound - buffered.startIndex <= maximum else { throw FSBridgeError.responseTooLarge }
                let result = Data(buffered[..<range.lowerBound])
                buffered.removeSubrange(..<range.upperBound)
                return result
            }
            guard buffered.count <= maximum else { throw FSBridgeError.responseTooLarge }
            guard try receive() else { throw FSBridgeError.malformedResponse }
        }
    }

    private func receive() throws -> Bool {
        while true {
            try wait(descriptor, event: Int16(POLLIN), deadline: deadline, control: control)
            var bytes = [UInt8](repeating: 0, count: 8_192)
            let count = Darwin.recv(descriptor, &bytes, bytes.count, 0)
            if count < 0 && [EINTR, EAGAIN, EWOULDBLOCK].contains(Darwin.errno) { continue }
            try control.check()
            guard count >= 0 else { throw FSBridgeError.unavailable(Darwin.errno) }
            if count == 0 { return false }
            buffered.append(contentsOf: bytes.prefix(count))
            return true
        }
    }
}
