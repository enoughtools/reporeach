# macOS setup for EnoughRepos

The current native development app requires **macOS 26 or later for virtual
repositories**, Git, and normal approval of its bundled FSKit extension. No
separate macFUSE installation is required. The management app's macOS 13
deployment target does not lower the filesystem requirement. Local runtime checks
cover macOS 27.0.1 on Apple Silicon; macOS 26 and Intel runtime remain unqualified.
See the [native acceptance record](fskit-acceptance.md).

EnoughRepos was previously called RepoReach. The app retains existing state,
signing identifiers, and the filesystem short name `reporeach`. Keep
`~/Library/Application Support/RepoReach` when upgrading; renaming or deleting it
can disconnect the app from existing repository state. Historical builds and
published downloads retain their original names.

## Install and enable the bundled extension

Copy `EnoughRepos.app` into Applications and open it. Check the build's manifest
for its actual signing and notarization status. A Developer ID signature
identifies the publisher and protects signed code; it does not substitute for
notarization or extension approval.

Enable EnoughRepos's File System Extension under **System Settings → General →
Login Items & Extensions → File System Extensions** when requested. Enable the
Finder extension separately for badges and repository actions. Approval of one
extension does not approve the other. Managed Macs can require administrator
approval. See [Apple's passthrough FSKit sample](https://developer.apple.com/documentation/fskit/building-a-passthrough-file-system)
for the platform's extension-enablement flow.

If macOS blocks an unnotarized app because Apple cannot check it, and you trust
the build, follow [Apple's opening instructions](https://support.apple.com/102445):
open **System Settings → Privacy & Security**, choose **Open Anyway** for
EnoughRepos, and confirm. Preserve the exact warning if macOS reports damage,
malware, or revoked authorization; those are different failures.

## Choose the repository folder

Choose a writable local folder, such as `~/Repositories`, then select **Enable
Virtual Folders**. The root and organization folders remain ordinary host
directories. Existing files can stay there; conflicting repository names are
refused. Keep the selected folder separate from private app storage.

Virtual entries link into a hidden app-managed FSKit volume. Existing checkouts
are adopted in place, and external checkouts receive direct links to their
original folders. **Keep Downloaded** publishes an ordinary checkout at the
selected repository path. Adopted and kept checkouts continue working after the
app quits. See [the user guide](user-guide.md) for storage and upgrade behavior.

Use local internal storage for initial testing. External disks, network
locations, cloud-synced folders, and paths inside another virtual filesystem
still need qualification. A folder accepted by the app does not establish all
filesystem or editor workflows at that location.

## Check Finder integration

Right-click one repository to check for **Keep Downloaded**, **Free Up Space**,
and **Refresh Repository**. Eligibility depends on its current state; actions
for ambiguous selections across repositories are absent. Active actions show
progress, and completion updates the repository's status. A compiled or signed
extension still needs installed-app validation of registration, permissions,
badges, and dispatch.

Closing the management window leaves EnoughRepos running without a menu-bar
item. Reopen the app to show the window. Explicit Quit stops its owned service
and disconnects virtual access; ordinary local checkouts remain available.
Launch at login is optional.

## Historical RepoReach beta downloads

The immutable RepoReach `0.1.0-beta.3` release uses the earlier macFUSE kernel
backend on macOS 13 or later. It requires a separately installed driver and can
require Recovery approval on Apple Silicon. Its separate-clone adoption and
cache-based Keep behavior differ from the native development app. Follow that
release's retained requirements and [historical validation](validation.md);
installing only macFUSE's FSKit component does not adapt the historical backend.
Current EnoughRepos source does not change those existing downloads.
