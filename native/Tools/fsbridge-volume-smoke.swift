// Compile with the production bridge and volume files, then run through
// TestNativeFSKitBridgeClient. This uses real SDK FSItem/attribute objects and
// the real Go catalogue/overlay service without requesting a system mount.
// FSKit-created options, buffers and directory packers have no public constructors.
// The shared chunk-consumer path validates reads without fabricating a framework
// buffer; mounted buffer handoff and enumeration remain separate acceptance checks.
import Foundation
import FSKit
import Darwin

@available(macOS 15.4, *)
private func checked(_ condition: @autoclosure () -> Bool, _ message: String) throws {
    if !condition() { throw NSError(domain: "VolumeSmoke", code: 1, userInfo: [NSLocalizedDescriptionKey: message]) }
}

@available(macOS 15.4, *)
private func one<T>(_ call: (@escaping (T?, Error?) -> Void) -> Void) async throws -> T {
    try await withCheckedThrowingContinuation { continuation in
        call { value, error in
            if let error { continuation.resume(throwing: error) }
            else if let value { continuation.resume(returning: value) }
            else { continuation.resume(throwing: POSIXError(.EIO)) }
        }
    }
}

@available(macOS 15.4, *)
private func pair(_ call: (@escaping (FSItem?, FSFileName?, Error?) -> Void) -> Void) async throws -> FSItem {
    try await one { reply in call { item, _, error in reply(item, error) } }
}

@available(macOS 15.4, *)
private func done(_ call: (@escaping (Error?) -> Void) -> Void) async throws {
    let _: Bool = try await one { reply in call { error in reply(error == nil ? true : nil, error) } }
}

@available(macOS 15.4, *)
private func errorCode(_ expected: Int32, _ operation: () async throws -> Void) async throws {
    do { try await operation(); throw NSError(domain: "VolumeSmoke", code: 2, userInfo: [NSLocalizedDescriptionKey: "Expected error \(expected)"]) }
    catch {
        let error = error as NSError
        try checked(error.domain == NSPOSIXErrorDomain && error.code == Int(expected), "Unexpected error domain/code: \(error.domain)/\(error.code), expected \(expected)")
    }
}

@available(macOS 15.4, *)
private func attrs(_ volume: RepoReachVolume, _ item: FSItem) async throws -> FSItem.Attributes {
    let request = FSItem.GetAttributesRequest()
    request.wantedAttributes = [.type, .fileID, .parentID, .mode, .size, .linkCount, .modifyTime]
    return try await one { volume.getAttributes(request, of: item, replyHandler: $0) }
}

@available(macOS 15.4, *)
private func create(_ volume: RepoReachVolume, _ parent: FSItem, _ name: String,
                    type: FSItem.ItemType = .file, attributes: FSItem.SetAttributesRequest? = nil) async throws -> FSItem {
    let attributes = attributes ?? FSItem.SetAttributesRequest()
    return try await pair { volume.createItem(named: FSFileName(string: name), type: type, inDirectory: parent, attributes: attributes, replyHandler: $0) }
}

@available(macOS 15.4, *)
private func lookup(_ volume: RepoReachVolume, _ parent: FSItem, _ name: String) async throws -> FSItem {
    try await pair { volume.lookupItem(named: FSFileName(string: name), inDirectory: parent, replyHandler: $0) }
}

private final class ReadReceipt: @unchecked Sendable {
    private let lock = NSLock()
    private var bytes = Data()
    private var contiguous = true

    func consume(_ data: Data, at position: Int) {
        lock.lock(); defer { lock.unlock() }
        contiguous = contiguous && position == bytes.count
        bytes.append(data)
    }

    var result: (Data, Bool) {
        lock.lock(); defer { lock.unlock() }
        return (bytes, contiguous)
    }
}

@available(macOS 15.4, *)
private func read(_ volume: RepoReachVolume, _ item: FSItem, offset: off_t = 0, length: Int) async throws -> Data {
    let receipt = ReadReceipt()
    let count: Int = try await one { reply in
        volume.read(from: item, at: offset, length: length, capacity: length,
                    consume: { receipt.consume($0, at: $1) }) { count, error in reply(count, error) }
    }
    let (bytes, contiguous) = receipt.result
    try checked(contiguous && bytes.count == count, "Read chunks preserve their offsets and returned byte count")
    return bytes
}

@main
struct VolumeSmoke {
    static func main() async {
        do {
            guard #available(macOS 15.4, *) else { throw POSIXError(.ENOTSUP) }
            try await run()
        } catch {
            fputs("FAIL volume smoke: \(error.localizedDescription)\n", stderr)
            exit(1)
        }
    }

    @available(macOS 15.4, *)
    static func run() async throws {
        guard CommandLine.arguments.count == 2 else { throw POSIXError(.EINVAL) }
        let source = URL(fileURLWithPath: CommandLine.arguments[1], isDirectory: true)
        let configURL = source.appendingPathComponent("connection.json")
        let raw = try FSBridgeClient(configURL: configURL)
        let client = try FSBridgeClient(configURL: configURL)
        let volume = RepoReachVolume(client: client, identifier: UUID(), readOnly: false)
        let root: FSItem = try await one { volume.activate(replyHandler: $0) }
        let rootAttrs = try await attrs(volume, root)
        try checked((root as? RepoReachItem)?.inode == 1 && rootAttrs.fileID == .rootDirectory && rootAttrs.type == .directory, "Root inode translation")
        let owner = try await lookup(volume, root, "alice")
        let repo = try await lookup(volume, owner, "project")
        let beforeOpen = try await lookup(volume, repo, "README.md")
        let header = try await read(volume, beforeOpen, length: 4096)
        try checked(!header.isEmpty, "Vnode header read succeeds before any open callback")
        try await errorCode(EBADF) { try await done { volume.closeItem(beforeOpen, modes: [.read], replyHandler: $0) } }
        for _ in 0..<257 {
            let byte = try await read(volume, beforeOpen, length: 1)
            try checked(byte == header.prefix(1), "Temporary readers are released beyond the pending-handle limit")
        }
        try await done { volume.openItem(beforeOpen, modes: [.read], replyHandler: $0) }
        let retainedHeader = try await read(volume, beforeOpen, length: 4096)
        try checked(retainedHeader == header, "A retained reader produces identical header bytes")
        try await done { volume.closeItem(beforeOpen, modes: [], replyHandler: $0) }
        let afterClose = try await read(volume, beforeOpen, length: 4096)
        try checked(afterClose == header, "Vnode read succeeds after final close without retaining read modes")
        let beyondEOF = try await read(volume, beforeOpen, offset: off_t(header.count + 4096), length: 17)
        try checked(beyondEOF.isEmpty, "Temporary vnode reader beyond EOF returns zero bytes")
        try await errorCode(EBADF) { try await done { volume.closeItem(beforeOpen, modes: [.read], replyHandler: $0) } }
        let initial = FSItem.SetAttributesRequest()
        initial.mode = 0o640
        initial.size = 4
        initial.modifyTime = timespec(tv_sec: 1_700_000_000, tv_nsec: 123_456_789)
        let file = try await create(volume, repo, "native-volume.bin", attributes: initial)
        let fileAttrs = try await attrs(volume, file)
        try checked(fileAttrs.mode == 0o640 && fileAttrs.size == 4 && initial.wasAttributeConsumed(.mode) && initial.wasAttributeConsumed(.size) && initial.wasAttributeConsumed(.modifyTime), "Creation attributes and consumed masks")
        let combined = FSItem.SetAttributesRequest()
        combined.mode = 0o644
        combined.modifyTime = timespec(tv_sec: 1_700_000_001, tv_nsec: 234_567_890)
        combined.accessTime = timespec(tv_sec: 123, tv_nsec: 456)
        let combinedAttrs: FSItem.Attributes = try await one { volume.setAttributes(combined, on: file, replyHandler: $0) }
        try checked(combined.wasAttributeConsumed(.mode) && combined.wasAttributeConsumed(.modifyTime) && !combined.wasAttributeConsumed(.accessTime), "Unsupported accessTime is left unconsumed")
        try checked(combinedAttrs.mode == 0o644 && combinedAttrs.modifyTime.tv_sec == 1_700_000_001 && combinedAttrs.modifyTime.tv_nsec == 234_567_890 && combinedAttrs.accessTime.tv_sec != 123, "Combined chmod/utimes does not fake atime")
        let binary = Data((0..<(2_097_152 + 17)).map { UInt8(truncatingIfNeeded: $0) })
        let written: Int = try await one { reply in volume.write(contents: binary, to: file, at: 0) { count, error in reply(count, error) } }
        try checked(written == binary.count, "Chunked volume binary write")
        let fileInode = (file as! RepoReachItem).inode
        let extraHandle = try await raw.request(FSBridgeRequest(op: "open", inode: fileInode)).handle!
        let bytes = try await raw.read(inode: fileInode, handle: extraHandle, offset: 0, size: 1_048_576)
        try checked(bytes == binary.prefix(1_048_576), "Binary bytes were preserved")
        let tail = try await raw.read(inode: fileInode, handle: extraHandle, offset: 2_097_152, size: 17)
        try checked(tail == binary.suffix(17), "Final binary chunk was preserved")
        try await done { volume.closeItem(file, modes: [], replyHandler: $0) }
        let binaryAfterClose = try await read(volume, file, length: binary.count)
        try checked(binaryAfterClose == binary, "Temporary vnode reader preserves every byte across multiple chunks")
        try await errorCode(EBADF) { try await done { volume.closeItem(file, modes: [.read], replyHandler: $0) } }
        try await done { volume.openItem(file, modes: [.read, .write], replyHandler: $0) }
        let shorter = FSItem.SetAttributesRequest(); shorter.size = 1024
        let shortAttrs: FSItem.Attributes = try await one { volume.setAttributes(shorter, on: file, replyHandler: $0) }
        try checked(shortAttrs.size == 1024 && shorter.wasAttributeConsumed(.size), "Truncate attributes")
        let target = try await create(volume, repo, "native-target.bin")
        let _: Int = try await one { reply in volume.write(contents: Data([9, 8, 7]), to: target, at: 0) { count, error in reply(count, error) } }
        let _: FSFileName = try await one { volume.renameItem(file, inDirectory: repo, named: FSFileName(string: "native-volume.bin"), to: FSFileName(string: "native-target.bin"), inDirectory: repo, overItem: target, replyHandler: $0) }
        let removedTarget = try await attrs(volume, target)
        try checked(removedTarget.size == 3 && removedTarget.linkCount == 0, "Overwritten open vnode keeps descriptor metadata")
        try await done { volume.removeItem(file, named: FSFileName(string: "native-target.bin"), fromDirectory: repo, replyHandler: $0) }
        let unlinked = try await attrs(volume, file)
        try checked(unlinked.size == 1024 && unlinked.linkCount == 0, "Open-unlinked getattr")
        let detachedSize = FSItem.SetAttributesRequest(); detachedSize.size = 2
        let detachedAttrs: FSItem.Attributes = try await one { volume.setAttributes(detachedSize, on: file, replyHandler: $0) }
        try checked(detachedAttrs.size == 2 && detachedAttrs.linkCount == 0, "Open-unlinked truncate")
        let detachedWrite: Int = try await one { reply in volume.write(contents: Data([0, 255]), to: file, at: 0) { count, error in reply(count, error) } }
        try checked(detachedWrite == 2, "Open-unlinked write")
        let detachedBytes = try await raw.read(inode: fileInode, handle: extraHandle, offset: 0, size: 10)
        try checked(detachedBytes == Data([0, 255]), "Independent retained handle observes detached write")
        let recreated = try await create(volume, repo, "native-target.bin")
        try checked((recreated as! RepoReachItem).inode != fileInode, "Recreated path receives a new inode")
        let link: FSItem = try await pair { volume.createSymbolicLink(named: FSFileName(string: "native-link"), inDirectory: repo, attributes: FSItem.SetAttributesRequest(), linkContents: FSFileName(string: "../README.md"), replyHandler: $0) }
        let linkTarget: FSFileName = try await one { volume.readSymbolicLink(link, replyHandler: $0) }
        try checked(linkTarget.string == "../README.md", "Symbolic link round trip")
        let parent = try await create(volume, repo, "native-dir", type: .directory)
        let child = try await create(volume, parent, "child", type: .directory)
        try await done { volume.reclaimItem(parent, replyHandler: $0) }
        let retainedParent = try await lookup(volume, child, "..")
        try checked(retainedParent === parent, "Reclaimed parent retained for child identity")
        let immutable = FSItem.SetAttributesRequest(); immutable.changeTime = timespec(tv_sec: 1, tv_nsec: 0)
        try await errorCode(EINVAL) { let _: FSItem.Attributes = try await one { volume.setAttributes(immutable, on: recreated, replyHandler: $0) } }
        try await errorCode(ENOTSUP) { _ = try await create(volume, repo, "fifo", type: .fifo) }
        try await errorCode(ENOTSUP) { let _: FSFileName = try await one { volume.createLink(to: recreated, named: FSFileName(string: "hardlink"), inDirectory: repo, replyHandler: $0) } }
        let ro = RepoReachVolume(client: try FSBridgeClient(configURL: configURL), identifier: UUID(), readOnly: true)
        let roRoot: FSItem = try await one { ro.activate(replyHandler: $0) }
        try await errorCode(EROFS) { _ = try await create(ro, roRoot, "forbidden") }
        _ = try await raw.request(FSBridgeRequest(op: "release", inode: fileInode, handle: extraHandle))
        try await done { volume.closeItem(file, modes: [], replyHandler: $0) }
        let closedRemoved = try await attrs(volume, file)
        try checked(closedRemoved.size == 2 && closedRemoved.linkCount == 0, "Closed removed vnode keeps last attributes")
        for mode: UInt32 in [0o400, 0] {
            let request = FSItem.SetAttributesRequest(); request.mode = mode
            let restricted = try await create(volume, repo, "native-restricted-\(mode)", attributes: request)
            let count: Int = try await one { reply in volume.write(contents: Data([0, 255]), to: restricted, at: 0) { count, error in reply(count, error) } }
            let restrictedAttrs = try await attrs(volume, restricted)
            try checked(count == 2 && restrictedAttrs.mode == mode, "Creation descriptor survives restrictive final mode")
        }
        let permissions = FSItem.SetAttributesRequest(); permissions.mode = 0o600
        let access = try await create(volume, repo, "native-access.bin", attributes: permissions)
        try await done { volume.closeItem(access, modes: [], replyHandler: $0) }
        try await done { volume.openItem(access, modes: [.read], replyHandler: $0) }
        let accessInode = (access as! RepoReachItem).inode
        let retainedReader = try await raw.request(FSBridgeRequest(op: "open", inode: accessInode, access: 1)).handle!
        let writeOnly = FSItem.SetAttributesRequest(); writeOnly.mode = 0o200
        let _: FSItem.Attributes = try await one { volume.setAttributes(writeOnly, on: access, replyHandler: $0) }
        try await done { volume.openItem(access, modes: [.write], replyHandler: $0) }
        let accessWrite: Int = try await one { reply in volume.write(contents: Data([7, 0, 255]), to: access, at: 0) { count, error in reply(count, error) } }
        let retainedBytes = try await raw.read(inode: accessInode, handle: retainedReader, offset: 0, size: 3)
        try checked(accessWrite == 3 && retainedBytes == Data([7, 0, 255]), "Reader survives chmod0200 while new write capability opens")
        _ = try await raw.request(FSBridgeRequest(op: "release", inode: accessInode, handle: retainedReader))
        try await done { volume.closeItem(access, modes: [], replyHandler: $0) }
        try await done { volume.openItem(access, modes: [.write], replyHandler: $0) }
        let readOnly = FSItem.SetAttributesRequest(); readOnly.mode = 0o400
        let _: FSItem.Attributes = try await one { volume.setAttributes(readOnly, on: access, replyHandler: $0) }
        try await done { volume.openItem(access, modes: [.read], replyHandler: $0) }
        let writerAfterChmod: Int = try await one { reply in volume.write(contents: Data([4, 3, 2]), to: access, at: 0) { count, error in reply(count, error) } }
        try checked(writerAfterChmod == 3, "Writer survives chmod0400 while new read capability opens")
        try await done { volume.closeItem(access, modes: [.read], replyHandler: $0) }
        try await errorCode(EACCES) { try await done { volume.openItem(access, modes: [.write], replyHandler: $0) } }
        await withCheckedContinuation { continuation in volume.unmount { continuation.resume() } }
        try await done { volume.reclaimItem(recreated, replyHandler: $0) }
        try await done { volume.mount(replyHandler: $0) }
        let sameRoot: FSItem = try await one { volume.activate(replyHandler: $0) }
        try checked(sameRoot === root, "Remount preserves root item identity")
        try await errorCode(ESTALE) { _ = try await attrs(volume, recreated) }
        let newOwner = try await lookup(volume, root, "alice")
        let newRepo = try await lookup(volume, newOwner, "project")
        _ = try await lookup(volume, newRepo, "native-target.bin")
        let committedOwner = try await lookup(volume, root, "team")
        let committedRepo = try await lookup(volume, committedOwner, "project")
        let committed = try await lookup(volume, committedRepo, "README.md")
        try await done { volume.openItem(committed, modes: [.read], replyHandler: $0) }
        let committedInode = (committed as! RepoReachItem).inode
        let committedReader = try await raw.request(FSBridgeRequest(op: "open", inode: committedInode, access: 1)).handle!
        _ = try await raw.read(inode: committedInode, handle: committedReader, offset: 0, size: 3)
        try await done { volume.openItem(committed, modes: [.write], replyHandler: $0) }
        let cowWrite: Int = try await one { reply in volume.write(contents: Data([77, 0, 255]), to: committed, at: 0) { count, error in reply(count, error) } }
        let cowRead = try await raw.read(inode: committedInode, handle: committedReader, offset: 0, size: 3)
        _ = try await raw.request(FSBridgeRequest(op: "release", inode: committedInode, handle: committedReader))
        async let first: Void = volume.shutdown()
        async let second: Void = volume.shutdown()
        _ = await (first, second)
        await ro.shutdown()
        try await errorCode(ENXIO) { _ = try await attrs(volume, root) }
        await raw.close()
        try checked(cowWrite == 3 && cowRead == Data([77, 0, 255]), "Previously opened committed reader observes linked copy-on-write data; fixture bytes returned: \(Array(cowRead))")
        print("PASS actual SDK FSVolume lifecycle/remount, vnode reads before open/after close, bounded temporary readers, binary reads/writes, root IDs, lookup/create/setattrs masks, rename/unlink retained handles, restrictive create/chmod capabilities, committed copy-on-write reads, symlinks, parent retention, unsupported operations, read-only errors, concurrent shutdown")
    }
}
