# EnoughRepos filesystem extension

This is a native FSKit module for macOS 26, packaged as an ExtensionKit extension at `EnoughRepos.app/Contents/Extensions/RepoReachFSKit.appex`. Its bundle identifier is `com.enoughtools.reporeach.fskit`; its filesystem type is `reporeach`.

The visible extension name is **EnoughRepos Filesystem**. Its identifier, filesystem short name, App Group, and internal module name stay compatible with existing installations and user approvals.

The implementation follows Apple's documented [unary filesystem design](https://developer.apple.com/documentation/fskit/) and [passthrough sample](https://developer.apple.com/documentation/fskit/building-a-passthrough-file-system). It uses public APIs only. `FSPathURLResource` requires the real macOS 26 SDK and runtime. Core volume operation signatures can be checked against the installed SDK 15.5, but that is not a full extension build or a mount test.

## Resource and service boundary

The source resource is the engine's private `<stateRoot>/FSKit` directory, containing a regular, user-owned, mode `0600` `connection.json`:

```json
{"version":1,"socket":"/absolute/group-container/b0123456789abcdef","token":"64 lowercase hexadecimal characters"}
```

The socket must be an owned mode `0600` direct child of the independently resolved, entitled macOS app-group container (`<TeamID>.rr`), with a path that fits Darwin’s Unix socket address. The parent app, Go engine and module claim this group. The descriptor and mount resource remain in the state directory. The module requires the security-scoped path URL supplied by FSKit. It holds that scope from loading the volume until outstanding work and request sockets have drained during resource unload. Probe opens only a temporary scope. Invalid resource types, malformed/private-file violations, unsuccessful authorization, and backend errors fail with POSIX errors; the extension does not attempt broader filesystem access.

Metadata operations use authenticated HTTP over that UNIX socket. File bytes use separate binary reads and writes in chunks of at most 1 MiB; they never become text. The shared secret appears only in the request authorization header. Request admission, buffers, headers, responses, and deadlines are bounded. Cancellation shuts down the request's own connection and does not replay mutations. The engine owns catalogues, Git authentication, hydration, writable overlays, inode identities, and open handles.

`FSKit` reserves item identifier `1` for the parent of the root and `2` for the root itself. The engine's root inode is `1`; the module translates reported IDs to `inode + 1` and retains the original engine inode for all RPCs. Lookup references are returned when FSKit reclaims items. Open file handles outlive pathname changes and are released after admitted I/O drains.

Unmount drains the current session and returns its handles and lookup references while preserving FSKit's cached root item. A later mount revalidates `connection.json` and creates a fresh client, allowing the engine to rotate its session token. Non-root items from the old session remain stale and cannot address reused inode numbers. Resource unload permanently closes the volume and ends its security scope. Installed unmount/remount and service-restart behavior still require macOS 26 validation.

## Supported operations and limits

The volume implements required lifecycle, lookup, attributes, paged directory enumeration, creation, deletion, rename, and symbolic-link operations, plus file opening, closing, reading, and writing. Read-only resources reject mutations with `EROFS`. File writes report errors and partial progress; synchronization reports flush failures. Teardown performs best-effort flushing and cleanup because FSKit's unmount callback has no error result.

Names and symbolic-link targets must be valid UTF-8. Hard links, special files, extended attributes, immutable flags, ownership changes, and unsupported permission bits are not offered as capabilities. Unsupported operations return errors or leave unsupported set attributes unconsumed as required by FSKit. Kernel-offloaded I/O is inhibited.

Changes made by Git, refresh, or management actions outside FSKit need explicit cache-coherence validation. Until the engine and extension can demonstrate correct behavior on macOS 26, an actual mount remains a release gate; compiling this module does not establish production filesystem support. See [native FSKit validation](../../docs/reporeach/native-fskit.md).

## Build and verification

Use `native/project.yml` and the `RepoReachFSKit` scheme with Xcode 26. It embeds the filesystem module in `Contents/Extensions` and the separate Finder extension in `Contents/PlugIns`. The management app retains a macOS 13 deployment target.

`native/project-macfuse.yml` exists for legacy source checks on older SDKs. Its `RepoReach` test scheme includes the pure `Bridge*.swift` sources and the real socket fixture tests, without fake FSKit resource declarations. The core volume alone may also be checked with the real older SDK:

```sh
swiftc -target arm64-apple-macos15.4 -typecheck Shared/FSBridgeContainer.swift FSKitExtension/Bridge*.swift FSKitExtension/RepoReachVolume.swift
```

The [native bridge smoke](../Tools/README.fsbridge-smoke.md) exercises the real Swift transport and Go catalogue without a mount. A separate callback harness uses actual `FSVolume` classes where the installed SDK permits their construction. Neither substitutes for the macOS 26 resource build or installed mount checks.

The current distribution guard requires an authenticated provisioning profile for this module that authorizes `com.apple.developer.fskit.fsmodule` and matches its identifier, team, and signing certificate. This is a release validation policy while actual Developer ID provisioning and activation are being established. The extension requests the app sandbox and the same macOS Team-ID-prefixed App Group as its parent and Go engine. It does not need an outbound IP-network entitlement for this Unix socket. Apple documents this group style without registration or a separate group profile; the restricted FSKit capability still requires its own valid profile. Activation in System Settings and the socket/resource access path must be verified on an installed, signed build on macOS 26.
