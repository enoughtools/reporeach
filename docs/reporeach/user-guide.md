# Using RepoReach

RepoReach groups repositories under a folder you choose. GitHub sign-in is
optional: add a remote, adopt an existing checkout, or connect GitHub to discover
accessible repositories.

```text
Repositories/
  your-account/
    personal-project/
  your-organization/
    shared-project/
```

This guide describes the signed local12 development build. Mounted storage and
actual Finder checks passed on macOS 27.0.1 ARM64, including nested traversal,
selected-file previews without writable preparation, and context-menu actions.
Real-network cold Finder latency and release readiness remain unqualified; see the
[acceptance record](fskit-acceptance.md). Historical beta.3 downloads retain their
macFUSE installation, separate-clone adoption, and cache-based Keep behavior.
Development changes do not alter those releases or establish release readiness.

## Set up the native build

1. Use the native build for macOS 26 or later. Its FSKit extension is bundled;
   no separate macFUSE installation is required. Consult the build manifest for
   signing and notarization status.
2. Open RepoReach and enable its File System Extension in macOS settings when
   requested. Enable the Finder extension separately for badges and actions.
3. Choose your repository folder, then **Enable Virtual Folders**. Existing files
   can stay there: the root and organization folders are ordinary directories.
   A conflicting repo name is refused rather than replacing a folder or foreign
   link. Keep the chosen folder separate from private app storage.
4. Add a repo or optionally connect GitHub through its official CLI and browser
   authorization flow.

Virtual repos link into a hidden app-managed filesystem. Adopted external
checkouts link to their original folders. **Keep Downloaded** replaces a virtual
entry with an ordinary local checkout. This supports mixed local and virtual
repos without mounting over the entire chosen folder. Locations other than
local internal storage still need testing. See [Native FSKit](native-fskit.md)
for development requirements and proof status.

## Add or adopt without signing in

Choose **Add Repository**, paste a remote or use **Choose Existing Folder**, then
select **Adopt Repository**. Optional details set owner and repo labels or a
branch for a virtual source. Local sources default to the `local` group.

HTTPS, SSH, SSH-style addresses such as `git@example.com:team/project.git`,
`git://`, absolute local paths, and local `file://` sources are accepted. Manual
sources use native Git credential helpers or SSH configuration. Configure private
server access in Git first. Credentials embedded in HTTP URLs are refused.

An existing nonbare checkout is adopted in place. Its branch or detached HEAD,
staged and unstaged changes, untracked files, index, and configuration are retained.
RepoReach neither moves it nor creates a separate managed clone. A checkout
already inside the chosen catalogue stays there; an external one receives a direct
catalogue link. Adopted originals work after app quit and cannot be removed by
Free Up Space. Change their branches through normal Git operations.

A remote or local bare repo becomes a virtual entry. Native Git inspects the
source branch; an explicit branch can be supplied when needed. Empty repos need
committed data. Browsing and preparation do not change local source files or
index. Legacy entries retain their separate virtual checkout on upgrade rather
than silently becoming adopted originals.

## Choose what appears

Use **Show in catalogue** on a repo, **Owners & organizations** in Settings, or
**Show group in catalogue** for the selected group. Group and repo choices persist
independently: enabling a group leaves individually disabled repos hidden.
Rediscovery retains those choices and manual sources. GitHub organization approval,
SSO, and token limits can restrict the discovered list.

Hiding removes an owned link or virtual visibility and pauses pin downloads.
Local data is retained. An ordinary checkout at its physical path remains visible
there, even if its group is hidden; this control does not remove real directories.
Use Free Up Space separately for eligible app-created checkouts. A busy change
can be refused; close active files or wait and retry. Visibility does not revoke
credentials or prevent all existing processes from accessing data.

## Browse and work

Root and organization listings use saved metadata. Within a dormant GitHub repo,
RepoReach acquires tree metadata in batches and loads deeper trees as needed,
without preparing every writable checkout or downloading file bodies. Complete
cached trees are available offline. Manual sources use separate shallow filtered
Git previews. A server ignoring filters can transfer more objects; an exact-size
request can fetch the selected file when Git does not know its size locally.
Native macOS opens can issue such size requests even before you read the file.

Reading a file downloads its selected immutable contents through a separate
shallow Git source and shared cache, without preparing a writable checkout.
Cached contents work offline. Finder thumbnails, editors, searches, and build
tools can trigger these downloads too. A read-only open defers contents unless
an exact unknown size is required. Git commands, writes, and explicit preparation
create the writable checkout at the selected preview commit. Listing the synthetic
`.git` entry needs no preparation; opening it prepares the real Git directory.

Virtual files are writable through ArtifactFS's overlay. Normal Git commands can
stage and commit through the `.git` pointer. Configure your author identity as for
any checkout. The native virtual view currently retains a persistent baseline:
background HEAD watching and remote refresh are disabled, and fetching commits
does not automatically rebuild the visible tree. Test branch changes and your
editor/build workflow before relying on it for important work.

Adopted and kept checkouts are ordinary Git folders; files, index, and branch
changes are independent of the virtual service. RepoReach does not automatically
commit or push changes in either kind of repo.

## Repository actions

| Action | Native development behavior |
| --- | --- |
| Discover repositories | Updates GitHub entries; missing known entries and local data are retained. |
| Add repository | Registers remote/bare sources virtually or adopts existing checkouts in place without GitHub sign-in. |
| Repo / owner switch | Changes virtual or owned-link visibility and pin eligibility; retains directories and data. |
| Open / prepare | Opens local checkouts directly; acquires virtual metadata or writable storage as needed. |
| Refresh Repository | Replaces an unprepared preview after safe disconnection; fetches prepared Git data without resetting its visible baseline. Local checkouts fetch their own remotes. |
| Keep Downloaded | Hydrates the current tree and safely publishes a local checkout with current files and existing Git state. |
| Cancel | Stops the active operation; completed cache and necessary recovery data remain. |
| Free Up Space | Reclaims verified preview content, or returns app-created checkouts/managed virtual storage to on-demand entries after recovery checks. Adopted originals cannot be freed. |

Keep preserves staged, unstaged, and untracked work in the virtual view without
resetting or publishing it. It copies the existing Git directory, index, and refs,
and verifies file data and supported local metadata. A verified private rollback
copy remains until successful Free cleanup, so conversion uses additional space.

**Keep covers the current checkout, not all offline history.** Uncached historical
blobs may still need the remote. Submodules and filters such as Git LFS are refused;
a pointer file does not mean its payload is available. Enabled pinned virtual
entries receive current-tree availability checks while the service runs. Ordinary
local files do not depend on that pin loop.

## When Free Up Space is refused

Free can refuse changed/staged/untracked/ignored files or folders, unpushed refs
or reflog history, unreachable objects, custom Git configuration or hooks, shared
Git storage, extended metadata such as Finder tags or resource forks, and active
file access. A fresh remote check must prove recoverability. Failed or canceled
checks retain the checkout; network failure can prevent freeing it.

Preserve local data and publish work you intend to recover before retrying.
RepoReach does not upload ignored files, force a push, or discard work to make Free
succeed. Adopted originals are always retained; manage their space through your
normal backup and Git workflow.

Virtual access temporarily disconnects during handoffs and checks. Busy unmounts
are refused rather than forced. Journals let startup finish cleanup or restore the
previous state after interruption. Uncertain ownership, changed files, or failed
cleanup retain data and report recovery needs; incomplete cleanup is not reported
as fully reclaimed space. An interrupted Keep's retained standalone checkout path
remains in status after restart, and partial Free cleanup can resume. Preserve
those recovery folders until recovery succeeds. Keep independent backups for
important work.

## Lifecycle and upgrades

Closing the window leaves RepoReach running without a menu-bar item. **Pause
Virtual Folders** disconnects virtual access. Quitting stops the owned service;
reopen the app to restore virtual links. Ordinary adopted and kept checkouts remain
usable. Launch at login is optional.

Discovery targets `github.com` and one selected GitHub CLI account. Manual sources
can use other hosts with native Git auth. There is no GitHub Enterprise discovery
selector, independent cloud backup restoration, or remote repo creation by writing
owner folders. Cross-repository filesystem moves are refused.

Schema 3 records ordinary checkout ownership. Schemas 1 and 2 are accepted and
upgraded on save without deleting sources or managed data. Older builds may not
read upgraded state. Stop the app and service for a consistent backup of
`~/Library/Application Support/RepoReach` before upgrading. Downgrading needs
compatible saved state or manual recovery, with newer work preserved first; no
automatic downgrade conversion exists.

Read [Privacy and authentication](privacy-auth.md) for credential/data details
and [Building and releasing](releasing.md) for builds.
