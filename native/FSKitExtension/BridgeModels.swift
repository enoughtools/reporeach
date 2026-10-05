import Foundation
import Darwin

/// Version 1 of the private, inode-based filesystem bridge. Optional request
/// fields are omitted rather than encoded as null, matching the Go endpoint.
struct FSBridgeRequest: Encodable, Sendable {
    let version = 1
    let op: String
    var inode: UInt64?
    var parent: UInt64?
    var name: String?
    var handle: UInt64?
    var offset: UInt64?
    var size: Int64?
    var mode: UInt32?
    var access: UInt32?
    var oldParent: UInt64?
    var oldName: String?
    var newParent: UInt64?
    var newName: String?
    var target: String?
    var n: UInt64?
    var attributes: FSBridgeAttributeChanges?

    enum CodingKeys: String, CodingKey {
        case version, op, inode, parent, name, handle, offset, size, mode, access, target, n, attributes
        case oldParent = "old_parent", oldName = "old_name"
        case newParent = "new_parent", newName = "new_name"
    }

    init(op: String, inode: UInt64? = nil, parent: UInt64? = nil, name: String? = nil,
         handle: UInt64? = nil, offset: UInt64? = nil, size: Int64? = nil, mode: UInt32? = nil, access: UInt32? = nil,
         oldParent: UInt64? = nil, oldName: String? = nil, newParent: UInt64? = nil,
         newName: String? = nil, target: String? = nil, n: UInt64? = nil,
         attributes: FSBridgeAttributeChanges? = nil) {
        self.op = op
        self.inode = inode
        self.parent = parent
        self.name = name
        self.handle = handle
        self.offset = offset
        self.size = size
        self.mode = mode
        self.access = access
        self.oldParent = oldParent
        self.oldName = oldName
        self.newParent = newParent
        self.newName = newName
        self.target = target
        self.n = n
        self.attributes = attributes
    }
}

struct FSBridgeAttributeChanges: Codable, Equatable, Sendable {
    var size: UInt64?
    var mode: UInt32?
    var uid: UInt32?
    var gid: UInt32?
    var atimeNS: Int64?
    var mtimeNS: Int64?

    enum CodingKeys: String, CodingKey {
        case size, mode, uid, gid
        case atimeNS = "atime_ns", mtimeNS = "mtime_ns"
    }
}

struct FSBridgeResponse: Decodable, Sendable {
    let version: Int
    let errno: Int32
    let node: FSBridgeNode?
    let handle: UInt64?
    let entries: [FSBridgeDirectoryEntry]?
    let nextOffset: UInt64?
    let eof: Bool?
    let target: String?
    let written: Int?
    let stat: FSBridgeStat?

    enum CodingKeys: String, CodingKey {
        case version, errno, node, handle, entries, eof, target, written, stat
        case nextOffset = "next_offset"
    }
}

struct FSBridgeNode: Codable, Equatable, Sendable {
    let inode: UInt64
    let generation: UInt64
    let attributes: FSBridgeAttributes
}

struct FSBridgeAttributes: Codable, Equatable, Sendable {
    enum FileType: String, Codable, Sendable { case file, dir, symlink }
    let size: UInt64
    let nlink: UInt32
    let mode: UInt32
    let type: FileType
    let uid: UInt32
    let gid: UInt32
    let atimeNS: Int64
    let mtimeNS: Int64
    let ctimeNS: Int64
    let birthtimeNS: Int64

    enum CodingKeys: String, CodingKey {
        case size, nlink, mode, type, uid, gid
        case atimeNS = "atime_ns", mtimeNS = "mtime_ns"
        case ctimeNS = "ctime_ns", birthtimeNS = "birthtime_ns"
    }
}

struct FSBridgeDirectoryEntry: Codable, Equatable, Sendable {
    let name: String
    let offset: UInt64
    let node: FSBridgeNode
}

struct FSBridgeStat: Codable, Equatable, Sendable {
    let blockSize: UInt64
    let blocks: UInt64
    let blocksFree: UInt64
    let blocksAvailable: UInt64
    let ioSize: UInt64
    let inodes: UInt64
    let inodesFree: UInt64

    enum CodingKeys: String, CodingKey {
        case blocks, inodes
        case blockSize = "block_size", blocksFree = "blocks_free"
        case blocksAvailable = "blocks_available", ioSize = "io_size"
        case inodesFree = "inodes_free"
    }
}

enum FSBridgeError: Error, LocalizedError, Equatable, Sendable {
    case invalidConfiguration
    case invalidRequest
    case malformedResponse
    case responseTooLarge
    case unavailable(Int32)
    case filesystem(Int32)
    case timedOut
    case cancelled

    var errno: Int32 {
        switch self {
        case .invalidConfiguration: return EACCES
        case .invalidRequest: return EINVAL
        case .malformedResponse, .responseTooLarge: return EIO
        case .unavailable(let code), .filesystem(let code): return code
        case .timedOut: return ETIMEDOUT
        case .cancelled: return EINTR
        }
    }

    var errorDescription: String? {
        switch self {
        case .invalidConfiguration: return "RepoReach's private filesystem connection is invalid."
        case .invalidRequest: return "The filesystem request is invalid."
        case .malformedResponse: return "The filesystem service returned an invalid response."
        case .responseTooLarge: return "The filesystem service exceeded the response limit."
        case .unavailable: return "The RepoReach filesystem service is unavailable."
        case .filesystem(let code): return "The filesystem operation failed (error \(code))."
        case .timedOut: return "The filesystem operation timed out."
        case .cancelled: return "The filesystem operation was cancelled."
        }
    }
}
