# Privacy and authentication

RepoReach is an Enough Tools desktop app. Its catalogue and repository service run on your Mac. The implementation has no Enough Tools account service or application analytics backend. GitHub sign-in is optional. Manual sources use your native Git credentials or SSH setup and can point to another Git host or local storage. Optional GitHub discovery uses GitHub and the official CLI; opening product/help links uses your browser.

## Manual sources

Adding a Git remote or local checkout does not require the GitHub CLI's OAuth authorization. HTTPS, SSH, and SSH-style addresses use native Git authentication for that source. Local paths are accessed through Git. An HTTP URL containing a password or token is refused; configure your normal credential helper or SSH access instead.

Source URLs and local paths are stored as catalogue metadata and can be private even when there is no GitHub privacy flag. RepoReach does not upload them to an Enough Tools discovery service. Adding a source records metadata; file/clone acquisition waits for entry, Prepare, or Keep Downloaded. Resolving a remote's default branch can contact that remote without acquiring its full checkout.

An existing local checkout supplies committed data to a separate managed virtual checkout. Registration/preparation leaves the original folder, index, staged changes, edits, and untracked files untouched. Source inspection is not a backup of dirty work. Explicit pushes, hooks, and commands that you run afterwards follow ordinary Git behavior and can change their configured destination.

## GitHub permissions

Optional account discovery signs in through the bundled official GitHub CLI using its browser/device authorization flow for `github.com`. Manual sources do not require these OAuth scopes. RepoReach adds no scopes to the CLI's default login. In the pinned CLI version `2.102.0`, a new OAuth login requests:

| Scope | Meaning |
| --- | --- |
| `repo` | Broad read and write access to public and private repositories and associated resources permitted to the account. |
| `read:org` | Read access to organization and team membership information. |
| `gist` | Read and write access to gists. |

This is **not a read-only authorization**. The broad scopes come from the official CLI's login flow, even though RepoReach's discovery and download actions primarily read data. Ordinary Git commands inside mounted repos can push work when your account has permission. See [the pinned CLI auth flow](https://github.com/cli/cli/blob/v2.102.0/internal/authflow/flow.go), [GitHub CLI login](https://cli.github.com/manual/gh_auth_login), and [GitHub's scope definitions](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps).

Existing GitHub CLI authentication is reused when available. RepoReach does not create an isolated CLI configuration directory. A new sign-in uses the GitHub CLI OAuth app and sets the CLI's `github.com` Git protocol to HTTPS. Existing credentials may have different permissions. Inherited `GH_TOKEN` or `GITHUB_TOKEN` can override stored credentials and determine the account and access; `GH_TOKEN` takes precedence. See [GitHub CLI environment variables](https://cli.github.com/manual/gh_help_environment).

Discovery requests repositories the credential can access as owner, collaborator, or organization member. It groups them by owner. An organization's OAuth app restrictions, SAML SSO requirements, or an existing token's restrictions can prevent some repos from appearing. RepoReach cannot bypass those controls. See [the repository listing API](https://docs.github.com/en/rest/repos/repos#list-repositories-for-the-authenticated-user) and [organization OAuth restrictions](https://docs.github.com/en/organizations/managing-oauth-access-to-your-organizations-data/about-oauth-app-access-restrictions).

## Credentials

The official CLI handles token storage. It normally uses macOS Keychain but can fall back to a plaintext configuration file when the system credential store is unavailable. Accordingly, RepoReach does not promise Keychain-only storage. The CLI's usual configuration location is `~/.config/gh`, subject to its environment/configuration rules. Check the official login documentation for your environment; do not paste token output into an issue.

RepoReach does not request `gh auth token`, put access tokens into catalogue records, or return them through its desktop API. Sign-in output is reduced to a recognized one-time device code and GitHub's verification URL. Raw GitHub CLI diagnostics are not returned to the UI.

GitHub-discovered clones use the official CLI's Git credential helper. This is scoped to those managed clones, rather than installed as a new global Git helper. Manual clones use native Git credential helpers or SSH authentication, including when their URL happens to point to GitHub. Git wrappers keep inline credentials out of subprocess URL arguments and redact credential-like error content. Avoid embedding credentials in remote URLs yourself.

Deleting the app or freeing a repository does not revoke the GitHub CLI OAuth authorization or remove shared CLI credentials. Manage the authorization in [GitHub's applications settings](https://github.com/settings/applications) and the official CLI's account settings. Changes there can affect other tools using the same GitHub CLI account.

## Local data

The usual app state root is `~/Library/Application Support/RepoReach`:

| Data | Contents and handling |
| --- | --- |
| `catalogue.json` | Optional account login/avatar URL, repo names and descriptions, source kind (`manual` or GitHub discovery), remote URLs or local source paths, branch/privacy metadata, visibility choices, download/pin/error state, and mount settings. Atomically replaced with mode `0600`. |
| `engine/` | Private blobless clones and fetched Git objects, SQLite snapshots/overlay metadata, locally edited files, and hydrated source contents. Contains private source when private repos are used. |
| `engine.sock` | Local control socket. The service uses HTTP framing over a Unix socket, opens no TCP listener, and sets the socket to mode `0600`. The state/socket directory must be a real directory owned by the current user with private permissions. |
| `finder-status.json` | Mount root and repo IDs/status/pin/error metadata. No repository file bodies or credential fields. Written atomically through a `0600` temporary file. |
| `service.log` | Local engine stdout/stderr. Created with mode `0600`. At service startup, an existing log over 1 MiB is removed; this is not a continuously enforced size cap. |

The native launcher requests `0700` for the app directory, and the service enforces private state-directory permissions. These are local access controls, not app-level encryption and not a boundary against another process running as your user. The containing app is not sandboxed. Use your Mac's disk encryption and backups for local sensitive data.

Finder gets only the status cache and sends validated action URLs to the app. Its sandbox has a narrow read-only exception for that file; see [the extension documentation](../../native/FinderExtension/README.md). The current UI uses a system account icon; the saved avatar URL is not a commitment that an avatar is downloaded.

## Network activity and telemetry

Optional account lookup and discovery contact GitHub's API, and browser authorization contacts GitHub. Manual sources are inspected through native Git at the URL/path you add. Entering a new repo acquires Git metadata, reading an uncached file hydrates its blob, Keep Downloaded fetches current-tree blobs, Refresh fetches source updates, and Free Up Space fetches refs from the source to verify a recoverable copy. The desktop beta disables periodic remote refresh; explicit actions and file reads can still use the network. Local UI status polling does not itself fetch every repo from GitHub.

Repository and owner switches retain local data while hiding entries, preventing new preparation, and pausing background pin downloads. They do not revoke credentials or prevent existing open files from hydrating content. They are not an offline switch or network access control.

The bundled GitHub CLI has its own [upstream telemetry settings](https://docs.github.com/en/github-cli/github-cli/github-cli-telemetry). RepoReach explicitly sets `GH_TELEMETRY=false` for its GitHub CLI subprocesses and managed Git credential-helper environment, overriding inherited telemetry settings. This does not change telemetry preferences for independently launched GitHub CLI commands or other tools.

Repository content is not sent to an Enough Tools repository-storage service. Your chosen Git source receives the normal Git requests needed for its operations; GitHub receives account/discovery requests when you use those features. User-configured Git hooks, filters, helpers, or build tools can make additional network requests outside RepoReach's own operations.

## Diagnostics and removal

Errors are redacted for recognized credential patterns, but can still include repo names, local paths, branch names, and commit IDs. Review logs and screenshots before sharing them. Report a suspected credential leak through [the security reporting process](../../SECURITY.md).

Free Up Space retains the catalogue entry and removes only accepted engine-owned repo storage after safety checks. It does not delete the original local source folder, delete the repository on its server, or clean up the account's GitHub CLI credentials. Turning a repo off also retains its storage. The beta has no general account-data deletion wizard or separate cloud backup service. Before removing app state manually, stop the app/service and independently preserve any local work you need.
