# RepoReach documentation

RepoReach is an [Enough Tools](https://enoughtools.com) macOS app built on [Cloudflare ArtifactFS](https://github.com/cloudflare/artifact-fs). It puts selected Git repositories in a folder you choose. Add a remote or existing local checkout without signing in, or connect GitHub for account and organization discovery. Repository contents become available on demand through a writable Git filesystem.

This documentation describes the current beta source. A packaged release's download manifest and release notes are the authority for its version, architecture, signing, and notarization status.

- [Using the beta](user-guide.md): setup, virtual folders, Git work, offline downloads, and freeing space.
- [Platform setup](platform-setup.md): macFUSE's kernel backend, signing, security approval, and Finder extension settings.
- [Architecture](architecture.md): the native app, local service, catalogue filesystem, and ArtifactFS storage.
- [Privacy and authentication](privacy-auth.md): GitHub permissions, local credentials, stored data, and network requests.
- [Building and releasing](releasing.md): packaging, signing, notarization, and the release validation checklist.
- [Beta validation](validation.md): tested environments, recorded checks, and remaining installation acceptance.
- [Contributing](../../CONTRIBUTING.md) and [security reporting](../../SECURITY.md).

Source: [github.com/enoughtools/reporeach](https://github.com/enoughtools/reporeach). Product and downloads: [reporeach.reb.run](https://reporeach.reb.run).
