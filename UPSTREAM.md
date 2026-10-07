# ArtifactFS provenance and maintenance

EnoughRepos includes a modified copy of [Cloudflare ArtifactFS](https://github.com/cloudflare/artifact-fs), licensed under [Apache 2.0](LICENSE). Enough Tools maintains the desktop product and its changes. The engine is compiled from this repository into the app's bundled `artifact-fs` helper; it is not an unmodified upstream release.

The upstream baseline is [`2b87a48691ef4ae82d391b7bbe4976c06c7fadf7`](https://github.com/cloudflare/artifact-fs/commit/2b87a48691ef4ae82d391b7bbe4976c06c7fadf7), the `1.0.0-rc.10` tag. This commit was also upstream `main` when checked on October 6, 2026. The original history, Apache license, CLI examples, and [upstream README](docs/upstream/README.md) are retained. The README describes the upstream CLI and its FUSE requirements; EnoughRepos's native macOS setup is documented separately.

## What is reused

All 13 original engine packages remain. Of their 24 original production Go files, 14 are unchanged and 10 have EnoughRepos changes. The additions use the original Git, snapshot, overlay, hydration, and merged-filesystem machinery.

| Original package | EnoughRepos relationship |
| --- | --- |
| `internal/snapshot`, `internal/meta`, `internal/watcher`, `internal/logging` | Original production files unchanged. |
| `internal/gitstore` | Original Git wrapper extended for scoped authentication, batch downloads, persistent working-tree baselines, and recoverability checks. |
| `internal/fusefs` | Original merged view and writable engine extended with native handle/directory behavior and persistent filesystem metadata. |
| `internal/hydrator` | Original hydration and cache verification retained with deduplicated verification changes. |
| `internal/daemon` | Original repository lifecycle extended with catalogue activation, persistent working-tree policy, download, and storage handoffs. |
| `internal/overlay`, `internal/model`, `internal/registry` | Original stores and canonical interfaces extended for metadata, catalogue policy, and desktop storage. |
| `internal/auth`, `internal/cli` | Original redaction and command entrypoints extended for desktop diagnostics and the local control service. |

`cmd/artifact-fs/main.go`, `go.mod`, and `go.sum` retain their upstream forms. The module path remains `github.com/cloudflare/artifact-fs` for the existing internal imports. Running `go install github.com/cloudflare/artifact-fs/cmd/artifact-fs@latest` installs the upstream CLI, not EnoughRepos's helper.

## What EnoughRepos adds

- `native/`: the Mac app, bundled FSKit module, Finder extension, and their shared protocol and tests.
- `internal/desktop`: optional GitHub discovery, native Git adoption, visibility controls, immutable tree previews, ordinary chosen-folder links, and journaled Keep Downloaded / Free Up Space transitions.
- `internal/catalogfs`: the catalogue namespace, lazy preview access, and activation of the writable engine when Git access or writes require it.
- `internal/fsbridge`: the private local filesystem protocol between the Swift FSKit module and the Go filesystem.
- `site/`, product documentation, and packaging/release tooling.

The app starts `artifact-fs desktop serve`; the original CLI does not provide that command. On macOS, FSKit callbacks reach the catalogue and shared engine through the local bridge. Reused FUSE operation types are an internal interface; this native path does not require macFUSE.

Cold browsing uses the new preview path without opening the writable engine. Writable activation still constructs ArtifactFS's Git store, snapshot, overlay, resolver, engine, and hydrator. Keep Downloaded adds verified conversion into an ordinary checkout; adopted checkouts remain native Git repositories.

An upstream binary cannot replace this helper without losing the added APIs and behavior. The original implementation lives in Go `internal/` packages and exposes no supported external library boundary. A future reusable library would require an explicit API and upstream cooperation or a maintained extraction. That refactor is separate from shipping the current desktop beta.

## Modified upstream files

The following original production files carry EnoughRepos changes:

```text
internal/auth/redact.go
internal/cli/cli.go
internal/daemon/daemon.go
internal/fusefs/fuse_unix.go
internal/fusefs/ops.go
internal/gitstore/gitstore.go
internal/hydrator/hydrator.go
internal/model/types.go
internal/overlay/store.go
internal/registry/registry.go
```

Original tests were also modified in `e2e_bench_test.go`, `e2e_git_test.go`, `e2e_test.go`, `internal/auth/redact_test.go`, `internal/fusefs/fuse_unix_test.go`, `internal/fusefs/merged_test.go`, `internal/gitstore/gitstore_test.go`, `internal/hydrator/hydrator_test.go`, and `internal/overlay/store_test.go`. Original `.gitignore` and root `README.md` were changed for the desktop project. Modified retained files carry change notices; new files are EnoughRepos additions.

The upstream `bonk.yml`, `build-test.yml`, `low-quality-filter.yml`, and `semgrep.yml` workflows were replaced by the EnoughRepos workflow. That workflow validates the desktop engine, Linux FUSE compatibility, website, and native SDK builds. It does not automatically publish releases. See [Contributing](CONTRIBUTING.md) and [release instructions](docs/reporeach/releasing.md) for the actual checks and manual distribution gates.

Review changes against the pinned baseline with:

```sh
git diff 2b87a48691ef4ae82d391b7bbe4976c06c7fadf7 -- internal cmd go.mod go.sum
```

Preserve the upstream history, license, notices, and this change map when updating the engine. Upstream fixes must be reviewed against the native catalogue, hydration, and storage contracts; replacing the bundled binary alone is not an engine update.

## Attribution and repository identity

The [license](LICENSE), [NOTICE](NOTICE), and bundled dependency notices accompany distributions. Apache 2.0 permits modifications and redistribution subject to its conditions, including retained notices and explicit notices in changed files. EnoughRepos is an Enough Tools project; Cloudflare does not endorse it. See [Apache 2.0, sections 4 and 6](https://www.apache.org/licenses/LICENSE-2.0).

GitHub's fork-network association is hosting metadata, separate from those license conditions. Keeping or changing that association does not remove the source's provenance or attribution requirements. This repository currently retains the association. Detaching it is not required to ship EnoughRepos; GitHub documents [the separate operation and its metadata-loss consequences](https://docs.github.com/en/pull-requests/how-tos/work-with-forks/detaching-a-fork).
