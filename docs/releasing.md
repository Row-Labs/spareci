# Releasing

Releases are built by [GoReleaser](https://goreleaser.com) from `.goreleaser.yaml`, run by `.github/workflows/release.yml` when a `v*` tag is pushed. That workflow always runs on GitHub-hosted runners (`ubuntu-24.04`), never through `SPARECI_RUNS_ON`, so a release never depends on someone's machine being up.

Each release publishes:

- `spareci_<os>_<arch>.tar.gz` for darwin and linux, arm64 and amd64: a static binary (`CGO_ENABLED=0`) plus `LICENSE` and `README.md`. The file names carry no version, so `releases/latest/download/spareci_darwin_arm64.tar.gz` always points at the newest build.
- `checksums.txt` (SHA-256 of each archive).
- A Homebrew cask, `Casks/spareci.rb`, pushed to [`Row-Labs/homebrew-tap`](https://github.com/Row-Labs/homebrew-tap), so `brew install row-labs/tap/spareci` works.

The version comes from the tag: `v0.1.0` builds a binary whose `spareci version` prints `spareci 0.1.0` (set with `-X main.version`).

## One-time setup

The owner does this once, before the first release.

1. **Make `Row-Labs/spareci` public.** Release downloads, `go install` and the Homebrew cask all need anonymous access to the repo.
2. **Create the tap repo** `Row-Labs/homebrew-tap`: public, with a default branch (initialize it with a README). GoReleaser creates `Casks/spareci.rb` in it on the first release.
3. **Create a fine-grained personal access token** (Settings → Developer settings → Fine-grained tokens):
   - Resource owner: `Row-Labs` (if the org requires approval for fine-grained tokens, approve it under the org's Settings → Personal access tokens)
   - Repository access: only `Row-Labs/homebrew-tap`
   - Permissions: Contents → Read and write (Metadata read-only is added automatically)
   - Expiration: whatever you're comfortable rotating; when it expires, the release still publishes binaries but the Homebrew step fails.
4. **Add it as a secret** on `Row-Labs/spareci`: Settings → Secrets and variables → Actions → New repository secret, name `HOMEBREW_TAP_TOKEN`.

The release itself uses the workflow's own `GITHUB_TOKEN` (`contents: write`) to create the GitHub release; no other secrets are needed.

## Cutting a release

```sh
git checkout main && git pull
git tag -a v0.1.0 -m v0.1.0
git push origin v0.1.0
```

Watch the **Release** workflow in the Actions tab. It runs `go vet`, the short tests, then GoReleaser. The changelog is generated from the commits since the previous tag.

If it fails after the GitHub release was created, delete the release and the tag, fix, and tag again.

## Testing the config locally

No install needed; this uses the same pinned version as the workflow:

```sh
go run github.com/goreleaser/goreleaser/v2@v2.19.0 check
go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean
./dist/spareci_darwin_arm64_v8.0/spareci version
```

A snapshot build publishes nothing. It writes the archives, `checksums.txt` and the cask to `dist/` (ignored by git). When upgrading GoReleaser, change the version in both the workflow and the commands above.

## Notes

- The binaries aren't signed or notarized. The cask removes macOS's quarantine attribute after install so Gatekeeper doesn't block the first run. People who download the tarball by hand with `curl` aren't affected (curl doesn't set quarantine); a browser download needs `xattr -d com.apple.quarantine spareci`.
- The release is built with the Go version in `go.mod`.
