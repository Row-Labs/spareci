# Releasing

Releases are built by [GoReleaser](https://goreleaser.com) from `.goreleaser.yaml`, run by `.github/workflows/release.yml` when a `v*` tag is pushed. That workflow always runs on GitHub-hosted runners (`ubuntu-24.04`), never through `SPARECI_RUNS_ON`, so a release never depends on someone's machine being up.

Each release publishes:

- `spareci_<os>_<arch>.tar.gz` for darwin and linux, arm64 and amd64: a static binary (`CGO_ENABLED=0`) plus `LICENSE` and `README.md`. The file names carry no version, so `releases/latest/download/spareci_darwin_arm64.tar.gz` always points at the newest build.
- `checksums.txt` (SHA-256 of each archive).
- A Homebrew cask, `Casks/spareci.rb`, pushed over SSH to [`Row-Labs/homebrew-spareci`](https://github.com/Row-Labs/homebrew-spareci), so `brew install row-labs/spareci/spareci` works.

The version comes from the tag: `v0.1.0` builds a binary whose `spareci version` prints `spareci 0.1.0` (set with `-X main.version`).

## One-time setup

The owner does this once, before the first release.

1. **Make `Row-Labs/spareci` public.** Release downloads, `go install` and the Homebrew cask all need anonymous access to the repo.
2. **Create the tap repo** `Row-Labs/homebrew-spareci`: public, with a default branch (initialize it with a README). GoReleaser creates `Casks/spareci.rb` in it on the first release.
3. **Create a deploy key for the tap.** It's an SSH key that can write to `Row-Labs/homebrew-spareci` and nothing else, never expires, and needs no org approval:

   ```sh
   ssh-keygen -t ed25519 -N "" -C "spareci release" -f /tmp/spareci-tap-key
   gh repo deploy-key add /tmp/spareci-tap-key.pub --repo Row-Labs/homebrew-spareci --allow-write --title "spareci release"
   gh secret set HOMEBREW_TAP_DEPLOY_KEY --repo Row-Labs/spareci < /tmp/spareci-tap-key
   rm /tmp/spareci-tap-key /tmp/spareci-tap-key.pub
   ```

   That adds the public half to the tap repo (Settings → Deploy keys, "Allow write access") and stores the private half as the `HOMEBREW_TAP_DEPLOY_KEY` secret on `Row-Labs/spareci`. To rotate it, delete the deploy key and repeat.

The release itself uses the workflow's own `GITHUB_TOKEN` (`contents: write`) to create the GitHub release. The Apple secrets below are optional: without them the release still works and ships unsigned macOS binaries.

## Signing and notarizing the macOS binaries

When five Apple secrets are set, the release signs the darwin binaries with a Developer ID Application certificate and notarizes them with Apple, so macOS runs them without a Gatekeeper prompt whether they came from Homebrew or a browser download. GoReleaser does this from the Linux runner (it uses [quill](https://github.com/anchore/quill), so no Mac or Xcode is involved in CI). It's configured in the `notarize` section of `.goreleaser.yaml`.

How the release decides:

- **All five secrets set:** binaries are signed and notarized, and the cask installs them as-is.
- **None set:** signing is skipped, the binaries ship unsigned, and the cask keeps its step that removes the quarantine attribute (see Notes).
- **Some but not all set:** the workflow fails before building, naming the missing ones, rather than quietly shipping unsigned.

The workflow's "Check Apple signing secrets" step sets `MACOS_NOTARIZE=true` only in the first case; GoReleaser's `notarize` step and the cask's quarantine step both key off it.

### One-time Apple setup

You need a Mac for step 2 (Keychain Access); everything else is in a browser.

1. **Join the [Apple Developer Program](https://developer.apple.com/programs/enroll/)** ($99/year). The certificate shows the enrolled name to anyone who inspects the binary, so enroll as an organization if it should say Row Labs rather than your own name; that needs a D-U-N-S number and takes longer to approve. Only the Account Holder can create Developer ID certificates.
2. **Create the Developer ID Application certificate.**
   1. In Keychain Access: Keychain Access → Certificate Assistant → Request a Certificate From a Certificate Authority. Enter your email, choose "Saved to disk", and save the `.certSigningRequest`.
   2. At [developer.apple.com → Certificates](https://developer.apple.com/account/resources/certificates/list), click **+**, choose **Developer ID Application**, profile type **G2 Sub-CA**, upload the request, and download the `.cer`.
   3. Double-click the `.cer` to add it to your login keychain. It appears under My Certificates as "Developer ID Application: <name> (<team id>)" with a private key under it.
   4. Right-click it → Export → save as `developer-id.p12` with a strong password. That password is `MACOS_SIGN_PASSWORD`.
3. **Create an App Store Connect API key** for notarization.
   1. At [App Store Connect → Users and Access → Integrations → App Store Connect API](https://appstoreconnect.apple.com/access/integrations/api), request access if asked (Account Holder only), then under **Team Keys** click **+**.
   2. Name it `spareci-notarize`, access **Developer**, and generate.
   3. Download the `AuthKey_<KEYID>.p8`. Apple lets you download it only once.
   4. Note the **Key ID** (in the key's row) and the **Issuer ID** (above the table).
4. **Add the five secrets** to `Row-Labs/spareci` (Settings → Secrets and variables → Actions), or with the GitHub CLI from the folder holding the files:

   ```sh
   base64 -i developer-id.p12 | gh secret set MACOS_SIGN_P12 -R Row-Labs/spareci
   gh secret set MACOS_SIGN_PASSWORD -R Row-Labs/spareci        # paste the .p12 password
   base64 -i AuthKey_XXXXXXXXXX.p8 | gh secret set MACOS_NOTARY_KEY -R Row-Labs/spareci
   gh secret set MACOS_NOTARY_KEY_ID -R Row-Labs/spareci        # e.g. XXXXXXXXXX
   gh secret set MACOS_NOTARY_ISSUER_ID -R Row-Labs/spareci     # a UUID
   ```

   | Secret | Value |
   | --- | --- |
   | `MACOS_SIGN_P12` | base64 of `developer-id.p12` |
   | `MACOS_SIGN_PASSWORD` | the password you set when exporting it |
   | `MACOS_NOTARY_KEY` | base64 of `AuthKey_<KEYID>.p8` |
   | `MACOS_NOTARY_KEY_ID` | the API key's Key ID |
   | `MACOS_NOTARY_ISSUER_ID` | the Issuer ID |

5. **Delete the local copies** of the `.p12` and `.p8` once the secrets are saved (or keep them in a password manager). The certificate stays in your keychain; you can export it again.

The Developer ID certificate is valid for five years. Binaries signed before it expires keep working after, because the signature is timestamped. To rotate, repeat step 2 and update `MACOS_SIGN_P12` and `MACOS_SIGN_PASSWORD`. If the API key is revoked, notarization fails and the release stops; create a new one and update the three `MACOS_NOTARY_*` secrets.

### Checking a signed release

The release log shows `signing` and `notarized` for each darwin binary. On a Mac, download a tarball in a browser (so it gets the quarantine attribute), extract it, and run `./spareci version`: it should run with no Gatekeeper prompt. `codesign -dv --verbose=2 spareci` shows `Authority=Developer ID Application: …` and a timestamp.

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

A snapshot build publishes nothing. It writes the archives, `checksums.txt` and the cask to `dist/` (ignored by git). Without `MACOS_NOTARIZE=true` in your environment the signing step is skipped, so no Apple secrets are needed. To see the cask a signed release would produce, run the snapshot with `MACOS_NOTARIZE=true` and `--skip=notarize`. When upgrading GoReleaser, change the version in both the workflow and the commands above.

## Notes

- Until the Apple secrets are set, the binaries aren't signed or notarized. The cask then removes macOS's quarantine attribute after install so Gatekeeper doesn't block the first run. People who download the tarball by hand with `curl` aren't affected (curl doesn't set quarantine); a browser download needs `xattr -d com.apple.quarantine spareci`. A signed release drops that cask step; the cask then has an empty `postflight_steps` block, which Homebrew loads and runs as a no-op (`brew style` flags it as an empty block; GoReleaser always writes the block once the hook is configured).
- A bare binary can't have its notarization ticket stapled to it (only apps, disk images and installer packages can), so on first run macOS checks Apple's servers for the ticket. A Mac that's offline on first run may still show the Gatekeeper prompt.
- The release waits up to 20 minutes (Apple's limit) for each notarization. Apple usually answers in a few minutes. If it hasn't by then, GoReleaser logs a timeout and the release carries on with the signed binary; Apple finishes the notarization on its side and the binary passes Gatekeeper once it does.
- The release is built with the Go version in `go.mod`.
