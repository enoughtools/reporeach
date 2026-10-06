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
omits it. Read-only content access hydrates the selected immutable blob through
a separate shallow Git source into the canonical cache shared with the writable
engine, without preparing that engine. Cached blobs remain readable offline.
A request requiring an exact unknown size can fetch that selected blob, including
size requests issued by a native POSIX open. Ordinary read-only opens defer
content acquisition where an exact size is not requested. Git access through
the synthetic `.git` file, writes, and explicit preparation promote the preview
to writable storage at its selected commit.

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
For repos with only preview content, Free can reclaim verified immutable content
and its shallow source without preparing a writable checkout, retaining browsing
metadata. Ownership receipts and journals also protect that cleanup.

## Acceptance status

The installed signed local9 build at `7280ba1` passed both disposable mounted
sequences on macOS 27.0.1 ARM64: primary in 6.84 seconds and cold storage in
6.43 seconds. It proved ordinary-folder placement, dirty adoption and native Git
state preservation, Keep, app-off offline access, metadata-aware Free refusal,
clean full cleanup, and reacquisition. Its initial preview took 226 ms, cached
listing 3 ms, and prepared names-only listing 5 ms. Listing left all five fixture
blobs missing with no source HTTP requests or blob-cache content. These fixture
timings do not measure Finder navigation or real GitHub network latency.

Actual Finder checks on local10 verified traversal of the ordinary root and
GitHub preview directories without permission badges. Missing Finder actions
were traced to status-cache location and resolved private-volume paths, with
corrections in local11. The extended local11 mounted sequence found a symlink
mode regression and clarified that native opens can request exact unknown sizes;
those fixes and the full `a6fc122` read-only content path require local12
qualification. No final Finder latency or release-readiness claim is made. See
the [mounted acceptance record](fskit-acceptance.md) for tested scope.

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
