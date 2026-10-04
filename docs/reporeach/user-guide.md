# Using the RepoReach beta

RepoReach shows selected Git repositories under a folder you choose. GitHub sign-in is optional: add a Git remote or existing local checkout directly, or connect GitHub to discover accessible repositories.

```text
Repositories/
  your-account/
    personal-project/
  your-organization/
    shared-project/
```

For GitHub discovery, owner folders come from repository ownership, so organizations appear alongside personal accounts. Access is limited to repositories returned to the authorized GitHub credential. Organization approval, SSO, or token restrictions can make the list smaller than the one visible on GitHub's website. Manually added sources join the catalogue under their owner/repository labels.

## Set up

1. Install the build for your Mac's architecture. The beta targets macOS 13 or later. Consult the release manifest for whether the download has a Developer ID signature and Apple notarization; an ad-hoc development build does not provide those assurances.
2. Install [macFUSE](https://macfuse.io/) separately and complete its kernel backend setup. The current beta uses the kernel backend; an FSKit-only setup is insufficient. Apple Silicon setup can require Recovery and Reduced Security. RepoReach does not bundle or install a filesystem driver.
3. Open RepoReach. Add a repository from its Git remote or local path, or optionally sign in to GitHub for discovery. GitHub sign-in uses the official GitHub CLI and opens GitHub's browser authorization flow.
4. Choose a dedicated empty folder, then connect it. The default is `~/Repositories`. Existing files must not be hidden by the mount; a nonempty folder is refused.
5. Enable the RepoReach Finder extension if you want Finder badges and context-menu actions. Use the app's extension-settings shortcut; the location of Finder extension settings varies by macOS version.

The chosen folder is a FUSE mount, not an Apple File Provider location. It can be placed outside `~/Library/CloudStorage`. For beta testing, use a dedicated folder on local internal storage; other location types have not been validated. A folder and the app's private state directory must remain separate. Changing the folder temporarily disconnects the catalogue and updates managed Git worktree locations. Read [macOS setup](platform-setup.md) for driver approval, opening an unnotarized beta, and Finder extension settings for your macOS version.

## Add a Git source without signing in

Choose **Add Repository**, paste a Git remote or use **Choose Existing Folder**, then select **Adopt Repository**. Expand **Optional details** to set an owner label, repository name, or branch.

Add an HTTPS or SSH Git remote, an SSH address such as `git@example.com:team/project.git`, a local bare repository, or an absolute path to an existing checkout. Standard HTTP, `git://`, and local `file://` sources are also accepted. Owner/group and repository names can be inferred from the source or supplied explicitly; local paths default to the `local` group. Choose an original Git folder outside RepoReach's mount and private storage folders. Manual sources use your native Git credential helpers or SSH setup; configure access to a private server in Git before using it in RepoReach. Do not put credentials in an HTTP URL: those URLs are refused.

Adding a source registers metadata without cloning its file contents. Preparation is deferred until you enter it or use Prepare or Keep Downloaded. By default, remote sources use the advertised default branch, and an existing local checkout supplies the branch of its committed `HEAD`. Preparation acquires that branch's committed state at the time it runs. An optional branch override is available for advanced use. A source without a usable default branch may need an explicit branch; an empty repository needs committed data before it can be added.

An existing checkout is a source for a **separate virtual checkout**. Adding and preparing it does not move or replace the original folder, reset its index, or import its staged changes, uncommitted edits, or untracked files. Committed history reachable from the selected source ref can be acquired even if it has not been pushed elsewhere. Keep using the original folder for work that has not been committed there. This feature does not migrate dirty work or back up the source folder.

Refresh reads from the chosen source. A local path remains a local Git source; adding it does not automatically switch the new checkout's upstream to the original folder's cloud remote. Explicit Git pushes follow the configured remote and its normal permissions and receive policy.

## Choose what appears

Use **Show repository in Finder** in a repository's details or **Owners & organizations** in Settings to choose which entries appear in the virtual catalogue. The sidebar lists **Repository groups**, with a **Show group in Finder** control for the selected group. Turning a repository or owner off hides its folders, blocks new preparation through the catalogue, and pauses its background pin downloads. It retains the managed clone, cache, overlay, pin intent, and other local data. Use Free Up Space separately when you want to reclaim storage.

Per-repository disabled choices survive GitHub rediscovery. Turning an owner back on re-exposes eligible repos while leaving individually disabled ones hidden. Owner controls also apply to manually added repos with that owner label.

A switch change is refused while an affected explicit operation or lazy activation is busy. Cancel the operation or wait and retry. Existing open files can continue to work and download missing contents after their entry is hidden. Visibility controls are not an access-revocation mechanism or an offline/network firewall.

## Browse and work

Listing the catalogue and owner folders uses saved metadata and does not clone every repository. Entering a repository prepares a blobless Git clone and a committed file-tree index. Reading a file downloads its contents when they are missing from the local cache. Preview tools, editors, searches, and build tools can read files too, so their access can trigger downloads.

On a Git server or local transport that does not honor partial-clone filters, preparation can transfer more Git objects than it does on a supporting server. The visible tree still uses the managed filesystem and local overlay.

The mounted repository is writable. Local file changes live in ArtifactFS's overlay until ordinary Git operations record them. A synthesized `.git` file points to the real Git directory in the app's state folder. You can use normal Git commands, including staging and committing, within the mounted repository. Configure your Git author name and email as you would for any checkout. The watcher updates the virtual base when local `HEAD` changes and reconciles the overlay.

Root, owner, and repository-folder browsing leaves repositories dormant. Inside a prepared repository, checking a file's attributes can download its contents when Git does not know the blob size. Finder previews, indexing, and file metadata requests can therefore trigger downloads before you explicitly open a file. This beta does not provide metadata-only browsing for every file.

Changes are not automatically committed or pushed. Use Git to publish work. “Sync” or Refresh refers to acquiring Git data from the source, not uploading each saved file. This beta does not provide a complete replacement for a Git client, nor a backup of every file in your development environment.

## Repository actions

| Action | Beta behavior |
| --- | --- |
| Discover repositories | Requests the accessible repository list from GitHub and updates the catalogue. Previously known entries missing from the result are retained with a warning so local data is not orphaned. |
| Add repository | Registers a manual Git source without requiring GitHub sign-in. Clone preparation waits until entry or a preparation/download action. |
| Repository / owner switch | Changes catalogue visibility and background pin eligibility without deleting data. Individual exclusions remain when an owner is re-enabled. |
| Open / prepare | Acquires Git metadata as needed from the selected source/ref. Reopening an already prepared repo preserves its local branch and index. |
| Refresh Repository | Explicitly requests a remote fetch. Use ordinary Git operations to merge, rebase, or update the working branch as appropriate. |
| Keep Downloaded | Downloads and verifies the unique Git blobs in the current committed tree. Completion is recorded only after checking that `HEAD` did not change during the download. |
| Cancel | Stops an active repository operation. Already prepared data and downloaded cache contents remain available for retry. |
| Free Up Space | Checks for locally valuable state before removing the repository's engine-owned clone, overlay, tree metadata, and blob cache. The discovered repo entry remains in the catalogue. |

**Keep Downloaded covers the current committed tree.** It does not fetch every historical file version, LFS object, or submodule repository. Commands that need an uncached historical blob can still need access to the source. Submodules and `.gitattributes` checkout filters such as Git LFS are explicitly refused for offline pinning. The beta does not implement LFS object hydration; Git pointer files are not a promise that the corresponding payload is available offline.

Pin intent is saved. While the service runs, it checks visible enabled pinned repositories for a changed local `HEAD` and downloads the new committed tree. Disabled entries pause that background work. After a restart, prepared repositories become available while pin completeness is verified again. A kept repo can therefore temporarily need attention or access to its source after changing branches or restarting with incomplete cache data.

## When Free Up Space is refused

Free Up Space is deliberately conservative. The implementation checks overlay files and deletions, staged changes, local refs and reflogs, unreachable Git objects, locks and in-progress operations, custom local Git configuration and hooks, local ignore and attribute rules, and ownership of storage paths. A fresh remote fetch must establish that the checked local history is reachable from remote branches or tags. A failed or canceled verification retains repository data.

Commit and push work you want to preserve, or archive it yourself, then retry. The beta does not silently upload ignored files, force a push, or discard local work to satisfy this action. A network failure can also prevent freeing a repository because recoverability cannot be verified.

The catalogue is temporarily unmounted during the safety check, including when a request is refused, then remounted if it was previously connected. Open editors or terminals may need to reopen their folder. A busy unmount is refused rather than forced.

Successful removal also discards the managed clone's local configuration and cached data. Keep independent backups for anything that is not reconstructable from the remote. These checks cover specified storage and Git states; they are not a guarantee against all external programs, concurrent writes, or filesystem failures.

## Lifecycle and limits

Closing the management window leaves RepoReach and its service running, with no menu-bar item. Quitting the app stops its owned service; disconnected virtual files are not ordinary stored files. Reopen RepoReach to restore the connection. Launch at login is optional and may require approval in macOS settings.

Automatic discovery currently targets `github.com` and one selected GitHub CLI account. Manual sources can use other Git hosts or local repositories, with native Git/SSH authentication. The beta does not expose a GitHub Enterprise discovery selector, restore removed repositories from an independent cloud backup, or create repos by writing owner-level catalogue folders. Cross-repository moves and filesystem extended attributes are not implemented by the catalogue layer. Test your editor and build workflow before relying on the beta for important work.

## Upgrading from beta.1

The current catalogue uses schema version 2 to preserve manual sources and visibility settings. An existing version 1 catalogue is read and upgraded automatically on save, without deleting repository storage or source folders.

Older betas cannot read the migrated catalogue. Preserve the app state rather than deleting it to make a downgrade launch. Before upgrading, stop RepoReach and its service if you want a consistent backup of `~/Library/Application Support/RepoReach`. Returning to an older beta requires a compatible pre-upgrade backup or a separate manual recovery/import process; first preserve any work added since that backup. There is no automatic downgrade conversion.

For credential and data details, read [Privacy and authentication](privacy-auth.md). For source builds, read [Building and releasing](releasing.md).
