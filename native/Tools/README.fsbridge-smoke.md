# Native client and Go filesystem bridge smoke

This check compiles the production Swift bridge client and runs it against the actual Go Unix-socket server, catalogue, snapshot, writable overlay, and hydrator fixture. It requires macOS and Swift, but no FSKit framework, kernel driver, mount, GitHub account, or network repository. It verifies the protocol boundary; it does not establish that an FSKit volume can mount.

Run from the repository root:

```sh
mkdir -p build/fsbridge-smoke
swiftc -parse-as-library native/Shared/FSBridgeContainer.swift native/FSKitExtension/BridgeConfiguration.swift \
  native/FSKitExtension/BridgeModels.swift native/FSKitExtension/BridgeTransport.swift \
  native/FSKitExtension/BridgeClient.swift native/Tools/fsbridge-smoke.swift \
  -o build/fsbridge-smoke/check
AFS_NATIVE_BRIDGE_CLIENT="$PWD/build/fsbridge-smoke/check" \
  GOTOOLCHAIN=go1.26.8 go test -count=1 -run '^TestNativeClientInterop$' -v ./internal/fsbridge
```

The test uses a fresh private session and checks root attributes, complete directory pagination and large HTTP framing, UTF-8 names, committed binary hydration, binary writes and ranged reads, empty directories and zero-length I/O, authentication rejection, POSIX errors, and offset/chunk bounds. Go independently verifies that the Swift mutations reached the real writable engine. The capability remains in the private descriptor and authorization header; it is never a command argument or test log.

## FSVolume callback integration

On macOS 15.4 or later, a second harness invokes production volume callbacks using actual SDK objects and the same Go backend:

```sh
xcrun swiftc -parse-as-library -target arm64-apple-macos15.4 \
  native/Shared/FSBridgeContainer.swift native/FSKitExtension/BridgeModels.swift native/FSKitExtension/BridgeConfiguration.swift \
  native/FSKitExtension/BridgeTransport.swift native/FSKitExtension/BridgeClient.swift \
  native/FSKitExtension/RepoReachVolume.swift native/Tools/fsbridge-volume-smoke.swift \
  -o build/fsbridge-smoke/volume-check
REPOREACH_FSBRIDGE_NATIVE_CLIENT="$PWD/build/fsbridge-smoke/volume-check" \
  GOTOOLCHAIN=go1.26.8 go test -count=1 -run '^TestNativeFSKitBridgeClient$' -v ./internal/fsbridge
```

For an Intel host, use `x86_64-apple-macos15.4` as the target. This exercises activation, root identities, creation and consumed attributes, binary writes, permission changes and retained access, rename/unlink with open handles, committed copy-on-write reader visibility, symlinks, stale items after remount, unsupported operations, read-only errors, and concurrent shutdown. Unsupported access-time changes remain unconsumed and the returned metadata must reflect the actual backend state.

FSKit-created read buffers, directory packers, and task options have no public constructors. The harness uses the volume's option-independent activation/mount entry points, and checks binary reads through the production bridge. It does not exercise FSKit's mounted read callback or directory packing, URL-resource scope, framework session ordering, or a restarted Go service. Those require the real macOS 26 installed mount tests.
