<!-- Modified by Enough Tools for EnoughRepos. Based on Cloudflare ArtifactFS (Apache-2.0); see UPSTREAM.md. -->

# EnoughRepos

**Every repo, right at home.** An open source Mac app by [Enough Tools](https://enoughtools.com).

EnoughRepos puts Git repositories in a folder you choose. Add a Git remote or adopt an existing local checkout without signing in, or connect GitHub to discover the repositories you can access. Browse the catalogue and committed files without cloning every repository. Git commands and writes prepare writable storage when needed. Keep a checkout downloaded for offline use, or reclaim its storage when it is recoverable from its source.

The app has a native SwiftUI management window, a background service, and Finder contextual actions. Closing the window keeps the service running; there is no menu-bar app.

If a virtual folder disconnects, use **Recover Virtual Folders** when offered in
the app. Recovery checks ownership and uses normal filesystem disconnection;
close apps and Finder windows using those folders if they are busy, then retry.
See [recovery guidance](docs/reporeach/user-guide.md#recover-virtual-folders).

[Website and downloads](https://enoughrepos.reb.run) · [Setup guide](docs/reporeach/user-guide.md) · [Architecture](docs/reporeach/architecture.md) · [ArtifactFS provenance](UPSTREAM.md) · [Contributing](CONTRIBUTING.md)

EnoughRepos was previously called RepoReach. Existing state under
`~/Library/Application Support/RepoReach`, signing identifiers, the FSKit short
name `reporeach`, and developer command/environment names are retained for
compatibility. The source repository, documentation paths, and historical
downloads still use the previous name; the previous website address remains
available alongside `enoughrepos.reb.run`. Upgrading keeps existing
repositories and settings; it does not create a separate app identity.

## Beta status

EnoughRepos is an early beta built on a focused fork of [Cloudflare ArtifactFS](https://github.com/cloudflare/artifact-fs). Virtual repositories require **macOS 26 or later**, Git, and normal approval of the bundled FSKit extension. No separate macFUSE installation is required. Read the [setup guide](docs/reporeach/platform-setup.md) and keep backups of valuable local work. The download manifest identifies each published build's signature, notarization status, source revision, and checksum.

The [native acceptance record](docs/reporeach/fskit-acceptance.md) describes the local macOS 27 Apple Silicon checks and remaining qualification work. Historical RepoReach beta.3 downloads retain their original macFUSE requirements and behavior; they do not contain these native changes. The current implementation supports:

- Optional GitHub sign-in through the bundled official GitHub CLI and discovery of accessible personal, collaborator, and organization repositories.
- Manual addition from HTTPS or SSH remotes, local bare repositories, or existing checkout paths, using your native Git credentials or SSH setup.
- An ordinary chosen folder and organization folders, with virtual entries linking into a hidden app-managed filesystem.
- A writable Git working tree backed by ArtifactFS, with ordinary commits and pushes.
- **Keep Downloaded**, **Free Up Space**, and explicit **Refresh** actions in the app and Finder extension.
- Persistent catalogue and pin state, progress reporting, and conservative checks before storage reclamation.
- Repository and owner/organization switches that hide catalogue entries and pause their background downloads without deleting local data.

Existing checkouts are adopted in place, preserving staged changes, uncommitted edits, untracked files, and Git configuration. Keep Downloaded converts a virtual entry into an ordinary checkout that works after the app quits. Per-repo exclusions survive GitHub rediscovery; turning an owner back on leaves individually disabled repositories hidden.

Keep Downloaded covers the current checkout, preserving supported local work. It does not promise complete offline history or every branch, and refuses submodules and checkout filters such as Git LFS. Filesystem indexing and previews can read files and trigger downloads. EnoughRepos does not automatically commit or push your edits: “sync” means acquiring or refreshing Git data, not publishing every save. GitHub discovery reflects what your credential is authorized to access; organization restrictions may require additional authorization.

Free Up Space is intentionally conservative. The service checks the overlay, Git state, local refs, stashes, hooks, and other local data before reclaiming a repository. When it cannot establish that the repository is recoverable, it refuses the operation and reports the reason. It is not a backup system.

## Building from source

The filesystem engine requires Go 1.26 or later:

```sh
go build ./cmd/artifact-fs
go vet ./...
go test ./...
```

The native app requires full Xcode 26 or later with a macOS 26 or later SDK, and XcodeGen. Validate compilation without producing release packages:

```sh
./scripts/build-macos.sh --arch arm64 --compile-only --unsigned
./scripts/build-macos.sh --arch x86_64 --compile-only --unsigned
```

Complete local apps and distribution packages require an authorized FSKit provisioning profile and Developer ID signing. The [release guide](docs/reporeach/releasing.md) documents packaging and optional notarization; compile checks alone do not establish installation or mounted behavior.

The marketing site uses the published [Enough UI](https://github.com/enoughtools/enough-ui) Astro components:

```sh
cd site
npm ci
npm run build
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for native tests, mounted filesystem checks, and the development workflow. The original upstream CLI remains available at `cmd/artifact-fs`; its [original documentation](docs/upstream/README.md) is retained for reference. EnoughRepos's desktop commands extend that same binary. [ArtifactFS provenance](UPSTREAM.md) records the upstream revision, reused engine, and desktop-specific changes.

## Security and licensing

Repository operations connect to the source you add, which can be GitHub, another Git server, or local storage. Manual sources use your native Git authentication; optional GitHub discovery uses the official CLI's credentials and OAuth scopes. The desktop control API uses a private local Unix socket. See [privacy and authentication](docs/reporeach/privacy-auth.md) and [SECURITY.md](SECURITY.md).

EnoughRepos and the ArtifactFS foundation are licensed under [Apache 2.0](LICENSE). Bundled GitHub CLI, Go dependency notices, Enough UI assets, and font licenses retain their respective licenses. GitHub, Cloudflare, Apple, and macFUSE are independent projects; EnoughRepos is an Enough Tools project.
