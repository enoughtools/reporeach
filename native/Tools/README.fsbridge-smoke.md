# Native client and Go filesystem bridge smoke

This check compiles the production Swift bridge client and runs it against the actual Go Unix-socket server, catalogue, snapshot, writable overlay, and hydrator fixture. It requires macOS and Swift, but no FSKit framework, kernel driver, mount, GitHub account, or network repository. It verifies the protocol boundary; it does not establish that an FSKit volume can mount.

Run from the repository root:

```sh
mkdir -p build/fsbridge-smoke
swiftc -parse-as-library native/FSKitExtension/BridgeConfiguration.swift \
  native/FSKitExtension/BridgeModels.swift native/FSKitExtension/BridgeTransport.swift \
  native/FSKitExtension/BridgeClient.swift native/Tools/fsbridge-smoke.swift \
  -o build/fsbridge-smoke/check
AFS_NATIVE_BRIDGE_CLIENT="$PWD/build/fsbridge-smoke/check" \
  GOTOOLCHAIN=go1.26.8 go test -count=1 -run '^TestNativeClientInterop$' -v ./internal/fsbridge
```

The test uses a fresh private session and checks root attributes, complete directory pagination and large HTTP framing, UTF-8 names, committed binary hydration, binary writes and ranged reads, empty directories and zero-length I/O, authentication rejection, POSIX errors, and offset/chunk bounds. Go independently verifies that the Swift mutations reached the real writable engine. The capability remains in the private descriptor and authorization header; it is never a command argument or test log.
