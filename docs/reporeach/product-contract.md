# EnoughRepos product contract

Cold browsing is the main product requirement. EnoughRepos should make repositories
visible in the user's chosen folder without downloading their file contents.
These requirements guide implementation and release qualification.

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
   path, accessible after EnoughRepos quits. Conversion preserves the existing
   private Git directory and current visible files without resetting or publishing
   work. Complete offline history, LFS, and submodule availability require explicit
   support.
6. **Free space without losing work.** Returning an app-created checkout to a
   virtual entry requires proof that its data is recoverable. Unpushed history,
   edits, untracked or ignored files, local metadata, and active access can prevent
   removal. EnoughRepos never removes an adopted original.
7. **Independent visibility controls.** Organization and repository choices
   persist independently. Hiding a group retains each repo's choice and local
   data. Ordinary directories stay on disk; hiding changes only owned catalogue
   links and virtual access. Manual sources use native Git authentication
   independently of optional GitHub discovery.
8. **Recover owned virtual sessions explicitly.** Recovery can reclaim an
   ownerless stale native session only with a matching durable ownership record.
   It must use normal disconnection and verify absence before creating a fresh
   session. Busy access, changed identities, or uncertain ownership retain data
   and report an actionable error. Adopted ordinary checkouts are outside this
   flow; older unrecorded sessions confer no automatic recovery authority.

## Native implementation

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
publishes through a durable handoff journal. Current-tree hydration batches
missing selected blobs before offline cache extraction. Export binds cached
preview identities to the frozen writable runtime before taking inventories,
so entering an unvisited directory cannot change the view halfway through Keep.
Native enumeration retires completed directory sessions before reaching its
bounded session limit, preserving active partial pages and reference recovery.
Export file descriptors acquire close-on-exec atomically. Former engine storage
stays as a verified rollback copy until successful Free cleanup. Free verifies
local state and remote recovery, moves the owned checkout aside, publishes its virtual
link, then removes verified owned copies. Startup recovery completes or rolls
back interrupted handoffs. Changed files or uncertain ownership retain data and
produce an error; incomplete cleanup must not be reported as reclaimed space.
An interrupted Keep's retained standalone checkout and its path remain in status
after restart. Free cleanup records progress per owned entry so an interrupted
deletion can resume without requiring already removed files to reappear.
For repos with only preview content, Free can reclaim verified immutable content
and its shallow source without preparing a writable checkout, retaining browsing
metadata. Ownership receipts and journals also protect that cleanup.

## Acceptance and qualification

Release validation must cover ordinary-folder placement, dirty adoption and
preserved Git state, metadata-only cold listings, selected-blob hydration without
writable preparation, Keep across unvisited descendants, offline app-off access,
conservative Free refusal, clean reclamation, and reacquisition. Installed Finder
checks must establish actual action dispatch and status updates, beyond menu
visibility or unit tests. Recovery checks must retain data and report busy or
uncertain ownership without forced disconnection.

The [mounted acceptance record](fskit-acceptance.md) preserves results for exact
source revisions, app builds, and platforms. Historical fixture passes and cached
Finder observations do not qualify a later release candidate or measure cold
GitHub network performance. Each candidate needs its own package, signing,
installed-module, and runtime evidence. Historical beta.3 downloads retain their
macFUSE transport, separate-clone adoption, and cache-based Keep behavior;
subsequent native changes do not alter those downloads.
