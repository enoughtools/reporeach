# Read-only FSKit module selection

`inspect-fskit-module.swift` uses public FSKit discovery to ensure that the
selected RepoReach extension is the sole enabled installed filesystem with
short name `reporeach`, at the exact selected bundle path. It reads only module
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

SDK 15.5 discovery on the updated development host returned only Apple's modules;
this cannot establish selection of the enabled validation extension. The CI
workflow can export a separate SDK 26 standalone inspector, with source and
executable/archive hashes. See [mounted acceptance prerequisites](../../docs/reporeach/fskit-acceptance.md)
for verification and the `AFS_FSKIT_INSPECTOR` override. A failed inventory must
remain a failure even when System Settings shows the extension enabled.
