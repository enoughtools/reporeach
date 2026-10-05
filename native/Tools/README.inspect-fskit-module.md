# Read-only FSKit module selection

`inspect-fskit-module.swift` requires the public FSKit inventory visible to its
caller to identify the selected RepoReach extension as the sole enabled
filesystem with short name `reporeach`, at the exact selected bundle path. This
is an acceptance precondition; it does not prove which extension the system
mount dispatcher executes. It reads only module
metadata. It never signs, registers, enables, launches, or mounts an extension.

```sh
xcrun swiftc -parse-as-library -target arm64-apple-macos15.4 \
  native/Tools/inspect-fskit-module.swift -o /absolute/temporary/inspect-fskit-module
/absolute/temporary/inspect-fskit-module --self-test
/absolute/temporary/inspect-fskit-module \
  com.enoughtools.reporeach.validation.fskit \
  /absolute/RepoReach.app/Contents/Extensions/RepoReachFSKit.appex
```

Use `x86_64-apple-macos15.4` when compiling locally on Intel. The self-test runs
16 inventory and metadata rejection scenarios without querying FSClient or
opening installed module bundles. Normal inspection outputs bounded JSON and
returns zero only for a unique matching enabled module; unknown, absent,
disabled, ambiguous, or mismatched results fail. Discovery stops after 15 seconds.

SDK 15.5 discovery on the updated development host returned only Apple's three
modules. The unentitled caller's host logs recorded a Team ID lookup failure;
signing a separate inspector with Developer ID team `FGXHYHH9MC` still returned
only those three. The ad-hoc validation extension has no Team ID. Caller/signing
visibility is therefore worth investigating, but these observations do not prove
a cause or establish selection of the enabled validation extension. Apple's
actual mount dispatcher previously found the old module despite its omission
from the public inventory.

The inspector can also be built locally with Xcode 27 and its macOS 27 SDK;
Xcode/SDK 26 or later meets the native app's toolchain requirement. Record the
actual selected Xcode and SDK. The CI workflow can export a separate SDK 26
standalone inspector, with source and executable/archive hashes. Neither a newer
SDK nor Developer ID signing promises to change caller visibility. See
[mounted acceptance prerequisites](../../docs/reporeach/fskit-acceptance.md)
for verification and the `AFS_FSKIT_INSPECTOR` override. A failed inventory remains
a failure even when System Settings shows the extension enabled; no mounted pass
has been established.
