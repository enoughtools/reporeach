# GitHub CLI security scan disposition

Reviewed October 4, 2026 for the official GitHub CLI **2.102.0**, source commit `fc4b137cdef0a6bd28fd461b7cf9c84a5812a8cd`, built with Go 1.27.1. The app bundles checksum-verified official archives without changing their code. Review this disposition again when changing the bundled version.

The official Apple Silicon executable's `govulncheck` 1.8.0 binary scan reports [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), which covers the unmaintained `golang.org/x/crypto/openpgp` family. No fixed version exists for those packages. The [raw scan output](github-cli-binary-scan.txt) is retained.

That finding is conservative module-level evidence. The executable is stripped (`-s -w`), and its Mach-O symbol table lacks `runtime.main`. The scanner's [Mach-O extraction](https://github.com/golang/vuln/blob/v1.8.0/internal/buildinfo/additions_buildinfo.go#L208) marks that condition as stripped. Its [binary fallback](https://github.com/golang/vuln/blob/v1.8.0/internal/vulncheck/binary.go#L107) then uses all known affected packages from the module metadata and synthesizes wildcard symbol entries. Those entries do not establish that the affected packages are linked or called.

Review of the exact release source found:

- The complete Darwin Apple Silicon and Intel `./cmd/gh` dependency graphs exclude all seven affected OpenPGP packages. GitHub CLI imports Rekor's X.509/identity branches, which do not import its PGP branch. Both architectures have identical `x/crypto` and Rekor package sets; Intel adds three CPU-detection packages.
- A complete source scan with Go 1.27.1 and govulncheck 1.8.0 reports **zero called vulnerabilities, zero imported vulnerabilities, and one required-module advisory**.
- Both original executables contain no affected OpenPGP or Rekor PGP package names, while their included X.509 and SSH package names remain visible.
- RepoReach's `gh api`, `gh auth login`, and `gh auth git-credential` command package graphs individually exclude OpenPGP, Rekor, and Sigstore. Its HTTPS device sign-in and Git credential exchange perform no OpenPGP parsing. See the official [login flow](https://github.com/cli/cli/blob/v2.102.0/pkg/cmd/auth/shared/login_flow.go#L157) and [credential helper](https://github.com/cli/cli/blob/v2.102.0/pkg/cmd/auth/gitcredential/helper.go#L58).

The [latest supported official release](https://github.com/cli/cli/releases/tag/v2.102.0) at the review date is 2.102.0. The source-backed disposition is that this advisory does not identify affected imported or called code in that release. It does not establish that every module is advisory-free, and no blanket scanner suppression is used.
