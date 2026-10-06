# RepoReach product contract

Cold browsing is the main product requirement. RepoReach should make repositories
visible in the user's chosen folder without downloading their file contents.
Getting a filesystem extension installed is a means to that experience, not the
acceptance criterion. Release work is deferred until the local experience meets
these requirements.

## Required behavior

1. **Chosen location and mixed local storage.** The catalogue belongs in the
   folder the user chooses. It must support existing local checkouts alongside
   virtual repositories without hiding the existing files underneath a mount.
   Finder's volume name or icon alone does not establish this behavior.
2. **Cold catalogue browsing first.** Showing the root, an organization, and its
   repository placeholders must not synchronously prepare every listed repo.
   Finder metadata probes must not turn an organization listing into a sequence
   of clones. A never-seen repository needs authoritative tree metadata once;
   obtaining it must be separate from downloading file contents. Cached trees
   should remain browsable offline. No invented empty directories or guessed
   negative lookups may conceal legitimate committed files.
3. **Content on demand.** Listing a repository hierarchy must not download blobs
   merely to obtain their exact sizes. Reading content may fetch the required
   blobs. A remote that cannot filter Git objects needs a clear capability limit;
   requesting a blobless clone does not prove that no blobs were transferred.
4. **Adopt an existing checkout.** Adoption must manage the existing native Git
   checkout, preserving its location, index, staged edits, unstaged edits,
   untracked files, branches, and Git configuration. Adding a remote as a new
   virtual repository is a separate operation. Neither needs GitHub sign-in.
5. **Keep locally.** Keeping a repository should make it an ordinary local
   checkout, accessible at its selected path after RepoReach quits. Hydrating a
   private cache is useful for offline virtual access but does not fulfill that
   requirement. Conversion must preserve local work and must not silently reset
   or publish it. Complete history, LFS, and submodule availability need their own
   explicit support rather than being implied by a completed current-tree fetch.
6. **Free space without losing work.** Returning a local repository to a virtual
   entry must prove its data is recoverable first. Unpushed commits, staged or
   unstaged edits, and untracked data must prevent automatic removal. The
   placeholder and its identity remain discoverable afterward.
7. **Independent visibility controls.** Organization and repository controls
   must persist independently. Disabling an organization must retain each repo's
   choice and its local data. GitHub discovery is optional; manually added Git
   sources continue to use native Git authentication.

## Current implementation and gaps

The local native implementation retains ArtifactFS's Git store, snapshots,
overlays, hydration, and writable filesystem engine. FSKit replaces the FUSE
transport. The entire owner/repository catalogue is currently one FSKit volume
mounted directly at the selected folder. It requires an empty mount root and
cannot mix ordinary physical checkouts into that root.

Current **Adopt** registers a separate managed clone of a local source's committed
branch. It leaves the original checkout unchanged; it does not adopt that
checkout in place. Current **Keep Downloaded** stores committed blobs in private
host storage. The data persists across normal shutdown, but the selected path
requires the filesystem service to expose it. Neither operation yet fulfills
the corresponding required behavior above.

The native persistent-working-tree policy also disables the background HEAD
watcher and remote refresh. Explicit Refresh fetches source data while preserving
the visible baseline. This compatibility policy must not be advertised as live
working-tree synchronization. See [native cache coherence](native-fskit.md#cache-coherence-is-a-release-gate).

Cold names-only directory enumeration now avoids hydration. However, any child
lookup beneath a dormant repository currently activates its managed checkout
before checking whether the child exists. Actual Finder observations still show
serial preparation of repositories while displaying an organization, even with
concurrent native lookup admission. The specific triggering Finder requests
have not yet been captured. Warm timings and unit tests are not evidence that
this cold Finder problem is fixed.

## Implementation direction to validate

Ordinary catalogue/organization directories, virtual per-repository filesystems,
and ordinary adopted or materialized checkouts are a candidate way to satisfy
the location and local-storage requirements. This is an architecture change,
not a volume-label change. Multiple FSKit resources, mount ownership, Finder
behavior, restart, and safe conversion all require proof. Per-repo mounts alone
do not solve cold preparation caused by Finder probes.

The next performance work must distinguish lightweight tree discovery and
metadata probing from activating a writable checkout. It must measure actual
first-time Finder browsing with empty managed state, counting source requests,
prepared repositories, and hydrated blobs. Root/org browsing, entering one repo,
traversing its subdirectories, and reading one file are separate measurements.
Include legitimate committed Finder metadata filenames so a shortcut cannot
pass by hiding them. Use disposable fixtures for storage conversions; do not
migrate the user's live checkout as an experiment.

Iterate with incremental local app builds and targeted checks. Native release
packaging, notarization, and deployment follow a working local experience.
