# Security policy

EnoughRepos is an Enough Tools beta built on Cloudflare ArtifactFS. Security fixes are developed on the current source and latest beta; there is no promised support window for older prerelease versions.

## Report a vulnerability

Use GitHub's [private vulnerability reporting form](https://github.com/enoughtools/reporeach/security/advisories/new). Include the source revision or release version, macOS version, filesystem backend, reproducible steps using a disposable repository, the affected boundary, and any data-loss or credential-exposure impact. Include the macFUSE version for historical builds that use it. Do not include real credentials, private source code, or unredacted logs.

If the private form becomes unavailable, open an issue asking a maintainer for a private reporting channel without including exploit details or sensitive data. There is no published response-time commitment.

Routine reproducible UI bugs can be reported as [GitHub issues](https://github.com/enoughtools/reporeach/issues). Treat possible credential disclosure, arbitrary filesystem access, and unsafe storage deletion as security reports.

## Boundaries and sensitive data

- EnoughRepos delegates GitHub authorization and credential storage to the official GitHub CLI. Its browser login normally uses the system credential store; the CLI can fall back to a plaintext configuration file if secure storage is unavailable. Environment-provided tokens and existing GitHub CLI account configuration can affect authentication.
- The app uses a local service and private Unix socket. The desktop catalogue/control API is not a hosted Enough Tools API. The containing macOS app is not sandboxed; it launches Git and the bundled helper tools as the current user.
- Repository content, downloaded blobs, overlays, Git objects, metadata, and logs are stored locally. Permission restrictions are not encryption. Use the host's ordinary disk encryption and backup controls as needed.
- Finder receives repository identity, status, errors, and the mount root through a metadata-only file. Action URLs are validated and routed back to the app; they do not grant a trusted channel from arbitrary applications.
- Git operations can execute Git hooks and external tooling configured in the user's environment or repo. Repository files and build commands remain untrusted code. Do not run a project merely because it is visible in the catalogue.

See [Privacy and authentication](docs/reporeach/privacy-auth.md) for the actual authorization scopes and storage locations.

## Data-loss safeguards

The desktop Free Up Space action is restricted to expected engine-owned storage. It stops filesystem access, checks specified local state, fetches remote refs, and refuses removal when it cannot verify recoverability. These checks include dirty overlay entries, staged changes, local-only or recovered history, unreachable Git objects, unsupported refs, locks, custom local configuration and hooks, and unsafe storage paths. Failed verification retains the data. Independent backups remain necessary for files and settings not recoverable from GitHub and for failures outside these checked states.

Keep Downloaded verifies the current committed tree; it is not a backup of every historical blob, LFS object, submodule, or development environment. Never use a generic file-by-file cloud-sync service on a live Git directory. [Git's FAQ describes the resulting integrity risks](https://git-scm.com/docs/gitfaq#_transfers).

Credential redaction is applied to service errors and logs, but logs may still contain local paths, repo names, commit identifiers, or other private metadata. Review any diagnostic material before sharing it. A reported redaction bypass should be treated as a security issue.

## Distribution

Official build scripts pin and verify the downloaded GitHub CLI archive, include license notices, and record artifact hashes, source revision, and actual signing/notarization flags. Ad-hoc builds are development artifacts; a valid ad-hoc signature is not a Developer ID identity or Apple notarization. Do not infer trust status from a filename or checksum alone. Review the release's manifest and [release validation checklist](docs/reporeach/releasing.md#release-validation-checklist).
