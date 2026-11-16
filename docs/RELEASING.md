# Releasing

For maintainers. A release is a draft GitHub release built by CI from a tag;
nothing is public until the owner publishes the draft. **The gate comes
before the tag**: the Go module proxy sees a tag as soon as it is pushed (and
keeps that version forever), so `go install ...@<version>` works the moment
the tag exists, release or not. Never push a tag to test something.

## What a release contains

| Asset | Made by |
|---|---|
| `refraktal_<version>_<os>_<arch>.tar.gz` for darwin and linux, arm64 and amd64 | `scripts/release`: `go build -trimpath`, no cgo, the version stamped into `refraktal.dev/refraktal.version`; the archive holds `refraktal`, `LICENSE` and `README.md` |
| `install.sh` | `scripts/install.sh`, attached so it can be fetched from the release itself |
| `SHA256SUMS` | `scripts/release` (`sha256sum` format), covering the archives and `install.sh` |
| `refraktal.rb` | `scripts/release`: a Homebrew formula with each archive's URL and checksum, for a tap that is not set up yet (below) |

Archives are reproducible: the same commit and Go version give the same bytes
(fixed timestamps and owners, fixed entry order, gzip without a name or time);
CI builds every target twice on each pull request and compares.

Versions are SemVer tags: `vMAJOR.MINOR.PATCH`, no leading zeros, optionally
`-` and a prerelease (dot-separated identifiers, none empty, numeric ones
without leading zeros; no `+build`). `<version>` in a file name has no
leading `v`. A tag with a hyphen (`v0.2.0-rc.1`) makes a **prerelease**: never
marked latest, so neither GitHub's latest release nor the install script's
default ever picks it.

## Version

`refraktal doctor` prints the version (text: its first line; `--json`: the
`summary` line). It is the release stamp; for `go install ...@v0.1.0`, the
module version Go records; for a local build, Go's VCS pseudo-version, or
`dev`. (The old placeholder tag `v0.0.1` exists: until `v0.1.0` is tagged,
`go install ...@latest` installs that placeholder.)

## Cutting a release

1. Merge everything into `main` with CI green on both systems.
2. **Gate the exact commit you will tag**, before tagging: a fresh checkout,
   `gofmt -l .`, `go vet ./...`, `go test -race ./...`, and the live checks
   (`scripts/m6check.sh`, `scripts/m7check.sh`). Optionally run the
   **Release** workflow by hand with a prerelease version such as
   `v0.1.0-rc.1`: it verifies, builds and uploads the assets as a workflow
   artifact only (no tag, no release); test `install.sh` from it.
3. Only then tag and push: `git tag -s v0.1.0 -m "Refraktal v0.1.0" && git
   push origin v0.1.0`. The **Release** workflow verifies on macOS and Linux,
   builds the assets, checks `SHA256SUMS`, and attaches them to a **draft**
   release (a prerelease for a hyphenated tag).
4. Review the draft: download an archive and `SHA256SUMS`, check that one
   archive, run it, and publish the draft.

   ```sh
   grep ' refraktal_0.1.0_darwin_arm64.tar.gz$' SHA256SUMS | shasum -a 256 -c -   # Linux: sha256sum -c -
   ```

5. `go install refraktal.dev/refraktal/cmd/refraktal@v0.1.0` works through
   the Go module proxy: `refraktal.dev/refraktal` resolves (its `go-import`
   tag points at this repository; checked by installing `v0.0.1`).

The **Release** workflow's actions are pinned to full commit SHAs (version in
a comment); only the job that creates the draft has `contents: write`, and
checkouts keep no credentials. Publishing to npm (`publish-npm.yml`,
`workflow_dispatch` only) and PyPI (`publish-pypi.yml`, on `py-v*` tags) is
separate: a GitHub release or a `v*` tag triggers neither.

## Install routes

Working for v0.1.0, and the only ones the README lists:

- the archives and `SHA256SUMS` on the GitHub release;
- `install.sh` fetched from the tagged release (or from the repository at the
  tag); it downloads over HTTPS only, resolves "latest" from GitHub's redirect
  and refuses a prerelease there, refuses a checksum mismatch, and installs
  through a fresh staging file and an atomic rename;
- `go install refraktal.dev/refraktal/cmd/refraktal@<version>`.

`go test ./scripts/release` exercises `install.sh` against a local HTTPS
server: a good install, a tampered checksum, an unlisted archive, non-HTTPS
bases, a planted symlink at a staging name, and a prerelease as "latest".

**Not yet set up** (do not advertise until they are):

- **Homebrew:** `refraktal/homebrew-tap` does not exist. To set it up: create
  that public repository, then after each release copy the release's
  `refraktal.rb` to `Formula/refraktal.rb` there (by hand, or from the
  workflow with a token that can write to the tap). The formula depends on
  `node`, which the ACP adapters need.
- **`https://refraktal.dev/install`:** the website does not serve the script
  (that URL returns 404). Serving `install.sh` there (from the latest release)
  is a website change, outside this repository.
- **npm and PyPI wrappers that fetch the binary:** `npm/` and `python/` are
  still placeholders, pinned by the trusted-publisher configurations; changing
  them needs the owner's approval. The archive names and `SHA256SUMS` above
  are stable for them to use.
