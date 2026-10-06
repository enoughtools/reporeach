# RepoReach Finder extension

The extension adds **Keep Downloaded**, **Free Up Space**, and **Refresh Repository** context menu actions and status badges under the root selected in RepoReach and at registered adopted-checkout paths. An action applies to the repository containing the selection; selections across different repositories intentionally have no actions. **Free Up Space** is disabled for adopted checkouts and explains that the original checkout is retained.

The host app writes the metadata-only snapshot atomically to `~/Library/Application Support/RepoReach/finder-status.json`, then posts the distributed notification `tools.enough.reporeach.statusChanged`. The schema is:

```json
{
  "mountRoot": "/Users/example/Repos",
  "virtualRoot": "/Users/example/Library/Application Support/RepoReach/native-catalogue/volume",
  "repositories": [
    { "id": "example/project", "state": "virtual", "pinned": false,
      "downloadedBytes": 4096, "error": null },
    { "id": "local/notes", "state": "available", "pinned": false,
      "localPath": "/Users/example/Source/notes", "localKind": "adopted" }
  ]
}
```

Finder does not read repository contents or run Git commands. It sends validated `reporeach://action?repo=example/project&action=keep` URLs to the host app with activation disabled. The host must revalidate each route and check repository state before acting, particularly before freeing storage.

`virtualRoot`, `localPath`, and `localKind` are optional for compatibility with older snapshots. Finder follows virtual repository links into the private volume, so the extension also observes `virtualRoot` while virtual entries exist and maps its owner/repository paths back to registered virtual entries. Adopted and materialized entries never acquire private virtual paths. The chosen and private roots must be absolute, non-root, and disjoint; traversal components and control characters are rejected.

A local path must be an absolute, non-root directory path, paired with `adopted` or `materialized`. Selection matching compares path components without resolving symlinks or accessing files, including native `.git` paths. A deeper registered checkout takes precedence over its containing checkout; equal-depth ambiguous registrations offer no actions. The extension observes the catalogue, private virtual volume, and registered physical checkout roots using only this metadata.

`downloadedBytes` defaults to zero for older snapshots. A virtual repository can hold cached file content without preparing a Git checkout or changing its state. **Free Up Space** is available for those cached bytes, prepared repositories, materialized checkouts, and kept repositories. Adopted checkouts never offer removal, and preparing or downloading states disable storage actions. These UI rules supplement the service's independent busy and recoverability checks; Finder never scans the cache itself.

Each repository can also carry an optional `operation` object with `action`, `status`, file/byte counts, and an error. The app publishes the accepted operation immediately, before repository preparation or a lock wait changes its durable state. Older snapshots without this object keep their state-based badges. Running operations take precedence over previous errors, disable conflicting actions, and show preparation/download/checkout progress in the context menu. Known Keep progress uses a circular badge in ten steps; the completed-download badge stays active while the local checkout is created. A failed operation shows an attention badge even if the repository remains available.

The extension remembers a bounded set of URLs Finder has requested badges for, removes them when their directories stop being observed, and repaints visible descendants when status changes. It does not enumerate directories to find children. The host's status task belongs to the application, independently of its windows. While repository actions are pending or running, a scoped `ProcessInfo` activity prevents App Nap from delaying background progress polling; completion, failure, service unavailability, or shutdown releases it.

For the developer beta, the extension is sandboxed and `Finder.entitlements` grants read-only access to exactly the shared metadata file in the current user's Application Support directory. There are deliberately no app-group entitlements requiring a provisioning profile. The containing app is not sandboxed. The shared cache uses the reentrant account lookup `getpwuid_r` to locate the user's host home; Foundation's named-user home API is also remapped to the private container in a sandboxed process. The lookup copies its result before releasing its bounded buffer. New cache contents are written to a 0600 temporary file and then atomically replace the old file.

The release build must validate Finder extension registration and cache access on an installed, signed app. The narrow temporary exception is documented by [Apple](https://developer.apple.com/library/archive/documentation/Miscellaneous/Reference/EntitlementKeyReference/Chapters/AppSandboxTemporaryExceptionEntitlements.html); an App Store release would need to revisit this arrangement. Users enable the extension in System Settings → Privacy & Security → Extensions → Finder Extensions (location varies by macOS version).

The extension identifier is `com.enoughtools.reporeach.finder` and its principal class is `RepoReachFinder.FinderSync`. Include both files from `native/Shared` in the Finder extension target as well as the app target. Requires macOS 13 or newer.

## Reloading an installed development build

Finder Sync runs separately from the containing app, and additional instances can serve Open and Save dialogs. Quitting RepoReach does not establish that those processes have exited. [Apple's Finder Sync guide](https://developer.apple.com/library/archive/documentation/General/Conceptual/ExtensibilityPG/Finder.html) describes this process model. The public SDK provides enablement inspection and the extension-management interface, but no programmatic restart method.

After a signed bundle update, refresh only the installed Finder extension's registration with `pluginkit -a /absolute/path/to/RepoReach.app/Contents/PlugIns/RepoReachFinder.appex`. Inspect `pluginkit -m -A -D -v -i com.enoughtools.reporeach.finder` and verify the expected installed path and existing user election. The local `pluginkit(8)` manual documents these registration operations; registration alone does not prove that an already running process loaded new code.

If an old process remains, a narrowly scoped development fallback is normal `SIGTERM` of only the captured old RepoReachFinder process IDs. Verify each process's user ID, exact executable path, and start time immediately before signalling so a reused PID or another extension cannot match. This is a process-lifecycle fallback, not a Finder Sync restart API or a guaranteed relaunch. Preserve user enablement; do not restart Finder, `pkd`, FSKit, or unrelated extensions. Let macOS load the elected extension when Finder next needs it, then verify a new process and the changed badges/actions in both the chosen folder and the resolved private virtual path. If it does not reload normally, stop and inspect the exact extension state rather than widening the restart scope.
