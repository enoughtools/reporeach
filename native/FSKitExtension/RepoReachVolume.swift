import Foundation
import FSKit
import Darwin
import OSLog

/// The service owns inode identities and open handles. FSKit items retain lookup
/// references until reclaim, rather than resolving mutable paths for every I/O.
@available(macOS 15.4, *)
final class RepoReachItem: FSItem {
    let inode: UInt64
    let generation: UInt64

    init(inode: UInt64, generation: UInt64) {
        self.inode = inode
        self.generation = generation
        super.init()
    }
}

/// A fair, cancellation-aware reader/writer gate. Mutations cannot overlap reads
/// or one another; up to eight reads can hydrate independent files concurrently.
private actor VolumeOperationGate {
    struct Permit { let id: UUID; let exclusive: Bool }
    private struct Waiter {
        let permit: Permit
        let resume: (Result<Permit, Error>) -> Void
        let timeout: Task<Void, Never>?
    }
    private var readers: Set<UUID> = []
    private var writer: UUID?
    private var waiters: [Waiter] = []
    private var closing = false

    func acquire(exclusive: Bool) async throws -> Permit {
        let permit = Permit(id: UUID(), exclusive: exclusive)
        return try await withTaskCancellationHandler {
            try Task.checkCancellation()
            return try await withCheckedThrowingContinuation { continuation in
                guard !closing else {
                    continuation.resume(throwing: POSIXError(.ENXIO))
                    return
                }
                guard waiters.count < 256 else {
                    continuation.resume(throwing: POSIXError(.EAGAIN))
                    return
                }
                let timeout = Task.detached {
                    do { try await Task.sleep(nanoseconds: 120_000_000_000) }
                    catch { return }
                    await self.cancel(permit.id, error: POSIXError(.ETIMEDOUT))
                }
                waiters.append(Waiter(permit: permit, resume: { continuation.resume(with: $0) }, timeout: timeout))
                admitWaiters()
            }
        } onCancel: {
            Task { await self.cancel(permit.id) }
        }
    }

    /// Stops admission, rejects queued work, then waits for admitted work to end.
    func drain() async -> Permit {
        closing = true
        let queued = waiters
        waiters.removeAll()
        for waiter in queued {
            waiter.timeout?.cancel()
            waiter.resume(.failure(POSIXError(.ENXIO)))
        }
        let permit = Permit(id: UUID(), exclusive: true)
        return await withCheckedContinuation { continuation in
            // Shutdown itself is not cancellable: handles must outlive admitted I/O.
            waiters.append(Waiter(permit: permit, resume: { result in
                // The drain waiter is never cancelled or rejected.
                if case .success(let permit) = result { continuation.resume(returning: permit) }
            }, timeout: nil))
            admitWaiters()
        }
    }

    func release(_ permit: Permit) {
        if permit.exclusive { writer = nil } else { readers.remove(permit.id) }
        admitWaiters()
    }

    func reopen() {
        guard writer == nil, readers.isEmpty, waiters.isEmpty else { return }
        closing = false
    }

    private func cancel(_ id: UUID, error: Error = POSIXError(.EINTR)) {
        guard let index = waiters.firstIndex(where: { $0.permit.id == id }) else { return }
        let waiter = waiters.remove(at: index)
        waiter.timeout?.cancel()
        waiter.resume(.failure(error))
        admitWaiters()
    }

    private func admitWaiters() {
        guard writer == nil else { return }
        while let first = waiters.first {
            if first.permit.exclusive {
                guard readers.isEmpty else { return }
                writer = first.permit.id
                first.timeout?.cancel()
                waiters.removeFirst().resume(.success(first.permit))
                return
            }
            guard readers.count < 8 else { return }
            readers.insert(first.permit.id)
            first.timeout?.cancel()
            waiters.removeFirst().resume(.success(first.permit))
        }
    }
}

/// One FSKit read callback owns this buffer until its reply. Only that callback's
/// admitted operation writes to it, serially, before invoking the reply handler.
@available(macOS 15.4, *)
private final class VolumeReadBuffer: @unchecked Sendable {
    let capacity: Int
    private let buffer: FSMutableFileDataBuffer

    init(_ buffer: FSMutableFileDataBuffer) {
        self.buffer = buffer
        capacity = buffer.withUnsafeMutableBytes { $0.count }
    }

    func copy(_ data: Data, at position: Int) {
        buffer.withUnsafeMutableBytes { destination in
            guard !data.isEmpty, let address = destination.baseAddress else { return }
            data.withUnsafeBytes { source in
                address.advanced(by: position).copyMemory(from: source.baseAddress!, byteCount: data.count)
            }
        }
    }
}

@available(macOS 15.4, *)
final class RepoReachVolume: FSVolume, FSVolume.Operations,
    FSVolume.ReadWriteOperations, FSVolume.OpenCloseOperations, FSVolume.XattrOperations {

    private struct ItemState {
        let item: RepoReachItem
        var attributes: FSBridgeAttributes
        var parent: UInt64
        var lookupReferences: UInt64
        var readHandle: UInt64?
        var writeHandle: UInt64?
        var handle: UInt64? { writeHandle ?? readHandle }
        var handles: [UInt64] { Array(Set([readHandle, writeHandle].compactMap { $0 })).sorted() }
        var openModes: FSVolume.OpenModes = []
        var removed = false
        var reclaimed = false
    }
    private struct DirectorySession {
        let inode: UInt64
        let handle: UInt64
        let includesDotEntries: Bool
        var lastUsed: Date
        var eofOffset: UInt64?
        var page: DirectoryPage?
    }
    private struct DirectoryPage {
        let startOffset: UInt64
        let entries: [FSBridgeDirectoryEntry]
        let nextOffset: UInt64
        let eof: Bool
        var pendingForgets: [FSBridgeForget]
        var cleanupFailed = false
    }
    private struct RunningOperation {
        let task: Task<Void, Never>
        let cancelOnDrain: Bool
    }
    private struct IOResult { let count: Int; let error: Error? }

    private var client: FSBridgeClient
    private let loadReadOnly: Bool
    private var sessionReadOnly = false
    private var readOnly: Bool { locked { loadReadOnly || sessionReadOnly } }
    private let gate = VolumeOperationGate()
    private let lock = NSLock()
    private let logger = Logger(subsystem: "com.enoughtools.reporeach", category: "FSKitVolume")
    private var items: [UInt64: ItemState] = [:]
    private var rootItem: RepoReachItem?
    private var directories: [UInt64: DirectorySession] = [:]
    private var transientReferences: [UInt64: UInt64] = [:]
    private var pendingHandles: [UInt64: UInt64] = [:]
    private var pendingOpenCount = 0
    private var verifierCounter: UInt64 = 1
    private var running: [UUID: RunningOperation] = [:]
    private var isClosing = false
    private var isActive = false
    private var isMounted = false
    private var shutdownTask: Task<Void, Never>?
    private var unmountTask: Task<Void, Never>?
    private var needsReconnection = false
    private var sessionEpoch: UInt64 = 0
    private var statistics: FSBridgeStat?

    init(client: FSBridgeClient, identifier: UUID, readOnly: Bool) {
        self.client = client
        self.loadReadOnly = readOnly
        super.init(volumeID: FSVolume.Identifier(uuid: identifier),
                   volumeName: FSFileName(string: "EnoughRepos"))
    }

    var maximumLinkCount: Int { 1 }
    var maximumNameLength: Int { 255 }
    var restrictsOwnershipChanges: Bool { true }
    var truncatesLongNames: Bool { false }
    var maximumFileSize: UInt64 { UInt64(Int64.max) }
    var maximumXattrSize: Int { FSBridgeClient.maximumXattrSize }

    var supportedVolumeCapabilities: FSVolume.SupportedCapabilities {
        let capabilities = FSVolume.SupportedCapabilities()
        capabilities.supportsSymbolicLinks = true
        capabilities.supportsHardLinks = false
        capabilities.supportsPersistentObjectIDs = false
        capabilities.supports64BitObjectIDs = true
        capabilities.supportsSparseFiles = true
        capabilities.supportsZeroRuns = false
        capabilities.supports2TBFiles = true
        capabilities.supportsFastStatFS = false
        capabilities.supportsHiddenFiles = false
        capabilities.doesNotSupportImmutableFiles = true
        capabilities.doesNotSupportRootTimes = true
        capabilities.doesNotSupportSettingFilePermissions = readOnly
        capabilities.doesNotSupportVolumeSizes = true
        capabilities.caseFormat = .sensitive
        return capabilities
    }

    var volumeStatistics: FSStatFSResult {
        let result = FSStatFSResult(fileSystemTypeName: "reporeach")
        result.blockSize = 4096
        result.ioSize = 1_048_576
        if let stat = locked({ statistics }) {
            result.blockSize = Int(clamping: max(1, stat.blockSize))
            result.ioSize = Int(clamping: max(stat.blockSize, stat.ioSize))
            result.totalBlocks = stat.blocks
            result.freeBlocks = stat.blocksFree
            result.availableBlocks = stat.blocksAvailable
            result.usedBlocks = stat.blocks >= stat.blocksFree ? stat.blocks - stat.blocksFree : 0
            result.totalFiles = stat.inodes
            result.freeFiles = stat.inodesFree
        }
        return result
    }

    func activate(options: FSTaskOptions,
                  replyHandler: @escaping (FSItem?, Error?) -> Void) {
        activate(taskOptions: options.taskOptions, replyHandler: replyHandler)
    }

    /// The host-side entry point uses the same option policy as FSKit without
    /// constructing framework-owned task options.
    func activate(replyHandler: @escaping (FSItem?, Error?) -> Void) {
        activate(taskOptions: [], replyHandler: replyHandler)
    }

    func activate(taskOptions: [String], replyHandler: @escaping (FSItem?, Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            let policy = try self.nextSessionReadOnly(taskOptions)
            try await self.ensureConnection()
            let root = try self.requireNode(try await self.client.request(FSBridgeRequest(op: "getattr", inode: 1)))
            guard root.inode == 1, root.attributes.type == .dir else { throw POSIXError(.EIO) }
            let item = try self.retain(root, parent: 1, grantsReference: false)
            let response = try await self.client.request(FSBridgeRequest(op: "statfs"))
            self.locked {
                self.statistics = response.stat
                self.sessionReadOnly = policy
                self.isActive = true
            }
            return item
        }
    }

    func mount(options: FSTaskOptions, replyHandler: @escaping (Error?) -> Void) {
        mount(taskOptions: options.taskOptions, replyHandler: replyHandler)
    }

    func mount(replyHandler: @escaping (Error?) -> Void) {
        mount(taskOptions: [], replyHandler: replyHandler)
    }

    func mount(taskOptions: [String], replyHandler: @escaping (Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            guard self.locked({ self.isActive }) else { throw POSIXError(.ENXIO) }
            let policy = try self.nextSessionReadOnly(taskOptions)
            try await self.ensureConnection()
            let root = try self.requireNode(try await self.client.request(FSBridgeRequest(op: "getattr", inode: 1)))
            guard root.inode == 1, root.attributes.type == .dir else { throw POSIXError(.EIO) }
            _ = try self.retain(root, parent: 1, grantsReference: false)
            self.locked { self.sessionReadOnly = policy; self.isMounted = true }
        }
    }

    /// Apple mount(8) forwards mount options as ordered `-o`, value pairs.
    /// Each value can contain comma-separated options; ro/rdonly takes priority
    /// within that value, and a later value can override an earlier one.
    private static func readOnlyOption(_ options: [String]) throws -> Bool? {
        var selected: Bool?
        var index = 0
        while index < options.count {
            guard options[index] != "--" else { break }
            guard options[index] == "-o" else { index += 1; continue }
            guard index + 1 < options.count else { throw POSIXError(.EINVAL) }
            let values = Set(options[index + 1].split(separator: ",").map(String.init))
            if values.contains("ro") || values.contains("rdonly") { selected = true }
            else if values.contains("rw") { selected = false }
            index += 2
        }
        return selected
    }

    /// The exclusive operation permit holds this proposal stable until a
    /// successful activation/mount publishes it. Retained descriptors and an
    /// attached mount forbid changing effective access, even after deactivation.
    private func nextSessionReadOnly(_ options: [String]) throws -> Bool {
        let requested = try Self.readOnlyOption(options)
        return try locked {
            let selected = requested ?? sessionReadOnly
            let changesAccess = (loadReadOnly || selected) != (loadReadOnly || sessionReadOnly)
            let hasHandles = items.values.contains { !$0.handles.isEmpty } || !directories.isEmpty ||
                !pendingHandles.isEmpty || pendingOpenCount > 0
            guard !changesAccess || (!isMounted && !hasHandles) else { throw POSIXError(.EBUSY) }
            return selected
        }
    }

    func unmount(replyHandler: @escaping () -> Void) {
        Task { await unmountSession(); replyHandler() }
    }

    func deactivate(options: FSDeactivateOptions, replyHandler: @escaping (Error?) -> Void) {
        // FSKit issues sync/unmount before deactivation. No resource I/O belongs here.
        locked { isActive = false; isMounted = false }
        replyHandler(nil)
    }

    func synchronize(flags: FSSyncFlags, replyHandler: @escaping (Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            // Synchronization proves pending writes reached storage. Advisory
            // statistics must not prevent a no-write volume from unmounting.
            try await self.synchronizeHandles()
        }
    }

    /// Called by resource unload. Security scope may only
    /// be released after this finishes and all request sockets have drained.
    func shutdown() async {
        let task = locked { () -> Task<Void, Never> in
            if let shutdownTask { return shutdownTask }
            isClosing = true
            sessionEpoch &+= 1
            let reads = running.values.filter(\.cancelOnDrain).map(\.task)
            reads.forEach { $0.cancel() }
            let unmount = unmountTask
            let task = Task {
                if let unmount { await unmount.value }
                await self.finishShutdown()
            }
            shutdownTask = task
            return task
        }
        await task.value
    }

    private func finishShutdown() async {
        let permit = await gate.drain()
        await clearSession(preservingRoot: false)
        locked { isActive = false; isMounted = false }
        await client.close()
        await gate.release(permit)
    }

    private func unmountSession() async {
        let task = locked { () -> Task<Void, Never> in
            if let shutdownTask { return shutdownTask }
            if let unmountTask { return unmountTask }
            isClosing = true
            sessionEpoch &+= 1
            running.values.filter(\.cancelOnDrain).forEach { $0.task.cancel() }
            let task = Task { await self.finishUnmount() }
            unmountTask = task
            return task
        }
        await task.value
    }

    private func finishUnmount() async {
        let permit = await gate.drain()
        await clearSession(preservingRoot: true)
        locked { isMounted = false; needsReconnection = true }
        await gate.release(permit)
        await gate.reopen()
        locked {
            if shutdownTask == nil { isClosing = false }
            unmountTask = nil
        }
    }

    private func ensureConnection() async throws {
        guard locked({ needsReconnection }) else { return }
        let fresh = try client.reconnected()
        await client.close()
        locked { client = fresh; needsReconnection = false }
    }

    private func clearSession(preservingRoot: Bool) async {
        let deadline = DispatchTime.now().uptimeNanoseconds + 30_000_000_000
        let states = locked { Array(items.values) }
        let sessions = locked { Array(directories.values) }
        let transient = locked { transientReferences }
        let pending = locked { pendingHandles }
        for state in states {
            for handle in state.handles {
                if !readOnly && state.writeHandle == handle {
                    await cleanup(FSBridgeRequest(op: "fsync", inode: state.item.inode, handle: handle), deadline: deadline)
                }
                await cleanup(FSBridgeRequest(op: "release", inode: state.item.inode, handle: handle), deadline: deadline)
            }
        }
        let activeHandles = Set(states.flatMap(\.handles))
        for (handle, inode) in pending where !activeHandles.contains(handle) {
            await cleanup(FSBridgeRequest(op: "release", inode: inode, handle: handle), deadline: deadline)
        }
        for session in sessions {
            await cleanup(FSBridgeRequest(op: "releasedir", inode: session.inode, handle: session.handle), deadline: deadline)
        }
        for state in states where state.lookupReferences > 0 {
            await cleanup(FSBridgeRequest(op: "forget", inode: state.item.inode, n: state.lookupReferences), deadline: deadline)
        }
        for (inode, count) in transient {
            await cleanup(FSBridgeRequest(op: "forget", inode: inode, n: count), deadline: deadline)
        }
        if DispatchTime.now().uptimeNanoseconds >= deadline {
            logger.error("Filesystem cleanup deadline reached; the service will drain remaining handles when detached")
        }
        locked {
            var root = preservingRoot ? items[1] : nil
            root?.lookupReferences = 0
            root?.readHandle = nil
            root?.writeHandle = nil
            root?.openModes = []
            root?.removed = false
            root?.reclaimed = false
            items.removeAll()
            if let root { items[1] = root }
            directories.removeAll(); transientReferences.removeAll(); pendingHandles.removeAll()
            pendingOpenCount = 0
        }
    }

    func getAttributes(_ desiredAttributes: FSItem.GetAttributesRequest, of item: FSItem,
                       replyHandler: @escaping (FSItem.Attributes?, Error?) -> Void) {
        let requireSize = desiredAttributes.isAttributeWanted(.size)
        perform(exclusive: false, reply: replyHandler) {
            let state = try self.state(for: item)
            let attributes: FSBridgeAttributes
            if state.removed && state.handle == nil {
                attributes = state.attributes
            } else {
                let node = try self.requireNode(try await self.client.request(FSBridgeRequest(op: "getattr", inode: state.item.inode,
                    handle: state.handle, requireSize: requireSize ? true : nil)))
                try self.validateIdentity(node, item: state.item)
                attributes = node.attributes
            }
            if requireSize && attributes.sizeKnown == false { throw FSBridgeError.malformedResponse }
            self.updateAttributes(attributes, inode: state.item.inode)
            return try self.makeAttributes(attributes, inode: state.item.inode, parent: state.parent,
                                           desired: desiredAttributes, removed: state.removed)
        }
    }

    func getXattr(named name: FSFileName, of item: FSItem,
                  replyHandler: @escaping (Data?, Error?) -> Void) {
        perform(exclusive: false, reply: replyHandler) {
            let state = try self.state(for: item)
            let name = try self.xattrName(name)
            return try await self.client.getXattr(inode: state.item.inode, name: name)
        }
    }

    func listXattrs(of item: FSItem, replyHandler: @escaping ([FSFileName]?, Error?) -> Void) {
        perform(exclusive: false, reply: replyHandler) {
            let state = try self.state(for: item)
            let names = try await self.client.listXattrs(inode: state.item.inode)
            return names.map { FSFileName(string: $0) }
        }
    }

    func setXattr(named name: FSFileName, to value: Data?, on item: FSItem,
                  policy: FSVolume.SetXattrPolicy, replyHandler: @escaping (Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            try self.requireWritable()
            let state = try self.state(for: item)
            let name = try self.xattrName(name)
            let bridgePolicy: FSBridgeXattrPolicy
            switch policy {
            case .alwaysSet: bridgePolicy = .alwaysSet
            case .mustCreate: bridgePolicy = .mustCreate
            case .mustReplace: bridgePolicy = .mustReplace
            case .delete:
                // Delete has no value, even if FSKit supplies an unused one.
                try await self.client.removeXattr(inode: state.item.inode, name: name)
                return
            @unknown default: throw POSIXError(.EINVAL)
            }
            guard let value else { throw POSIXError(.EINVAL) }
            guard value.count <= self.maximumXattrSize else { throw POSIXError(.E2BIG) }
            try await self.client.setXattr(inode: state.item.inode, name: name, value: value, policy: bridgePolicy)
        }
    }

    func setAttributes(_ newAttributes: FSItem.SetAttributesRequest, on item: FSItem,
                       replyHandler: @escaping (FSItem.Attributes?, Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            try self.requireWritable()
            let state = try self.state(for: item)
            let changes = try self.attributeChanges(newAttributes, type: state.attributes.type)
            let node: FSBridgeNode
            if changes.size != nil || changes.mode != nil || changes.mtimeNS != nil {
                var temporary: UInt64?
                if changes.size != nil, state.writeHandle == nil {
                    guard !state.removed else { throw POSIXError(.EBADF) }
                    temporary = try await self.openHandle(inode: state.item.inode, access: 2)
                }
                do {
                    let handle = changes.size != nil ? (state.writeHandle ?? temporary) : state.handle
                    node = try self.requireNode(try await self.client.request(FSBridgeRequest(op: "setattr", inode: state.item.inode,
                        handle: handle, attributes: changes)))
                } catch {
                    if let temporary { await self.cleanupPendingHandle(inode: state.item.inode, handle: temporary) }
                    throw error
                }
                if let temporary { await self.cleanupPendingHandle(inode: state.item.inode, handle: temporary) }
                try self.validateIdentity(node, item: state.item)
                self.updateAttributes(node.attributes, inode: state.item.inode)
                self.consume(newAttributes, changes: changes)
            } else {
                node = FSBridgeNode(inode: state.item.inode, generation: state.item.generation, attributes: state.attributes)
            }
            return try self.makeAttributes(node.attributes, inode: node.inode, parent: state.parent,
                                           desired: nil, removed: state.removed)
        }
    }

    func lookupItem(named name: FSFileName, inDirectory directory: FSItem,
                    replyHandler: @escaping (FSItem?, FSFileName?, Error?) -> Void) {
        // Lookup can run alongside other reads, but it acquires a reference.
        // Drain must await its response so that reference is recorded first.
        perform(exclusive: false, cancelOnDrain: false, reply: replyHandler) {
            let parent = try self.directoryState(directory)
            let component = try self.component(name, allowDots: true)
            if component == "." {
                self.locked { self.items[parent.item.inode]?.reclaimed = false }
                return (parent.item, name)
            }
            if component == ".." {
                guard let item = self.locked({ self.items[parent.parent]?.item }) else { throw POSIXError(.ESTALE) }
                self.locked { self.items[item.inode]?.reclaimed = false }
                return (item, name)
            }
            let node = try self.requireNode(try await self.client.request(FSBridgeRequest(op: "lookup", parent: parent.item.inode, name: component)))
            self.noteTransientReference(node.inode)
            do {
                let item = try self.retain(node, parent: parent.item.inode, grantsReference: true)
                self.consumeTransientReference(node.inode)
                return (item, FSFileName(string: component))
            }
            catch {
                try? await self.forgetTransientReference(node.inode)
                throw error
            }
        }
    }

    func reclaimItem(_ item: FSItem, replyHandler: @escaping (Error?) -> Void) {
        if locked({ isClosing }) { replyHandler(nil); return }
        perform(exclusive: true, reply: replyHandler) {
            // Unmount already balanced references from the previous session.
            // A stale inode number may now belong to a new session's item.
            guard let state = try? self.state(for: item) else { return }
            var failure: Error?
            for handle in state.handles {
                do { try await self.flushAndRelease(inode: state.item.inode, handle: handle) }
                catch { failure = error }
            }
            do { try await self.releaseDirectories(for: state.item.inode) }
            catch { if failure == nil { failure = error } }
            self.locked { self.items[state.item.inode]?.reclaimed = true }
            do { try await self.reclaimIfUnreferenced(state.item.inode) }
            catch { if failure == nil { failure = error } }
            if let failure { throw failure }
        }
    }

    func readSymbolicLink(_ item: FSItem, replyHandler: @escaping (FSFileName?, Error?) -> Void) {
        perform(exclusive: false, reply: replyHandler) {
            let state = try self.state(for: item)
            guard state.attributes.type == .symlink else { throw POSIXError(.EINVAL) }
            let response = try await self.client.request(FSBridgeRequest(op: "readlink", inode: state.item.inode))
            guard let target = response.target, !target.utf8.contains(0) else { throw POSIXError(.EIO) }
            return FSFileName(string: target)
        }
    }

    func createItem(named name: FSFileName, type: FSItem.ItemType, inDirectory directory: FSItem,
                    attributes newAttributes: FSItem.SetAttributesRequest,
                    replyHandler: @escaping (FSItem?, FSFileName?, Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            try self.requireWritable()
            guard type == .file || type == .directory else { throw POSIXError(.ENOTSUP) }
            let parent = try self.directoryState(directory)
            let component = try self.component(name)
            let fileType: FSBridgeAttributes.FileType = type == .file ? .file : .dir
            let changes = try self.attributeChanges(newAttributes, type: fileType)
            let mode = changes.mode ?? (type == .directory ? 0o755 : 0o644)
            let response = try await self.client.request(FSBridgeRequest(op: type == .directory ? "mkdir" : "create",
                parent: parent.item.inode, name: component, mode: mode, access: type == .file ? 3 : nil))
            let node = try self.requireNode(response)
            let item = try await self.retainCreated(node, parent: parent.item.inode, handle: response.handle)
            do {
                try await self.applyCreationAttributes(newAttributes, changes: changes, item: item, initialMode: mode)
            } catch {
                await self.abandonCreation(item: item)
                throw error
            }
            return (item, FSFileName(string: component))
        }
    }

    func createSymbolicLink(named name: FSFileName, inDirectory directory: FSItem,
                            attributes newAttributes: FSItem.SetAttributesRequest, linkContents contents: FSFileName,
                            replyHandler: @escaping (FSItem?, FSFileName?, Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            try self.requireWritable()
            let parent = try self.directoryState(directory)
            let component = try self.component(name)
            guard let target = contents.string, !target.isEmpty, !contents.data.contains(0), contents.data.count <= 4096 else {
                throw POSIXError(.EINVAL)
            }
            let changes = try self.attributeChanges(newAttributes, type: .symlink)
            let response = try await self.client.request(FSBridgeRequest(op: "symlink", parent: parent.item.inode,
                name: component, target: target))
            let item = try await self.retainCreated(try self.requireNode(response), parent: parent.item.inode, handle: nil)
            do { try await self.applyCreationAttributes(newAttributes, changes: changes, item: item, initialMode: nil) }
            catch {
                await self.abandonCreation(item: item)
                throw error
            }
            return (item, FSFileName(string: component))
        }
    }

    func createLink(to item: FSItem, named name: FSFileName, inDirectory directory: FSItem,
                    replyHandler: @escaping (FSFileName?, Error?) -> Void) {
        replyHandler(nil, POSIXError(readOnly ? .EROFS : .ENOTSUP))
    }

    func removeItem(_ item: FSItem, named name: FSFileName, fromDirectory directory: FSItem,
                    replyHandler: @escaping (Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            try self.requireWritable()
            let state = try self.state(for: item)
            let parent = try self.directoryState(directory)
            let component = try self.component(name)
            _ = try await self.client.request(FSBridgeRequest(op: state.attributes.type == .dir ? "rmdir" : "unlink",
                parent: parent.item.inode, name: component))
            self.locked { self.items[state.item.inode]?.removed = true }
        }
    }

    func renameItem(_ item: FSItem, inDirectory sourceDirectory: FSItem, named sourceName: FSFileName,
                    to destinationName: FSFileName, inDirectory destinationDirectory: FSItem, overItem: FSItem?,
                    replyHandler: @escaping (FSFileName?, Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            try self.requireWritable()
            let source = try self.state(for: item)
            let oldParent = try self.directoryState(sourceDirectory)
            let newParent = try self.directoryState(destinationDirectory)
            let oldName = try self.component(sourceName)
            let newName = try self.component(destinationName)
            let overwritten = try overItem.map { try self.state(for: $0) }
            _ = try await self.client.request(FSBridgeRequest(op: "rename", oldParent: oldParent.item.inode,
                oldName: oldName, newParent: newParent.item.inode, newName: newName))
            self.locked {
                self.items[source.item.inode]?.parent = newParent.item.inode
                if let overwritten, overwritten.item !== source.item { self.items[overwritten.item.inode]?.removed = true }
            }
            do { try await self.reclaimIfUnreferenced(oldParent.item.inode) }
            catch { self.logger.error("Deferred inode cleanup failed with error \(self.posix(error).code, privacy: .public)") }
            return FSFileName(string: newName)
        }
    }

    func enumerateDirectory(_ directory: FSItem, startingAt cookie: FSDirectoryCookie,
                            verifier: FSDirectoryVerifier, attributes: FSItem.GetAttributesRequest?,
                            packer: FSDirectoryEntryPacker,
                            replyHandler: @escaping (FSDirectoryVerifier, Error?) -> Void) {
        enumerateDirectory(directory, startingAt: cookie, verifier: verifier, attributes: attributes,
            packEntry: { name, type, id, nextCookie, attributes in
                packer.packEntry(name: name, itemType: type, itemID: id, nextCookie: nextCookie, attributes: attributes)
            }, replyHandler: replyHandler)
    }

    /// FSKit owns its packer constructors. This shared path lets host-side tests
    /// exercise buffer-full replies and cookie continuation with real items.
    func enumerateDirectory(_ directory: FSItem, startingAt cookie: FSDirectoryCookie,
                            verifier: FSDirectoryVerifier, attributes: FSItem.GetAttributesRequest?,
                            packEntry: @escaping (FSFileName, FSItem.ItemType, FSItem.Identifier,
                                                  FSDirectoryCookie, FSItem.Attributes?) -> Bool,
                            replyHandler: @escaping (FSDirectoryVerifier, Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            let state = try self.directoryState(directory)
            let sessionID: UInt64
            let session: DirectorySession
            if cookie.rawValue == 0 && verifier.rawValue == 0 {
                try await self.evictOldDirectorySessions()
                let response = try await self.client.request(FSBridgeRequest(op: "opendir", inode: state.item.inode))
                guard let handle = response.handle, handle > 0 else { throw POSIXError(.EIO) }
                session = DirectorySession(inode: state.item.inode, handle: handle, includesDotEntries: attributes == nil,
                    lastUsed: Date(), eofOffset: nil, page: nil)
                sessionID = self.locked {
                    repeat {
                        self.verifierCounter = self.verifierCounter == UInt64.max ? 1 : self.verifierCounter + 1
                    } while self.directories[self.verifierCounter] != nil
                    self.directories[self.verifierCounter] = session
                    return self.verifierCounter
                }
            } else {
                guard verifier.rawValue != 0,
                      let saved = self.locked({ self.directories[verifier.rawValue] }),
                      saved.inode == state.item.inode, saved.includesDotEntries == (attributes == nil) else {
                    throw FSError(.invalidDirectoryCookie)
                }
                sessionID = verifier.rawValue
                session = saved
                self.locked { self.directories[sessionID]?.lastUsed = Date() }
            }
            var position = cookie.rawValue
            var packedCount = 0
            if session.includesDotEntries && position < 2 {
                if position == 0 {
                    guard packEntry(FSFileName(string: "."), .directory,
                        try self.identifier(state.item.inode), FSDirectoryCookie(1), nil) else {
                        return FSDirectoryVerifier(sessionID)
                    }
                    position = 1
                    packedCount += 1
                }
                guard packEntry(FSFileName(string: ".."), .directory,
                    try self.identifier(state.parent), FSDirectoryCookie(2), nil) else {
                    return FSDirectoryVerifier(sessionID)
                }
                position = 2
                packedCount += 1
            }
            let bias: UInt64 = session.includesDotEntries ? 2 : 0
            guard position >= bias else { throw FSError(.invalidDirectoryCookie) }
            var offset = position - bias
            if let eofOffset = session.eofOffset, offset == eofOffset, packedCount == 0 {
                _ = try await self.client.request(FSBridgeRequest(op: "releasedir", inode: session.inode, handle: session.handle))
                self.locked { _ = self.directories.removeValue(forKey: sessionID) }
                return FSDirectoryVerifier(sessionID)
            }
            while true {
                let page = try await self.directoryPage(sessionID: sessionID, session: session, offset: offset, bias: bias)
                for entry in page.entries where entry.offset > offset {
                    let name = try self.component(FSFileName(string: entry.name))
                    let packedAttributes = try attributes.map {
                        try self.makeAttributes(entry.node.attributes, inode: entry.node.inode, parent: session.inode, desired: $0, removed: false)
                    }
                    if !packEntry(FSFileName(string: name), self.itemType(entry.node.attributes.type),
                        try self.identifier(entry.node.inode), FSDirectoryCookie(entry.offset + bias), packedAttributes) {
                        // Keep this bounded page for continuation or replay. Its
                        // directory handle retains IDs after the batch forget.
                        return FSDirectoryVerifier(sessionID)
                    }
                    offset = entry.offset
                    packedCount += 1
                }
                self.locked { self.directories[sessionID]?.page = nil }
                if page.eof {
                    self.locked { self.directories[sessionID]?.eofOffset = offset }
                    if packedCount == 0 {
                        _ = try await self.client.request(FSBridgeRequest(op: "releasedir", inode: session.inode, handle: session.handle))
                        self.locked { _ = self.directories.removeValue(forKey: sessionID) }
                    }
                    return FSDirectoryVerifier(sessionID)
                }
                // An entry removed after opendir can leave an empty page with a
                // valid advancing cookie; it is not necessarily end-of-directory.
                guard page.nextOffset >= offset, page.nextOffset > position - bias else { throw POSIXError(.EIO) }
                offset = page.nextOffset
                position = offset + bias
                try Task.checkCancellation()
            }
        }
    }

    func openItem(_ item: FSItem, modes: FSVolume.OpenModes, replyHandler: @escaping (Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            if modes.contains(.write) {
                if self.readOnly { self.logger.notice("Write open refused by read-only volume policy") }
                try self.requireWritable()
            }
            let state = try self.state(for: item)
            guard state.attributes.type == .file else {
                if state.attributes.type == .dir { return }
                throw POSIXError(.EINVAL)
            }
            var read = state.readHandle
            var write = state.writeHandle
            var opened: [UInt64] = []
            do {
                if modes.contains(.read), read == nil {
                    read = try await self.openHandle(inode: state.item.inode, access: 1)
                    opened.append(read!)
                }
                if modes.contains(.write), write == nil {
                    write = try await self.openHandle(inode: state.item.inode, access: 2)
                    opened.append(write!)
                }
            } catch {
                for handle in opened { await self.cleanupPendingHandle(inode: state.item.inode, handle: handle) }
                throw error
            }
            self.locked {
                self.items[state.item.inode]?.readHandle = read
                self.items[state.item.inode]?.writeHandle = write
                self.items[state.item.inode]?.openModes.formUnion(modes)
                for handle in opened { self.pendingHandles.removeValue(forKey: handle) }
            }
        }
    }

    func closeItem(_ item: FSItem, modes: FSVolume.OpenModes, replyHandler: @escaping (Error?) -> Void) {
        perform(exclusive: true, reply: replyHandler) {
            let state = try self.state(for: item)
            guard state.attributes.type == .file else { return }
            guard !modes.contains(.read) || state.readHandle != nil,
                  !modes.contains(.write) || state.writeHandle != nil else { throw POSIXError(.EBADF) }
            let kept = Set([modes.contains(.read) ? state.readHandle : nil,
                            modes.contains(.write) ? state.writeHandle : nil].compactMap { $0 })
            var failure: Error?
            if !modes.contains(.write), let writer = state.writeHandle, kept.contains(writer), !self.readOnly {
                // A creation handle can provide both capabilities. Losing write
                // access still needs a flush even when its read descriptor stays.
                do { _ = try await self.client.request(FSBridgeRequest(op: "flush", inode: state.item.inode, handle: writer)) }
                catch { failure = error }
            }
            for handle in state.handles where !kept.contains(handle) {
                do { try await self.flushAndRelease(inode: state.item.inode, handle: handle) }
                catch { if failure == nil { failure = error } }
            }
            self.locked {
                if !modes.contains(.read) { self.items[state.item.inode]?.readHandle = nil }
                if !modes.contains(.write) { self.items[state.item.inode]?.writeHandle = nil }
                self.items[state.item.inode]?.openModes = modes
            }
            if let failure { throw failure }
        }
    }

    func read(from item: FSItem, at offset: off_t, length: Int, into buffer: FSMutableFileDataBuffer,
              replyHandler: @escaping (Int, Error?) -> Void) {
        let destination = VolumeReadBuffer(buffer)
        read(from: item, at: offset, length: length, capacity: destination.capacity, consume: { data, position in
            destination.copy(data, at: position)
        }, replyHandler: replyHandler)
    }

    /// FSKit owns its mutable buffer constructors. Sharing the actual read path
    /// with a chunk consumer also permits host-side validation with real items.
    func read(from item: FSItem, at offset: off_t, length: Int, capacity: Int,
              consume: @escaping @Sendable (Data, Int) -> Void, replyHandler: @escaping (Int, Error?) -> Void) {
        perform(exclusive: false, reply: { (result: IOResult?, error: Error?) in
            replyHandler(result?.count ?? 0, error ?? result?.error)
        }) {
            let state = try self.state(for: item)
            guard state.attributes.type == .file else { throw POSIXError(.EBADF) }
            guard offset >= 0, length >= 0, length <= Int.max - Int(offset) else { throw POSIXError(.EINVAL) }
            guard length <= capacity else { throw POSIXError(.EINVAL) }
            if length == 0 { return IOResult(count: 0, error: nil) }
            let handle: UInt64
            let temporary: Bool
            if let reader = state.readHandle {
                handle = reader
                temporary = false
            } else {
                // Executable-header inspection can issue a vnode read before
                // any open callback. As in Apple's passthrough sample, acquire
                // a read descriptor for this operation without changing the
                // item's retained descriptors or the kernel's open modes.
                guard !state.removed else { throw POSIXError(.EBADF) }
                handle = try await self.openTemporaryReadHandle(inode: state.item.inode)
                temporary = true
            }
            var count = 0
            var failure: Error?
            while count < length {
                do {
                    try Task.checkCancellation()
                    let size = min(1_048_576, length - count)
                    let data = try await self.client.read(inode: state.item.inode, handle: handle,
                        offset: UInt64(offset) + UInt64(count), size: size)
                    guard data.count <= size else { throw POSIXError(.EIO) }
                    consume(data, count)
                    count += data.count
                    if data.count < size { break }
                } catch { failure = self.posix(error); break }
            }
            if temporary, let cleanupError = await self.releaseTemporaryReadHandle(inode: state.item.inode, handle: handle), failure == nil {
                failure = cleanupError
            }
            return IOResult(count: count, error: failure)
        }
    }

    func write(contents: Data, to item: FSItem, at offset: off_t,
               replyHandler: @escaping (Int, Error?) -> Void) {
        perform(exclusive: true, reply: { (result: IOResult?, error: Error?) in
            replyHandler(result?.count ?? 0, error ?? result?.error)
        }) {
            try self.requireWritable()
            let state = try self.state(for: item)
            guard state.attributes.type == .file, let handle = state.writeHandle,
                  state.openModes.contains(.write) else { throw POSIXError(.EBADF) }
            guard offset >= 0, contents.count <= Int.max - Int(offset) else { throw POSIXError(.EINVAL) }
            var count = 0
            while count < contents.count {
                do {
                    let size = min(1_048_576, contents.count - count)
                    let data = contents.subdata(in: count..<(count + size))
                    let written = try await self.client.write(inode: state.item.inode, handle: handle,
                        offset: UInt64(offset) + UInt64(count), data: data)
                    guard written > 0, written <= size else { throw POSIXError(.EIO) }
                    count += written
                    self.recordWrite(inode: state.item.inode, end: UInt64(offset) + UInt64(count))
                } catch { return IOResult(count: count, error: self.posix(error)) }
            }
            return IOResult(count: count, error: nil)
        }
    }

    private func synchronizeHandles() async throws {
        guard !readOnly else { return }
        let states = locked { Array(items.values) }
        for state in states {
            if let handle = state.writeHandle {
                _ = try await client.request(FSBridgeRequest(op: "fsync", inode: state.item.inode, handle: handle))
            }
        }
    }

    private func flushAndRelease(inode: UInt64, handle: UInt64) async throws {
        var failure: Error?
        if !readOnly && locked({ items[inode]?.writeHandle == handle }) {
            do { _ = try await client.request(FSBridgeRequest(op: "flush", inode: inode, handle: handle)) }
            catch { failure = error }
        }
        do {
            _ = try await client.request(FSBridgeRequest(op: "release", inode: inode, handle: handle))
            locked {
                if items[inode]?.readHandle == handle {
                    items[inode]?.readHandle = nil
                    items[inode]?.openModes.remove(.read)
                }
                if items[inode]?.writeHandle == handle {
                    items[inode]?.writeHandle = nil
                    items[inode]?.openModes.remove(.write)
                }
            }
        }
        catch {
            locked { pendingHandles[handle] = inode }
            if failure == nil { failure = error }
        }
        if let failure { throw failure }
    }

    private func releaseDirectories(for inode: UInt64) async throws {
        let sessions = locked { directories.filter { $0.value.inode == inode } }
        for (id, session) in sessions {
            _ = try await client.request(FSBridgeRequest(op: "releasedir", inode: session.inode, handle: session.handle))
            locked { _ = directories.removeValue(forKey: id) }
        }
    }

    private func openHandle(inode: UInt64, access: UInt32) async throws -> UInt64 {
        try reservePendingOpen()
        do {
            let response = try await client.request(FSBridgeRequest(op: "open", inode: inode, access: access))
            guard let handle = response.handle, handle > 0 else { throw POSIXError(.EIO) }
            recordPendingOpen(handle: handle, inode: inode)
            return handle
        } catch {
            locked { pendingOpenCount -= 1 }
            throw error
        }
    }

    private func openTemporaryReadHandle(inode: UInt64) async throws -> UInt64 {
        try Task.checkCancellation()
        try reservePendingOpen()
        let connection = client
        let opened = Task.detached {
            // An OPEN response conveys ownership of a newly allocated handle.
            // Caller cancellation must not discard that identity. The admitted
            // read keeps its permit while this bounded operation completes.
            try await connection.request(FSBridgeRequest(op: "open", inode: inode, access: 1), timeout: 120)
        }
        do {
            let response = try await opened.value
            guard let handle = response.handle, handle > 0 else { throw POSIXError(.EIO) }
            recordPendingOpen(handle: handle, inode: inode)
            return handle
        } catch {
            locked { pendingOpenCount -= 1 }
            throw error
        }
    }

    private func reservePendingOpen() throws {
        try locked {
            guard pendingHandles.count + pendingOpenCount < 256 else { throw POSIXError(.EAGAIN) }
            pendingOpenCount += 1
        }
    }

    private func recordPendingOpen(handle: UInt64, inode: UInt64) {
        locked {
            pendingOpenCount -= 1
            pendingHandles[handle] = inode
        }
    }

    private func cleanupPendingHandle(inode: UInt64, handle: UInt64) async {
        do {
            _ = try await client.request(FSBridgeRequest(op: "release", inode: inode, handle: handle), timeout: 10)
            locked { _ = pendingHandles.removeValue(forKey: handle) }
        } catch { logger.error("Pending handle cleanup failed with error \(self.posix(error).code, privacy: .public)") }
    }

    private func releaseTemporaryReadHandle(inode: UInt64, handle: UInt64) async -> Error? {
        // A cancelled read still owns its descriptor. Run its bounded release
        // independently, and await it before giving up the operation permit.
        let connection = client
        let release = Task.detached {
            try await connection.request(FSBridgeRequest(op: "release", inode: inode, handle: handle), timeout: 10)
        }
        do {
            _ = try await release.value
            locked { _ = pendingHandles.removeValue(forKey: handle) }
            return nil
        } catch {
            // Preserve the pending entry so session drain retries failed cleanup.
            logger.error("Temporary read handle cleanup failed with error \(self.posix(error).code, privacy: .public)")
            return posix(error)
        }
    }

    /// Keep a reclaimed parent inode alive while a child still needs its `..`
    /// identity. FSKit can reclaim a parent vnode before its descendants.
    private func reclaimIfUnreferenced(_ inode: UInt64) async throws {
        guard let state = locked({ items[inode] }), state.reclaimed, state.handle == nil,
              locked({ !items.values.contains { $0.item.inode != inode && $0.parent == inode } }),
              locked({ !directories.values.contains { $0.inode == inode } }) else { return }
        if state.lookupReferences > 0 {
            _ = try await client.request(FSBridgeRequest(op: "forget", inode: inode, n: state.lookupReferences))
        }
        locked { _ = items.removeValue(forKey: inode) }
        if state.parent != inode { try await reclaimIfUnreferenced(state.parent) }
    }

    private func abandonCreation(item: RepoReachItem) async {
        guard let state = try? state(for: item) else { return }
        // A failed follow-up setattr does not give us authority to unlink a name
        // that another process may already have replaced. Release our references;
        // the partially created item remains discoverable by a subsequent lookup.
        for handle in state.handles {
            do { try await flushAndRelease(inode: item.inode, handle: handle) }
            catch { logger.error("Creation handle cleanup failed with error \(self.posix(error).code, privacy: .public)") }
        }
        locked { items[item.inode]?.reclaimed = true }
        do { try await reclaimIfUnreferenced(item.inode) }
        catch { logger.error("Creation reference cleanup failed with error \(self.posix(error).code, privacy: .public)") }
    }

    private func evictOldDirectorySessions() async throws {
        let stale = locked {
            directories.filter {
                Date().timeIntervalSince($0.value.lastUsed) > 60 &&
                    $0.value.page?.cleanupFailed != true && $0.value.page?.pendingForgets.isEmpty != false
            }
        }
        for (id, session) in stale {
            try await retireDirectorySession(id, session: session)
        }
        // FSKit need not ask for an empty page after a reply packed the final
        // entries. Keep these sessions for cookie replay while space permits,
        // then retire the oldest completed snapshot before opening another one.
        // Incomplete pages and uncertain reference cleanup retain their owners.
        if locked({ directories.count >= 128 }),
           let completed = locked({
               directories.filter { $0.value.eofOffset != nil && $0.value.page == nil }.min {
                   $0.value.lastUsed == $1.value.lastUsed ? $0.key < $1.key : $0.value.lastUsed < $1.value.lastUsed
               }
           }) {
            try await retireDirectorySession(completed.key, session: completed.value)
        }
        guard locked({ directories.count < 128 }) else { throw POSIXError(.EMFILE) }
    }

    private func retireDirectorySession(_ id: UInt64, session: DirectorySession) async throws {
        do {
            _ = try await client.request(FSBridgeRequest(op: "releasedir", inode: session.inode, handle: session.handle))
        } catch FSBridgeError.filesystem(let code) where code == EBADF {
            // A lost successful reply can leave local ownership after the
            // broker removed this handle. Only its authenticated filesystem
            // error establishes that fact; transport errors remain retryable.
        }
        // A retired verifier becomes invalid; never replay IDs after their
        // retaining handle is released. Other failed releases remain owned.
        locked { _ = directories.removeValue(forKey: id) }
    }

    private func directoryPage(sessionID: UInt64, session: DirectorySession,
                               offset: UInt64, bias: UInt64) async throws -> DirectoryPage {
        if let cached = locked({ directories[sessionID]?.page }) {
            try await forgetDirectoryPageReferences(sessionID: sessionID)
            if (offset != cached.nextOffset || cached.eof),
               offset == cached.startOffset || cached.entries.contains(where: { $0.offset == offset }) {
                try validateDirectoryPage(cached, bias: bias)
                return try currentDirectoryPage(sessionID)
            }
            locked { directories[sessionID]?.page = nil }
        }
        let response: FSBridgeResponse
        do {
            response = try await client.request(FSBridgeRequest(op: "readdir", inode: session.inode,
                handle: session.handle, offset: offset))
        } catch {
            if posix(error).code == Int(EINVAL) { throw FSError(.invalidDirectoryCookie) }
            throw error
        }
        let entries = response.entries ?? []
        // A 64 KiB page of the engine's aligned dirents has at most 2048 entries.
        // Keep at most one such page in each of the 128 bounded sessions.
        guard entries.count <= 2_048 else { throw POSIXError(.EIO) }
        var counts: [UInt64: UInt64] = [:]
        for entry in entries {
            noteTransientReference(entry.node.inode)
            counts[entry.node.inode, default: 0] += 1
        }
        let page = DirectoryPage(startOffset: offset, entries: entries,
            nextOffset: response.nextOffset ?? 0, eof: response.eof ?? false,
            pendingForgets: counts.keys.sorted().map { FSBridgeForget(inode: $0, n: counts[$0]!) })
        // Persist ownership before the cancellable cleanup request. An error
        // leaves unacknowledged counts available to session drain.
        locked { directories[sessionID]?.page = page }
        try await forgetDirectoryPageReferences(sessionID: sessionID)
        try validateDirectoryPage(page, bias: bias)
        return try currentDirectoryPage(sessionID)
    }

    private func currentDirectoryPage(_ sessionID: UInt64) throws -> DirectoryPage {
        try locked {
            guard let page = directories[sessionID]?.page else { throw POSIXError(.EIO) }
            return page
        }
    }

    private func validateDirectoryPage(_ page: DirectoryPage, bias: UInt64) throws {
        var previous = page.startOffset
        for entry in page.entries {
            guard entry.offset > previous, entry.offset <= UInt64.max - bias else { throw POSIXError(.EIO) }
            previous = entry.offset
        }
        guard page.nextOffset >= previous, page.nextOffset <= UInt64.max - bias else { throw POSIXError(.EIO) }
    }

    private func forgetDirectoryPageReferences(sessionID: UInt64) async throws {
        while let page = locked({ directories[sessionID]?.page }) {
            // A failed response may have followed a successful server mutation.
            // Never replay it against references acquired by a later lookup.
            guard !page.cleanupFailed else { throw POSIXError(.EIO) }
            let pending = page.pendingForgets
            if pending.isEmpty { return }
            let batch = Array(pending.prefix(FSBridgeForget.maximumBatchCount))
            do {
                _ = try await client.request(FSBridgeRequest(op: "batchforget", forgets: batch), timeout: 10)
            } catch {
                locked { directories[sessionID]?.page?.cleanupFailed = true }
                throw error
            }
            locked {
                for pair in batch { consumeTransientReferences(pair.inode, count: pair.n) }
                directories[sessionID]?.page?.pendingForgets.removeFirst(batch.count)
            }
        }
    }

    private func retainCreated(_ node: FSBridgeNode, parent: UInt64, handle: UInt64?) async throws -> RepoReachItem {
        noteTransientReference(node.inode)
        if let handle { locked { pendingHandles[handle] = node.inode } }
        let item: RepoReachItem
        do {
            item = try retain(node, parent: parent, grantsReference: true)
            consumeTransientReference(node.inode)
        }
        catch {
            if let handle { await cleanupPendingHandle(inode: node.inode, handle: handle) }
            try? await forgetTransientReference(node.inode)
            throw error
        }
        locked {
            items[node.inode]?.readHandle = handle
            items[node.inode]?.writeHandle = handle
            if handle != nil { items[node.inode]?.openModes = [.read, .write] }
            if let handle { pendingHandles.removeValue(forKey: handle) }
        }
        return item
    }

    private func retain(_ node: FSBridgeNode, parent: UInt64, grantsReference: Bool) throws -> RepoReachItem {
        _ = try identifier(node.inode)
        return try locked {
            if var state = items[node.inode] {
                guard state.item.generation == node.generation, !state.removed else { throw POSIXError(.ESTALE) }
                guard state.lookupReferences < UInt64.max else { throw POSIXError(.EOVERFLOW) }
                if grantsReference { state.lookupReferences += 1 }
                state.attributes = node.attributes
                state.parent = parent
                state.reclaimed = false
                items[node.inode] = state
                return state.item
            }
            let item: RepoReachItem
            if node.inode == 1, let rootItem {
                item = rootItem
            } else {
                item = RepoReachItem(inode: node.inode, generation: node.generation)
                if node.inode == 1 { rootItem = item }
            }
            items[node.inode] = ItemState(item: item, attributes: node.attributes, parent: parent,
                lookupReferences: grantsReference ? 1 : 0)
            return item
        }
    }

    private func state(for item: FSItem) throws -> ItemState {
        guard let item = item as? RepoReachItem,
              let state = locked({ items[item.inode] }), state.item === item else { throw POSIXError(.ESTALE) }
        return state
    }

    private func directoryState(_ item: FSItem) throws -> ItemState {
        let state = try state(for: item)
        guard state.attributes.type == .dir else { throw POSIXError(.ENOTDIR) }
        guard !state.removed else { throw POSIXError(.ENOENT) }
        return state
    }

    private func requireNode(_ response: FSBridgeResponse) throws -> FSBridgeNode {
        guard let node = response.node else { throw POSIXError(.EIO) }
        return node
    }

    private func validateIdentity(_ node: FSBridgeNode, item: RepoReachItem) throws {
        // getattr/setattr have no generation in fuseops, encoded as zero by v1.
        guard node.inode == item.inode,
              node.generation == 0 || node.generation == item.generation else { throw POSIXError(.ESTALE) }
    }

    /// FSKit reserves 1 for parent-of-root and 2 for root; the bridge uses root 1.
    private func identifier(_ inode: UInt64) throws -> FSItem.Identifier {
        guard inode > 0, inode < UInt64.max, let identifier = FSItem.Identifier(rawValue: inode + 1) else {
            throw POSIXError(.EOVERFLOW)
        }
        return identifier
    }

    private func component(_ name: FSFileName, allowDots: Bool = false) throws -> String {
        guard let string = name.string else { throw POSIXError(.EILSEQ) }
        guard !string.isEmpty, !name.data.contains(0), !name.data.contains(47) else { throw POSIXError(.EINVAL) }
        guard name.data.count <= maximumNameLength else { throw POSIXError(.ENAMETOOLONG) }
        guard allowDots || (string != "." && string != "..") else { throw POSIXError(.EINVAL) }
        return string
    }

    private func xattrName(_ name: FSFileName) throws -> String {
        guard let string = String(data: name.data, encoding: .utf8) else { throw POSIXError(.EINVAL) }
        guard !name.data.isEmpty, !name.data.contains(0) else { throw POSIXError(.EINVAL) }
        guard name.data.count <= FSBridgeClient.maximumXattrNameSize else { throw POSIXError(.ENAMETOOLONG) }
        return string
    }

    private func requireWritable() throws { if readOnly { throw POSIXError(.EROFS) } }

    private func attributeChanges(_ request: FSItem.SetAttributesRequest,
                                  type: FSBridgeAttributes.FileType) throws -> FSBridgeAttributeChanges {
        for attribute in [FSItem.Attribute.type, .linkCount, .allocSize, .fileID, .parentID,
                          .changeTime, .supportsLimitedXAttrs, .inhibitKernelOffloadedIO] {
            if request.isValid(attribute) { throw POSIXError(.EINVAL) }
        }
        var changes = FSBridgeAttributeChanges()
        if request.isValid(.size), type == .file {
            guard request.size <= maximumFileSize else { throw POSIXError(.EFBIG) }
            changes.size = request.size
        }
        if request.isValid(.mode), type != .symlink {
            guard request.mode & ~UInt32(0o777) == 0 else { throw POSIXError(.ENOTSUP) }
            changes.mode = request.mode
        }
        if request.isValid(.modifyTime) { changes.mtimeNS = try nanoseconds(request.modifyTime) }
        return changes
    }

    private func consume(_ request: FSItem.SetAttributesRequest, changes: FSBridgeAttributeChanges) {
        var consumed: FSItem.Attribute = []
        if changes.size != nil { consumed.insert(.size) }
        if changes.mode != nil { consumed.insert(.mode) }
        if changes.mtimeNS != nil { consumed.insert(.modifyTime) }
        request.consumedAttributes.formUnion(consumed)
    }

    private func applyCreationAttributes(_ request: FSItem.SetAttributesRequest, changes: FSBridgeAttributeChanges,
                                         item: RepoReachItem, initialMode: UInt32?) async throws {
        var remaining = changes
        if let initialMode, remaining.mode == initialMode {
            remaining.mode = nil
            request.consumedAttributes.insert(.mode)
        }
        if remaining.size != nil || remaining.mode != nil || remaining.mtimeNS != nil {
            let state = try state(for: item)
            let node = try requireNode(try await client.request(FSBridgeRequest(op: "setattr", inode: item.inode,
                handle: state.handle, attributes: remaining)))
            try validateIdentity(node, item: item)
            updateAttributes(node.attributes, inode: item.inode)
            consume(request, changes: remaining)
        }
    }

    private func makeAttributes(_ value: FSBridgeAttributes, inode: UInt64, parent: UInt64,
                                desired: FSItem.GetAttributesRequest?, removed: Bool) throws -> FSItem.Attributes {
        let result = FSItem.Attributes()
        func wanted(_ attribute: FSItem.Attribute) -> Bool { desired?.isAttributeWanted(attribute) ?? true }
        if wanted(.type) { result.type = itemType(value.type) }
        if wanted(.mode) { result.mode = value.mode & 0o777 }
        if wanted(.uid) { result.uid = value.uid }
        if wanted(.gid) { result.gid = value.gid }
        if wanted(.flags) { result.flags = 0 }
        if wanted(.linkCount) { result.linkCount = removed ? 0 : (value.type == .dir ? max(2, value.nlink) : 1) }
        if wanted(.size), value.sizeKnown != false { result.size = value.size }
        if wanted(.fileID) { result.fileID = try identifier(inode) }
        if wanted(.parentID) { result.parentID = inode == 1 ? .parentOfRoot : try identifier(parent) }
        if wanted(.accessTime) { result.accessTime = timespecFromNanoseconds(value.atimeNS) }
        if wanted(.modifyTime) { result.modifyTime = timespecFromNanoseconds(value.mtimeNS) }
        if wanted(.changeTime) { result.changeTime = timespecFromNanoseconds(value.ctimeNS) }
        if wanted(.birthTime), value.birthtimeNS != 0 { result.birthTime = timespecFromNanoseconds(value.birthtimeNS) }
        if wanted(.supportsLimitedXAttrs) { result.supportsLimitedXAttrs = false }
        if wanted(.inhibitKernelOffloadedIO) { result.inhibitKernelOffloadedIO = true }
        return result
    }

    private func itemType(_ type: FSBridgeAttributes.FileType) -> FSItem.ItemType {
        switch type { case .file: return .file; case .dir: return .directory; case .symlink: return .symlink }
    }

    private func updateAttributes(_ attributes: FSBridgeAttributes, inode: UInt64) {
        locked { items[inode]?.attributes = attributes }
    }

    private func noteTransientReference(_ inode: UInt64) {
        locked { transientReferences[inode, default: 0] += 1 }
    }

    private func consumeTransientReference(_ inode: UInt64) {
        locked { consumeTransientReferences(inode, count: 1) }
    }

    /// The caller holds the item-state lock and has acknowledged these counts.
    private func consumeTransientReferences(_ inode: UInt64, count: UInt64) {
        guard let held = transientReferences[inode] else { return }
        if held <= count { transientReferences.removeValue(forKey: inode) }
        else { transientReferences[inode] = held - count }
    }

    private func forgetTransientReference(_ inode: UInt64) async throws {
        _ = try await client.request(FSBridgeRequest(op: "forget", inode: inode, n: 1), timeout: 10)
        consumeTransientReference(inode)
    }

    private func recordWrite(inode: UInt64, end: UInt64) {
        locked {
            guard let state = items[inode] else { return }
            let value = state.attributes
            let now = Int64(Date().timeIntervalSince1970 * 1_000_000_000)
            items[inode]?.attributes = FSBridgeAttributes(size: max(value.size, end), nlink: value.nlink,
                mode: value.mode, type: value.type, uid: value.uid, gid: value.gid, atimeNS: value.atimeNS,
                mtimeNS: now, ctimeNS: now, birthtimeNS: value.birthtimeNS, sizeKnown: value.sizeKnown)
        }
    }

    private func nanoseconds(_ value: timespec) throws -> Int64 {
        guard value.tv_nsec >= 0, value.tv_nsec < 1_000_000_000 else { throw POSIXError(.EINVAL) }
        let seconds = Int64(value.tv_sec).multipliedReportingOverflow(by: 1_000_000_000)
        let total = seconds.partialValue.addingReportingOverflow(Int64(value.tv_nsec))
        guard !seconds.overflow, !total.overflow else { throw POSIXError(.EOVERFLOW) }
        return total.partialValue
    }

    private func timespecFromNanoseconds(_ value: Int64) -> timespec {
        var seconds = value / 1_000_000_000
        var remainder = value % 1_000_000_000
        if remainder < 0 { seconds -= 1; remainder += 1_000_000_000 }
        return timespec(tv_sec: Int(seconds), tv_nsec: Int(remainder))
    }

    private func cleanup(_ request: FSBridgeRequest, deadline: UInt64? = nil) async {
        var timeout: TimeInterval = 10
        if let deadline {
            let now = DispatchTime.now().uptimeNanoseconds
            guard now < deadline else { return }
            timeout = min(timeout, Double(deadline - now) / 1_000_000_000)
        }
        do { _ = try await client.request(request, timeout: timeout) }
        catch { logger.error("Filesystem cleanup failed with POSIX error \(self.posix(error).code, privacy: .public)") }
    }

    private func posix(_ error: Error) -> NSError {
        if let bridge = error as? FSBridgeError { return NSError(domain: NSPOSIXErrorDomain, code: Int(bridge.errno)) }
        if error is CancellationError { return NSError(domain: NSPOSIXErrorDomain, code: Int(EINTR)) }
        let nsError = error as NSError
        if nsError.domain == NSPOSIXErrorDomain || nsError.domain == FSKitErrorDomain { return nsError }
        return NSError(domain: NSPOSIXErrorDomain, code: Int(EIO))
    }

    private func perform<T>(exclusive: Bool, cancelOnDrain: Bool? = nil, reply: @escaping (T?, Error?) -> Void,
                            operation: @escaping () async throws -> T) {
        let id = UUID()
        let accepted = locked { () -> Bool in
            guard !isClosing else { return false }
            let epoch = sessionEpoch
            let task = Task {
                do {
                    let permit = try await self.gate.acquire(exclusive: exclusive)
                    do {
                        guard self.locked({ self.sessionEpoch == epoch && !self.isClosing }) else {
                            throw POSIXError(.ENXIO)
                        }
                        let result = try await operation()
                        await self.gate.release(permit)
                        reply(result, nil)
                    } catch {
                        await self.gate.release(permit)
                        reply(nil, self.posix(error))
                    }
                } catch { reply(nil, self.posix(error)) }
                self.locked { _ = self.running.removeValue(forKey: id) }
            }
            running[id] = RunningOperation(task: task, cancelOnDrain: cancelOnDrain ?? !exclusive)
            return true
        }
        if !accepted { reply(nil, POSIXError(.ENXIO)) }
    }

    private func perform(exclusive: Bool, reply: @escaping (Error?) -> Void,
                         operation: @escaping () async throws -> Void) {
        perform(exclusive: exclusive, reply: { (_: Void?, error: Error?) in reply(error) }, operation: operation)
    }

    private func perform(exclusive: Bool, cancelOnDrain: Bool? = nil, reply: @escaping (FSItem?, FSFileName?, Error?) -> Void,
                         operation: @escaping () async throws -> (FSItem, FSFileName)) {
        perform(exclusive: exclusive, cancelOnDrain: cancelOnDrain, reply: { (value: (FSItem, FSFileName)?, error: Error?) in
            reply(value?.0, value?.1, error)
        }, operation: operation)
    }

    private func perform(exclusive: Bool, reply: @escaping (FSDirectoryVerifier, Error?) -> Void,
                         operation: @escaping () async throws -> FSDirectoryVerifier) {
        perform(exclusive: exclusive, reply: { (value: FSDirectoryVerifier?, error: Error?) in
            reply(value ?? FSDirectoryVerifier(0), error)
        }, operation: operation)
    }

    private func locked<T>(_ body: () throws -> T) rethrows -> T {
        lock.lock(); defer { lock.unlock() }
        return try body()
    }
}
