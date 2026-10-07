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
publishes through a durable handoff journal. Current-tree hydration batches
missing selected blobs before offline cache extraction. Export binds cached
preview identities to the frozen writable runtime before taking inventories,
so entering an unvisited directory cannot change the view halfway through Keep.
Native enumeration retires completed directory sessions before reaching its
bounded session limit, preserving active partial pages and reference recovery.
Export file descriptors acquire close-on-exec atomically. Former engine storage
stays
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

The installed signed local15 build uses clean executable source
`de3610c581d32bfbd89039bfdd0c5d3e1c4701c8`. Ordered CLI build, vet, and full Go
tests passed in 8.547, 2.305, and 159.504 seconds respectively. Three mounted
fixtures passed on macOS 27.0.1 ARM64: expanded dormant-preview Keep in
12.26 seconds, primary in 7.96 seconds, and cold storage in 7.67 seconds. They
verified ordinary-folder placement, dirty adoption and native Git state, Keep, app-off
access, safe Free refusal, full clean cleanup, and reacquisition.

The dormant Keep fixture started with cached file/directory attributes and
unvisited nested content across 160 descendant directories. Accepted-operation
and blocked-source progress status each rounded to 0 ms. Keep materialized
207 regular files and one symlink, representing 208 unique blobs and 4,341 bytes,
into an ordinary checkout readable with the app stopped and zero source requests.
This expanded disposable regression exceeds the native directory-session budget
and verifies repeated export inventories.

A bulk Git fixture also acquired 188 selected blobs in two HTTP requests and
extracted cached contents offline without changing native Git state.

The cold fixture verifies metadata-only listing, selected 20-byte read-only
hydration without writable preparation, five unique blobs totaling 107 bytes
through Keep, zero-request offline reads, metadata-aware Free refusal, clean
Free, and reacquisition. These loopback fixtures do not measure Finder navigation
or GitHub network latency.

Actual Finder checks on local12 opened an ordinary root with 14 organization
folders, a group of four repos, a 49-entry repository, and nested directories
without permission alerts or red badges. The selected repository's tree metadata
was already cached. Root/org navigation left its content cache empty; entering
the repo caused thumbnails to acquire four selected image blobs totaling
18,741 bytes. It remained virtual without preparing a writable checkout, and
the total engine registrations stayed at 114 throughout. Keep and Refresh actions on its public catalogue path were enabled while Free was disabled with no cached content;
after thumbnail downloads, nested context menus enabled Keep, Free, and Refresh.
No conversion action was invoked on the user's repository.

These observations establish traversal, action availability, and selected-thumbnail
hydration on local12. Local15 Finder callbacks now use fresh selected/targeted
URLs, recheck action eligibility, and explicitly dispatch to the containing app.
Live Keep through the management app also completed for an existing repository,
with an observed accepted-to-complete interval of 10.113 seconds. Its 188 blobs
totaling 2,491,698 bytes were already cached. The result is an ordinary checkout
with its own `.git` directory, clean native Git status, Finder's folder/kept
presentation, and the app's local-checkout state. After normal app shutdown,
the app and engine were absent; native Git still reported clean status and could
read HEAD and the README blob with lazy fetching disabled. Complete cached mount
inspections after the fixtures and app shutdown found no RepoReach volumes.
This is a conversion/app-off observation, not network download timing.

The actual Finder Keep callback reaches its handler but local15 rejects a
containing-app identity read under the extension sandbox. Finder dispatch remains
pending the local16 correction; use of the management app above does not qualify
Finder execution.
UI capture duration is not filesystem latency, and a
real-network cold GitHub Finder benchmark remains unqualified. macOS 26 and Intel
runtime are also unqualified. See the [mounted acceptance record](fskit-acceptance.md)
for precise scope. Further release qualification must cover remaining recovery,
failure, and cache-coherence gates using disposable fixtures; do not convert the
user's live checkout as an experiment.

Historical beta.3 downloads retain their macFUSE transport, separate-clone
adoption, and cache-based Keep behavior. Native development changes do not alter
those releases. Incremental local app builds precede release packaging,
notarization, and deployment.
