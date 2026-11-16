# Releasing

For maintainers. A release is a draft GitHub release built by CI from a tag;
nothing is published until the owner publishes the draft.

## What a release contains

| Asset | Made by |
|---|---|
| `refraktal_<version>_<os>_<arch>.tar.gz` for darwin and linux, arm64 and amd64 | `scripts/release`: `go build -trimpath`, no cgo, the version stamped into `refraktal.dev/refraktal.version`; the archive holds `refraktal`, `LICENSE` and `README.md` |
| `SHA256SUMS` | `scripts/release` (`sha256sum -c` format) |
| `refraktal.rb` | `scripts/release`: the Homebrew formula for the tap, with each archive's URL and checksum |

Archives are reproducible: the same commit and Go version give the same bytes
(fixed timestamps and owners, sorted entries, gzip without a name or time).
`<version>` in a file name has no leading `v`; tags do (`v0.1.0`).

## Version

`refraktal doctor` prints the version (text: its first line; `--json`: the
`summary` line). It is the release stamp; for `go install ...@v0.1.0`, the
module version Go records; for a local build, Go's VCS pseudo-version, or
`dev`.

## Cutting a release

1. Merge everything into `main` with CI green on both systems.
2. Dry run (optional): run the **Release** workflow by hand with a version
   such as `v0.1.0-rc.1`. It verifies, builds and uploads the assets as a
   workflow artifact; no tag, no release.
3. Tag and push: `git tag -s v0.1.0 -m "Refraktal v0.1.0" && git push origin
   v0.1.0`. The **Release** workflow verifies on macOS and Linux, builds the
   assets, checks `SHA256SUMS`, and attaches them to a **draft** release.
4. Review the draft (download an archive, `sha256sum -c SHA256SUMS`, run
   `refraktal doctor`) and publish it.
5. Homebrew: copy the draft's `refraktal.rb` to `Formula/refraktal.rb` in the
   `refraktal/homebrew-tap` repository and commit. **Prerequisite:** that
   repository does not exist yet; the owner creates it (public, named
   `homebrew-tap`, so `brew install refraktal/tap/refraktal` finds it). (Automating this needs a
   token with write access to the tap; until one is set up, it is a manual
   step.)
6. `go install refraktal.dev/refraktal/cmd/refraktal@latest` picks the new tag
   up through the Go module proxy; `refraktal.dev/refraktal` already resolves
   (its `go-import` tag points at this repository).

## The install script

`scripts/install.sh` is meant to be served at `https://refraktal.dev/install`
(the website, outside this repository, must serve it: today that URL is not
there). It resolves the latest release from GitHub's redirect, downloads the
archive for the machine and `SHA256SUMS`, refuses to install on a checksum
mismatch, and installs to `~/.local/bin`. `REFRAKTAL_VERSION`,
`REFRAKTAL_INSTALL_DIR` and `REFRAKTAL_DOWNLOAD_BASE` override it; `go test
./scripts/release` exercises it against a local fake release.

## npm and PyPI

`publish-npm.yml`, `publish-pypi.yml`, `npm/` and `python/` are pinned by the
trusted-publisher configurations and are not part of this pipeline. The
packages are still placeholders. Making them fetch the binary means changing
`npm/` and `python/`, which needs the owner's approval; the archive names and
`SHA256SUMS` above are stable for them to use. The PyPI workflow triggers on
`py-v*` tags, which the release's `v*` tags never match.
