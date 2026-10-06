# RepoReach Finder extension

The extension adds **Keep Downloaded**, **Free Up Space**, and **Refresh Repository** context menu actions and status badges under the root selected in RepoReach and at registered adopted-checkout paths. An action applies to the repository containing the selection; selections across different repositories intentionally have no actions. **Free Up Space** is disabled for adopted checkouts and explains that the original checkout is retained.

The host app writes the metadata-only snapshot atomically to `~/Library/Application Support/RepoReach/finder-status.json`, then posts the distributed notification `tools.enough.reporeach.statusChanged`. The schema is:

```json
{
  "mountRoot": "/Users/example/Repos",
  "virtualRoot": "/Users/example/Library/Application Support/RepoReach/native-catalogue/volume",
  "repositories": [
    { "id": "example/project", "state": "ready", "pinned": false, "error": null },
    { "id": "local/notes", "state": "available", "pinned": false,
      "localPath": "/Users/example/Source/notes", "localKind": "adopted" }
  ]
}
```

Finder does not read repository contents or run Git commands. It sends validated `reporeach://action?repo=example/project&action=keep` URLs to the host app with activation disabled. The host must revalidate each route and check repository state before acting, particularly before freeing storage.

`virtualRoot`, `localPath`, and `localKind` are optional for compatibility with older snapshots. Finder follows virtual repository links into the private volume, so the extension also observes `virtualRoot` while virtual entries exist and maps its owner/repository paths back to registered virtual entries. Adopted and materialized entries never acquire private virtual paths. The chosen and private roots must be absolute, non-root, and disjoint; traversal components and control characters are rejected.

A local path must be an absolute, non-root directory path, paired with `adopted` or `materialized`. Selection matching compares path components without resolving symlinks or accessing files, including native `.git` paths. A deeper registered checkout takes precedence over its containing checkout; equal-depth ambiguous registrations offer no actions. The extension observes the catalogue, private virtual volume, and registered physical checkout roots using only this metadata.

For the developer beta, the extension is sandboxed and `Finder.entitlements` grants read-only access to exactly the shared metadata file in the current user's Application Support directory. There are deliberately no app-group entitlements requiring a provisioning profile. The containing app is not sandboxed. The shared cache locates the named user's home rather than the extension's private sandbox container. New cache contents are written to a 0600 temporary file and then atomically replace the old file.

The release build must validate Finder extension registration and cache access on an installed, signed app. The narrow temporary exception is documented by [Apple](https://developer.apple.com/library/archive/documentation/Miscellaneous/Reference/EntitlementKeyReference/Chapters/AppSandboxTemporaryExceptionEntitlements.html); an App Store release would need to revisit this arrangement. Users enable the extension in System Settings → Privacy & Security → Extensions → Finder Extensions (location varies by macOS version).

The extension identifier is `com.enoughtools.reporeach.finder` and its principal class is `RepoReachFinder.FinderSync`. Include both files from `native/Shared` in the Finder extension target as well as the app target. Requires macOS 13 or newer.
