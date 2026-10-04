# Using the RepoReach beta

RepoReach shows your accessible GitHub repositories under a folder you choose:

```text
Repositories/
  your-account/
    personal-project/
  your-organization/
    shared-project/
```

The owner folders come from repository ownership, so organizations appear alongside personal accounts. Access is limited to repositories returned to the authorized GitHub credential. Organization approval, SSO, or token restrictions can make the list smaller than the one visible on GitHub's website.

## Set up

1. Install the build for your Mac's architecture. The beta targets macOS 13 or later. Consult the release manifest for whether the download has a Developer ID signature and Apple notarization; an ad-hoc development build does not provide those assurances.
2. Install [macFUSE](https://macfuse.io/) separately and complete its kernel backend setup. The current beta uses the kernel backend; an FSKit-only setup is insufficient. Apple Silicon setup can require Recovery and Reduced Security. RepoReach does not bundle or install a filesystem driver.
3. Open RepoReach and sign in to GitHub. The app uses the official GitHub CLI and opens GitHub's browser authorization flow.
4. Choose a dedicated empty folder, then connect it. The default is `~/Repositories`. Existing files must not be hidden by the mount; a nonempty folder is refused.
5. Enable the RepoReach Finder extension if you want Finder badges and context-menu actions. Use the app's extension-settings shortcut; the location of Finder extension settings varies by macOS version.

The chosen folder is a FUSE mount, not an Apple File Provider location. It can be placed outside `~/Library/CloudStorage`. For the first beta, use a dedicated folder on local internal storage; other location types have not been validated. A folder and the app's private state directory must remain separate. Changing the folder temporarily disconnects the catalogue and updates managed Git worktree locations. Read [macOS setup](platform-setup.md) for driver approval, opening an unnotarized beta, and Finder extension settings for your macOS version.

## Browse and work

Listing the catalogue and owner folders uses saved metadata and does not clone every repository. Entering a repository prepares a blobless Git clone and a committed file-tree index. Reading a file downloads its contents when they are missing from the local cache. Preview tools, editors, searches, and build tools can read files too, so their access can trigger downloads.

The mounted repository is writable. Local file changes live in ArtifactFS's overlay until ordinary Git operations record them. A synthesized `.git` file points to the real Git directory in the app's state folder. You can use normal Git commands, including staging and committing, within the mounted repository. Configure your Git author name and email as you would for any checkout. The watcher updates the virtual base when local `HEAD` changes and reconciles the overlay.

Root, owner, and repository-folder browsing leaves repositories dormant. Inside a prepared repository, checking a file's attributes can download its contents when Git does not know the blob size. Finder previews, indexing, and file metadata requests can therefore trigger downloads before you explicitly open a file. This beta does not provide metadata-only browsing for every file.

Changes are not automatically committed or pushed. Use Git to publish work. This beta does not provide a complete replacement for a Git client, nor a backup of every file in your development environment.

## Repository actions

| Action | Beta behavior |
| --- | --- |
| Discover repositories | Requests the accessible repository list from GitHub and updates the catalogue. Previously known entries missing from the result are retained with a warning so local data is not orphaned. |
| Open / prepare | Acquires the repository's Git metadata as needed. The initial checkout uses its default branch; reopening an already prepared repo preserves its local branch and index. |
| Refresh Repository | Explicitly requests a remote fetch. Use ordinary Git operations to merge, rebase, or update the working branch as appropriate. |
| Keep Downloaded | Downloads and verifies the unique Git blobs in the current committed tree. Completion is recorded only after checking that `HEAD` did not change during the download. |
| Cancel | Stops an active repository operation. Already prepared data and downloaded cache contents remain available for retry. |
| Free Up Space | Checks for locally valuable state before removing the repository's engine-owned clone, overlay, tree metadata, and blob cache. The discovered repo entry remains in the catalogue. |

**Keep Downloaded covers the current committed tree.** It does not fetch every historical file version, LFS object, or submodule repository. Commands that need an uncached historical blob can still need a network connection. Submodule offline downloads are explicitly refused. LFS files may remain Git pointer files because the beta does not implement LFS object hydration.

Pin intent is saved. While the service runs, it checks pinned repositories for a changed local `HEAD` and downloads the new committed tree. After a restart, prepared repositories become available while pin completeness is verified again. A kept repo can therefore temporarily need attention or a network connection after changing branches or restarting with incomplete cache data.

## When Free Up Space is refused

Free Up Space is deliberately conservative. The implementation checks overlay files and deletions, staged changes, local refs and reflogs, unreachable Git objects, locks and in-progress operations, custom local Git configuration and hooks, local ignore and attribute rules, and ownership of storage paths. A fresh remote fetch must establish that the checked local history is reachable from remote branches or tags. A failed or canceled verification retains repository data.

Commit and push work you want to preserve, or archive it yourself, then retry. The beta does not silently upload ignored files, force a push, or discard local work to satisfy this action. A network failure can also prevent freeing a repository because recoverability cannot be verified.

The catalogue is temporarily unmounted during the safety check, including when a request is refused, then remounted if it was previously connected. Open editors or terminals may need to reopen their folder. A busy unmount is refused rather than forced.

Successful removal also discards the managed clone's local configuration and cached data. Keep independent backups for anything that is not reconstructable from the remote. These checks cover specified storage and Git states; they are not a guarantee against all external programs, concurrent writes, or filesystem failures.

## Lifecycle and limits

Closing the management window leaves RepoReach and its service running, with no menu-bar item. Quitting the app stops its owned service; disconnected virtual files are not ordinary stored files. Reopen RepoReach to restore the connection. Launch at login is optional and may require approval in macOS settings.

The desktop beta currently targets `github.com` and one selected GitHub CLI account. It does not expose a GitHub Enterprise host selector, restore removed repositories from an independent cloud backup, or create new repos by writing owner-level catalogue folders. Cross-repository moves and filesystem extended attributes are not implemented by the catalogue layer. Test your editor and build workflow before relying on the beta for important work.

For credential and data details, read [Privacy and authentication](privacy-auth.md). For source builds, read [Building and releasing](releasing.md).
