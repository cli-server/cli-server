# agentx rebrand and release pipeline design

Date: 2026-05-05
Status: Approved (awaiting implementation plan)
Repo affected: `agentserver/codex` (fork of `openai/codex`), checked out at `/root/codex`

## Goal

Ship the `agentserver/codex` fork as a standalone, installable product called **agentx**, distributed via:

- GitHub Releases (Linux tarballs, macOS dmgs, Windows .exe.zip)
- Windows Package Manager (WinGet)
- Chocolatey

while changing as little of the upstream `openai/codex` source as possible, so that ongoing rebases against upstream stay near-trivial.

## Non-goals

- Renaming the binary or any user-visible string inside the source code. `agentx --version` will still report `codex 0.128.0-agentx.N`, help text will still say "codex", config dir stays `~/.codex/`. Rebranding is **packaging-layer only**.
- Linux `.deb` / `.rpm`, Homebrew tap, ARM64 Windows binary, Sparkle auto-update, `.app` bundle. (See "YAGNI" section.)
- Pushing tags matching upstream's `rust-v*.*.*` pattern. We use our own `agentx-v*.*.*` namespace.

## Constraints

- **Minimize edits to upstream files.** Every modified upstream file becomes a recurring rebase conflict. Acceptable diff: one workspace `version` line in `codex-rs/Cargo.toml` per release. Everything else is fork-only new files.
- **No paid Windows code signing.** WinGet and Chocolatey accept unsigned binaries; SmartScreen warnings are accepted as a known limitation.
- **macOS signing uses standard Apple Developer Program assets** (Developer ID Application certificate + App Store Connect API key). Reuse upstream's `.github/actions/macos-code-sign/` composite action unmodified.
- **No access to OpenAI's self-hosted runner group `codex-runners`** — Windows builds use GitHub-hosted `windows-latest` runners.

## Architecture

### Single new workflow file

A new file `.github/workflows/agentx-release.yml` is added to the fork. It triggers only on tags matching `agentx-v*.*.*`:

```yaml
on:
  push:
    tags:
      - "agentx-v*.*.*"
```

Upstream's `.github/workflows/rust-release.yml` is left untouched. It triggers on `rust-v*.*.*` tags only, which we never push, so it stays dormant.

### Job topology

```
tag-check
   ├─→ build-linux    (matrix: x86_64-unknown-linux-musl, aarch64-unknown-linux-musl)
   ├─→ build-macos    (matrix: aarch64-apple-darwin, x86_64-apple-darwin; signed + notarized)
   └─→ build-windows  (matrix: x86_64-pc-windows-msvc; unsigned, on windows-latest)
                                                       │
                                                       ▼
                                                    release  (creates GitHub Release, uploads artifacts)
                                                       ├─→ winget   (if stable)
                                                       └─→ choco    (if stable)
```

### Mapping to upstream `rust-release.yml`

| Upstream job | agentx treatment |
|---|---|
| `tag-check` | Mirror copy. Tag regex changed to `^agentx-v[0-9]+\.[0-9]+\.[0-9]+(-(alpha\|beta\|agentx)(\.[0-9]+)?)?$`. Cargo version comparison logic unchanged. |
| `build` (matrix containing macOS + Linux) | Split into separate `build-linux` and `build-macos` jobs. Matrix entries copied verbatim from upstream for the targets we keep. The four `bundle: app-server` entries are dropped (we ship only the main `agentx` binary). The two `bundle: primary` macOS entries and two Linux musl entries are kept. |
| `build-windows` (uses `./.github/workflows/rust-release-windows.yml`) | Discarded. Replaced by an inline job `runs-on: windows-latest` that builds `x86_64-pc-windows-msvc` unsigned. The reusable workflow stays in upstream form, untouched. |
| `argument-comment-lint-release-assets` | Removed. Built dylint, requires `macos-15-xlarge` and complex toolchain; not needed by terminal users. |
| `zsh-release-assets` | Removed. Ships a patched zsh for the `shell-escalation` Linux-only subsystem; not part of agentx's distribution surface. |
| `release` (incl. dotslash + GH Release) | Pruned copy. Artifact names rewritten to `agentx-*`. **Dotslash steps removed** (would require adding per-binary `.zst` outputs and an agentx-specific dotslash config JSON; not worth the divergence for a fork). NPM publishing also removed (we don't publish to npm). The `developers.openai.com` deploy hook is removed. |
| `winget` | Mirror copy. `identifier: Agentserver.AgentX`, `installers-regex: ^agentx-x86_64-pc-windows-msvc\.exe\.zip$` (only x86_64 — see ARM64 entry in YAGNI), `fork-user: agentserver`. |
| `update-branch` (latest-alpha-cli) | Removed. Fork does not maintain that branch. |
| **(new)** `choco` | Pushes `agentx` to chocolatey.org. Triggers only when version is stable (no `-` in cargo version). |
| **(new)** `homebrew` | Updates `Casks/agentx.rb` in the `agentserver/homebrew-tap` repo. Triggers only when version is stable. Mirrors the upstream codex cask shape (cross-platform: macOS arm/intel + Linux arm/intel), sed-substitutes a template file with the version + 4 SHA256s. |

### Files added / modified

**Added (fork-only, never conflict with upstream):**

- `.github/workflows/agentx-release.yml` — the workflow
- `.github/workflows/agentx-release-package.sh` — packaging helper invoked by the build jobs (handles `codex` → `agentx` binary rename, tar.gz / dmg / .exe.zip assembly, SHA256SUMS generation)
- `.github/chocolatey/agentx.nuspec` + install/uninstall scripts — Chocolatey package definition
- `.github/homebrew/agentx.rb.template` — Homebrew cask template with `__VERSION__` and `__SHA256_*__` placeholders that the `homebrew` CI job sed-substitutes at release time
- `Makefile` (or addition to existing) — `make agentx-release VERSION=...` target to bump Cargo version, commit, tag

**Modified (upstream file, conflicts on rebase):**

- `codex-rs/Cargo.toml` — workspace `version` field bumped per release. Single-line conflict, mechanical resolution: keep our `0.128.0-agentx.N`, accept upstream for everything else.

Total expected rebase conflict surface: **1 file, 1 line per release**.

## Naming and version derivation

### Binary rename timing

Source-tree `[[bin]] name = "codex"` is unchanged. The build outputs `target/<triple>/release/codex(.exe)`. The packaging step renames it:

```bash
cp "codex-rs/target/${TARGET}/release/codex${EXT}" "agentx${EXT}"
# then tar/dmg/zip the renamed binary
```

`EXT` is `""` on Linux/macOS, `".exe"` on Windows.

### Artifact filenames

| Platform | Artifact(s) |
|---|---|
| Linux x86_64 musl | `agentx-x86_64-unknown-linux-musl.tar.gz` |
| Linux aarch64 musl | `agentx-aarch64-unknown-linux-musl.tar.gz` |
| macOS aarch64 | `agentx-aarch64-apple-darwin.tar.gz`, `agentx-aarch64-apple-darwin.dmg` |
| macOS x86_64 | `agentx-x86_64-apple-darwin.tar.gz`, `agentx-x86_64-apple-darwin.dmg` |
| Windows x86_64 | `agentx-x86_64-pc-windows-msvc.exe.zip` |
| All | `SHA256SUMS` (one file across all artifacts) |

Each archive contains exactly one file: `agentx` (or `agentx.exe` on Windows). No version string is embedded in the filename — the GitHub Release tag carries the version, and the WinGet `installers-regex` is version-agnostic.

### Version derivation

Single source of truth: `codex-rs/Cargo.toml` workspace `version` field.

```
Cargo.toml workspace.version = "0.128.0-agentx.1"
        │
        ├─→ tag = "agentx-v0.128.0-agentx.1"             (tag-check enforces equality)
        ├─→ GitHub Release tag = same                     (release job consumes directly)
        ├─→ winget version    = "0.128.0"                 (strip suffix at first '-')
        └─→ choco version     = "0.128.0"                 (same)
```

Helper logic, used by `release`, `winget`, `choco` jobs:

```bash
cargo_ver="$(grep -m1 '^version' codex-rs/Cargo.toml | sed -E 's/version *= *"([^"]+)".*/\1/')"
clean_ver="${cargo_ver%%-*}"             # 0.128.0
is_stable() { [[ "${cargo_ver}" == "${clean_ver}" ]]; }
```

`winget` and `choco` jobs gate on `if: needs.release.outputs.is-stable == 'true'`. Prereleases (any `-` in the version) only produce a GitHub Release; they do not pollute the package manager catalogs.

### Top-level workflow permissions

```yaml
permissions:
  contents: write    # GitHub Release creation
  id-token: write    # OIDC for any future cloud signing; also unblocks dotslash
```

This is the fix for the prior `startup_failure` ("nested job 'build-windows' is requesting 'id-token: write', but is only allowed 'id-token: none'"). Lives in the new file only; upstream `rust-release.yml` is not touched.

## Required secrets

Configured in `agentserver/codex` repo Settings → Secrets and variables → Actions:

| Secret | Source | Used by |
|---|---|---|
| `APPLE_CERTIFICATE_P12` | Keychain export of Developer ID Application certificate, `base64 -i cert.p12` | `build-macos` |
| `APPLE_CERTIFICATE_PASSWORD` | Password set when exporting `.p12` | `build-macos` |
| `APPLE_NOTARIZATION_KEY_P8` | App Store Connect → Users and Access → Keys → download `.p8`, `base64 -i AuthKey_XXX.p8` | `build-macos` |
| `APPLE_NOTARIZATION_KEY_ID` | Same page, 10-character Key ID | `build-macos` |
| `APPLE_NOTARIZATION_ISSUER_ID` | Same page header, UUID | `build-macos` |
| `WINGET_PUBLISH_PAT` | GitHub PAT with `public_repo` scope, account that has forked `microsoft/winget-pkgs` under `agentserver` | `winget` |
| `CHOCO_API_KEY` | chocolatey.org account → My Account → API key | `choco` |
| `HOMEBREW_TAP_PAT` | Fine-grained GitHub PAT, Resource owner = `agentserver` org, scoped to repo `agentserver/homebrew-tap`, Permissions: Contents=Read and write | `homebrew` |
| `GITHUB_TOKEN` | Provided automatically | `release` |

## Operational prerequisites (one-time, manual)

1. Fork `microsoft/winget-pkgs` into the `agentserver` org. The `vedantmgoyal9/winget-releaser` action will push commits to it.
2. Register on chocolatey.org with the publisher email; verify; reserve the package id `agentx`.
3. In Apple App Store Connect, create an API Key with the "Developer" role.
4. Ensure `agentserver/homebrew-tap` repo exists (it already does). The `homebrew` CI job will write `Casks/agentx.rb` into it on each stable release.
5. Populate the nine secrets listed above.

These steps are not in CI — they happen once before the first release.

## Release flow (per release)

```
1. git fetch upstream && git merge upstream/main             # pull upstream
2. make agentx-release VERSION=0.128.0-agentx.2              # bumps Cargo.toml, commits, tags
3. git push origin main && git push origin agentx-v0.128.0-agentx.2
                                            │
                                            └─→ triggers agentx-release.yml
                                                 ├─→ tag-check verifies version match
                                                 ├─→ three platforms build in parallel
                                                 ├─→ release creates GitHub Release
                                                 └─→ stable versions also trigger winget + choco
```

The `make agentx-release` target:

```makefile
agentx-release:
    @test -n "$(VERSION)" || (echo "VERSION=x.y.z[-agentx.N] required"; exit 1)
    sed -i 's/^version = .*/version = "$(VERSION)"/' codex-rs/Cargo.toml
    cd codex-rs && cargo update --workspace
    git commit -am "chore(release): agentx $(VERSION)"
    git tag agentx-v$(VERSION)
    @echo "Now run: git push origin main && git push origin agentx-v$(VERSION)"
```

## Error handling and recovery

| Failure | Behavior | Recovery |
|---|---|---|
| `tag-check` fails (tag ≠ Cargo.toml version) | Workflow halts; no jobs queued | Delete tag, fix Cargo.toml, retag with the same name |
| Any `build-*` job fails | That platform's archive is missing from the release; other platforms upload normally | Bump patch suffix (e.g., `-agentx.2` → `-agentx.3`), push new tag — never reuse a failed tag for a different artifact |
| macOS notarization stalls or fails | `build-macos` fails; Linux/Windows already uploaded if they finished first | Same as above |
| `release` job fails after builds | Builds artifacts are present in the workflow run; the GitHub Release was not created | Re-run `release` job only (do not rebuild) |
| `winget` PR fails | GitHub Release exists; only the WinGet manifest update was lost | Re-run `winget` job; or manually open PR against `microsoft/winget-pkgs` |
| `choco push` rejected by moderation | GitHub Release exists; package is not on chocolatey.org | Address moderator feedback, re-run `choco` job |

**Invariant**: a tag, once pushed, is never reused for a different binary. Every retry uses a new patch suffix. This prevents downstream caches (winget, choco, dotslash) from serving inconsistent content for the same version string.

## Testing and pre-flight validation

Before cutting `agentx-v0.128.0-agentx.1` for real, push a prerelease tag:

```
agentx-v0.128.0-agentx.0
```

Because the version contains `-`, `winget` and `choco` jobs do not run. Only the GitHub Release is produced. Validation steps on the artifacts:

- macOS: `spctl --assess --type execute -vv agentx` should report `accepted, source=Developer ID`.
- macOS: `codesign -dvv --deep --strict agentx` should show the certificate chain back to Apple.
- macOS: `xcrun stapler validate agentx-aarch64-apple-darwin.dmg` should report `The validate action worked!`.
- Linux: `./agentx --help` runs on a clean Ubuntu 24.04 container.
- Windows: extract `.exe.zip` and run on Windows 11. SmartScreen warning is expected on first launch (until reputation builds); WinGet install path bypasses most warnings.

If all five pass, push the real `agentx-v0.128.0-agentx.1`.

## YAGNI — explicitly out of scope

The following were considered and dropped. Reintroduce only when there is a concrete user request.

- **Apple `.app` bundle / Sparkle auto-update** — upstream codex does not ship an .app bundle; agentx is CLI.
- **Linux `.deb` / `.rpm`** — upstream ships only tarballs; users install via `cargo install` or extract tarball.
- **ARM64 Windows binary** — upstream does build it, but GitHub-hosted runners do not provide ARM64 Windows; user surface is small.
- **`argv[0]`-based binary multiplexing** — codex appears to use argv[0] introspection (`~/.codex/tmp/arg0/`), but with only one binary shipped, the single name `agentx` does not exercise that branch.
- **Auto-PR back to upstream** — fork does not contribute upstream from CI.
- **DotSlash artifacts** — would require per-binary `.zst` outputs and a fork-specific dotslash config; not a primary install channel for terminal users (winget/choco/dmg cover the cases).
- **NPM publishing** — upstream stages an npm wrapper around the binaries; agentx ships only the binary directly.

## Open ops items (not blocking design)

- Decide whether to publish the GitHub Release as "Latest" automatically. Recommendation: yes for stable, no for prereleases (use Release `prerelease: true` when version contains `-`).
- Decide whether to mirror the previous `rust-v0.128.0-agentserver.1` tag to an `agentx-v*` equivalent for continuity, or simply abandon it. Recommendation: abandon — it never produced a successful release anyway.
