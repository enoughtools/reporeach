# RepoReach product contract

Cold browsing is the main product requirement. RepoReach should make repositories
visible in the user's chosen folder without downloading their file contents.
Release work is deferred until the local experience meets these requirements.

## Required behavior

1. **Chosen location and mixed local storage.** The catalogue belongs in the
   folder the user chooses. Ordinary local checkouts and virtual repositories
   must coexist without hiding existing files beneath a mount.
2. **Cold catalogue browsing first.** Root and organization listings use saved
   metadata. Dormant repository lookups acquire authoritative tree metadata
   separately from writable checkout preparation. Finder metadata probes must
   not prepare every listed repo. Cached complete trees remain browsable offline;
   failed discovery must not become invented empty directories or conceal
   legitimate committed files.
3. **Content on demand.** Names, types, and available sizes must be usable without
   fetching file bodies. Reading content may hydrate blobs. Unknown sizes remain
   unknown until a caller requires an exact size. A remote that ignores Git's
   partial-clone filter has a capability limit.
4. **Adopt an existing checkout.** Adoption preserves its original location,
   index, staged and unstaged changes, untracked files, branches, and configuration.
   Adding a remote as a new virtual repo is a separate operation. Neither needs
   GitHub sign-in.
5. **Keep locally.** Keep Downloaded creates an ordinary checkout at its catalogue
   path, accessible after RepoReach quits. Conversion preserves the existing
   private Git directory and current visible files without resetting or publishing
   work. Complete offline history, LFS, and submodule availability require explicit
   support.
6. **Free space without losing work.** Returning an app-created checkout to a
   virtual entry requires proof that its data is recoverable. Unpushed history,
   edits, untracked or ignored files, local metadata, and active access can prevent
   removal. RepoReach never removes an adopted original.
7. **Independent visibility controls.** Organization and repository choices
   persist independently. Hiding a group retains each repo's choice and local
   data. Ordinary directories stay on disk; hiding changes only owned catalogue
   links and virtual access. Manual sources use native Git authentication
   independently of optional GitHub discovery.

## Native development implementation

The selected root and organization folders are ordinary host directories. Virtual
repos are app-owned symbolic links into one hidden FSKit volume in private app
storage. Adopted external checkouts receive direct links to their original paths.
Kept checkouts are ordinary directories at their catalogue paths. Publication
refuses existing directories and foreign links. This design retains a single
filesystem volume; it does not mount a volume over the entire chosen folder or
create a mount for every repo.

ArtifactFS remains the Git, snapshot, overlay, hydration, and writable engine.
FSKit replaces its FUSE transport and is bundled for macOS 26 or later. Virtual
links require the filesystem service; ordinary adopted and kept checkouts remain
available independently of it.

GitHub roots are acquired in bounded GraphQL batches; deeper trees load lazily by
immutable object identity. Manual sources use separate shallow filtered Git
previews. Browsing a preview neither prepares the writable engine nor alters a
source checkout. If Git cannot supply a blob size locally, cheap metadata access
omits it; a request requiring an exact size can activate content acquisition.
Content reads promote a preview to writable storage at its selected commit.

Explicit Refresh on an unprepared virtual repo quiesces the catalogue and replaces
its preview baseline. Refresh on a prepared repo fetches Git data while preserving
its persistent visible baseline. Background HEAD watching and remote refresh
remain disabled under the native policy; this is not live working-tree sync. See
[native cache coherence](native-fskit.md#cache-coherence-is-a-release-gate).
Refresh on a local checkout fetches its own remotes without resetting files or
index.

Keep stages and verifies a standalone checkout, drains the filesystem, and
publishes through a durable handoff journal. Former private engine storage stays
as a verified rollback copy until successful Free cleanup. Free verifies local
state and remote recovery, moves the owned checkout aside, publishes its virtual
link, then removes verified owned copies. Startup recovery completes or rolls
back interrupted handoffs. Changed files or uncertain ownership retain data and
produce an error; incomplete cleanup must not be reported as reclaimed space.
An interrupted Keep's retained standalone checkout and its path remain in status
after restart. Free cleanup records progress per owned entry so an interrupted
deletion can resume without requiring already removed files to reappear.

## Acceptance status

The primary disposable mounted sequence passed in 6.94 seconds on macOS 27.0.1
ARM64 using the signed local8 native module and a development engine based on
`aedad10`.
It covers ordinary-folder placement, adoption, dirty working files and Git-index
preservation through Keep, local metadata preservation, and ordinary checkout
access with the app stopped.

The complete cold storage fixture passed in 6.83 seconds with that same installed
module and the updated development engine. Initial preview acquisition took
211 ms, cached preview listing 3 ms, and prepared names-only listing 4 ms. The
names-only listing made zero source requests, left all five blobs missing, and
kept the blob cache empty. Keep verified five unique blobs totaling 107 bytes;
the ordinary checkout worked with the app stopped and zero offline requests.
Free correctly refused local extended metadata, then reclaimed the clean checkout
and rollback storage before successful preparation and reacquisition. These are
disposable fixture results, not Finder navigation timings.

Actual cold Finder navigation and the signed combined local9 build remain pending
installation and testing. See the [mounted acceptance record](fskit-acceptance.md)
for the precise tested scope.

Source tests do not establish that Finder's cold experience is fixed. Validate the
signed local build using disposable fixtures and record source requests, prepared
repos, hydrated blobs, and elapsed time separately for root/org browsing, entering
one repo, traversing subdirectories, and reading one file. Include real committed
Finder metadata filenames and the synthetic `.git` entry so shortcuts cannot pass
by hiding them.

Further acceptance must verify Finder navigation and repeat the relevant storage,
restart recovery, and busy-operation checks for the final combined build.
Do not use the user's live checkout as a conversion experiment.

Historical beta.3 downloads retain their macFUSE transport, separate-clone
adoption, and cache-based Keep behavior. Native development changes do not alter
those releases. Incremental local app builds precede release packaging,
notarization, and deployment.
