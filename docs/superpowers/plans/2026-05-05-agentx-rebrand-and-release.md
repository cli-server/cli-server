# agentx Rebrand and Release Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the `agentserver/codex` fork as `agentx` via GitHub Releases + WinGet + Chocolatey, with macOS code signing, no Windows signing, and minimal divergence from upstream `openai/codex`.

**Architecture:** A single new GitHub Actions workflow file `.github/workflows/agentx-release.yml` triggered on `agentx-v*.*.*` tags. Source code is untouched; binary rename `codex` → `agentx` happens at packaging time. Upstream `rust-release.yml` is left alone (only fires on `rust-v*` tags we never push). Helper files: a packaging shell script, a Chocolatey package skeleton, and a top-level `Makefile` for cutting tags.

**Tech Stack:** GitHub Actions (ubuntu-24.04, ubuntu-24.04-arm, macos-15, windows-latest), Rust/Cargo for builds, Apple `codesign` + `notarytool` for macOS signing (reusing upstream `.github/actions/macos-code-sign/`), `vedantmgoyal9/winget-releaser` for WinGet PRs, `choco push` for Chocolatey, bash + zstd + tar + zip for packaging.

**Repo paths.** All file edits in this plan are under `/root/codex` (the codex fork). The plan/spec docs live under `/root/agentserver/docs/superpowers/`. Always verify cwd at the start of each task.

**Validation tooling.** This plan uses `actionlint` for workflow YAML validation and `shellcheck` for the packaging script. Both must be installed before Task 2.

**Reference spec:** `/root/agentserver/docs/superpowers/specs/2026-05-05-agentx-rebrand-and-release-design.md`

---

## Task 1: Install validation tooling

**Files:** none (system tools).

- [ ] **Step 1: Install actionlint and shellcheck**

```bash
# Debian/Ubuntu/Arch — pick the right one for the host
# On Arch (this dev env):
sudo pacman -S --noconfirm actionlint shellcheck

# OR via go install if pacman entry missing:
# go install github.com/rhysd/actionlint/cmd/actionlint@latest
# (then make sure $GOPATH/bin or ~/go/bin is in PATH)
```

- [ ] **Step 2: Verify both tools work**

Run:
```bash
actionlint --version
shellcheck --version
```
Expected: both print a version string and exit 0.

- [ ] **Step 3: No commit needed (tooling install is host-local).**

---

## Task 2: Create test harness for the packaging script

The packaging script is the only nontrivial piece of bash logic in this plan. We TDD it locally with fake binaries before writing it. Tests live under `/root/codex/.github/workflows/tests/`.

**Files:**
- Create: `/root/codex/.github/workflows/tests/test_package.sh`
- Create: `/root/codex/.github/workflows/tests/fixtures/.gitkeep`

- [ ] **Step 1: Create directory and test script**

```bash
cd /root/codex
mkdir -p .github/workflows/tests/fixtures
touch .github/workflows/tests/fixtures/.gitkeep
```

Write `/root/codex/.github/workflows/tests/test_package.sh`:

```bash
#!/usr/bin/env bash
# Unit tests for agentx-release-package.sh.
# Each test: set up a fake codex binary in a temp tree, invoke the
# packaging script, assert on the output archive contents.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACKAGE_SH="${SCRIPT_DIR}/../agentx-release-package.sh"

[[ -f "$PACKAGE_SH" ]] || { echo "FAIL: $PACKAGE_SH not found"; exit 1; }

passed=0
failed=0
fail() { echo "FAIL: $1"; failed=$((failed + 1)); }
pass() { echo "PASS: $1"; passed=$((passed + 1)); }

with_tempdir() {
  local d
  d="$(mktemp -d)"
  trap "rm -rf '$d'" RETURN
  pushd "$d" > /dev/null
  "$@"
  popd > /dev/null
}

# Test 1: Linux tar.gz contains a binary named 'agentx' with the right contents.
test_linux_tarball() {
  local target="x86_64-unknown-linux-musl"
  mkdir -p "codex-rs/target/${target}/release"
  printf 'fake-binary-payload' > "codex-rs/target/${target}/release/codex"
  chmod +x "codex-rs/target/${target}/release/codex"

  TARGET="$target" PLATFORM=linux OUTDIR="$PWD/dist" bash "$PACKAGE_SH"

  local archive="dist/agentx-${target}.tar.gz"
  [[ -f "$archive" ]] || { fail "linux tarball missing: $archive"; return; }

  mkdir -p extract && tar -xzf "$archive" -C extract
  [[ -f extract/agentx ]] || { fail "tarball missing 'agentx' entry"; return; }
  [[ "$(cat extract/agentx)" == "fake-binary-payload" ]] \
    || { fail "tarball binary content mismatch"; return; }
  [[ -x extract/agentx ]] || { fail "tarball binary not executable"; return; }
  pass "linux tarball"
}

# Test 2: Windows .exe.zip contains 'agentx.exe' with the right contents.
test_windows_zip() {
  local target="x86_64-pc-windows-msvc"
  mkdir -p "codex-rs/target/${target}/release"
  printf 'fake-windows-payload' > "codex-rs/target/${target}/release/codex.exe"

  TARGET="$target" PLATFORM=windows OUTDIR="$PWD/dist" bash "$PACKAGE_SH"

  local archive="dist/agentx-${target}.exe.zip"
  [[ -f "$archive" ]] || { fail "windows zip missing: $archive"; return; }

  mkdir -p extract && unzip -q "$archive" -d extract
  [[ -f extract/agentx.exe ]] || { fail "zip missing 'agentx.exe' entry"; return; }
  [[ "$(cat extract/agentx.exe)" == "fake-windows-payload" ]] \
    || { fail "zip binary content mismatch"; return; }
  pass "windows zip"
}

# Test 3: macOS tar.gz contains 'agentx', and dmg passthrough works (we don't
# build a real dmg here; we mock its presence and assert it's renamed/copied).
test_macos_outputs() {
  local target="aarch64-apple-darwin"
  mkdir -p "codex-rs/target/${target}/release"
  printf 'fake-mac-binary' > "codex-rs/target/${target}/release/codex"
  chmod +x "codex-rs/target/${target}/release/codex"
  printf 'fake-dmg-content' > "codex-rs/target/${target}/release/codex-${target}.dmg"

  TARGET="$target" PLATFORM=macos OUTDIR="$PWD/dist" bash "$PACKAGE_SH"

  local tarball="dist/agentx-${target}.tar.gz"
  local dmg="dist/agentx-${target}.dmg"
  [[ -f "$tarball" ]] || { fail "macos tarball missing: $tarball"; return; }
  [[ -f "$dmg" ]] || { fail "macos dmg missing: $dmg"; return; }

  mkdir -p extract && tar -xzf "$tarball" -C extract
  [[ -f extract/agentx ]] || { fail "macos tarball missing 'agentx'"; return; }
  [[ "$(cat extract/agentx)" == "fake-mac-binary" ]] \
    || { fail "macos tarball binary content mismatch"; return; }
  [[ "$(cat "$dmg")" == "fake-dmg-content" ]] \
    || { fail "macos dmg content not preserved"; return; }
  pass "macos outputs"
}

# Test 4: SHA256SUMS file is generated and contains every artifact.
test_sha256sums() {
  local target="x86_64-unknown-linux-musl"
  mkdir -p "codex-rs/target/${target}/release"
  printf 'x' > "codex-rs/target/${target}/release/codex"
  chmod +x "codex-rs/target/${target}/release/codex"

  TARGET="$target" PLATFORM=linux OUTDIR="$PWD/dist" bash "$PACKAGE_SH"

  local sums="dist/SHA256SUMS"
  [[ -f "$sums" ]] || { fail "SHA256SUMS missing"; return; }
  grep -q "agentx-${target}.tar.gz" "$sums" \
    || { fail "SHA256SUMS missing tarball entry"; return; }
  pass "sha256sums"
}

with_tempdir test_linux_tarball
with_tempdir test_windows_zip
with_tempdir test_macos_outputs
with_tempdir test_sha256sums

echo "Results: ${passed} passed, ${failed} failed"
[[ $failed -eq 0 ]] || exit 1
```

```bash
chmod +x /root/codex/.github/workflows/tests/test_package.sh
```

- [ ] **Step 2: Run test to confirm it fails (script doesn't exist yet)**

Run:
```bash
bash /root/codex/.github/workflows/tests/test_package.sh
```
Expected: `FAIL: ...agentx-release-package.sh not found` and exit code 1.

- [ ] **Step 3: Commit the test harness**

```bash
cd /root/codex
git add .github/workflows/tests/
git commit -m "test(agentx-release): packaging script test harness"
```

---

## Task 3: Write the packaging script to make tests pass

**Files:**
- Create: `/root/codex/.github/workflows/agentx-release-package.sh`

- [ ] **Step 1: Write the script**

Create `/root/codex/.github/workflows/agentx-release-package.sh`:

```bash
#!/usr/bin/env bash
# Package the built `codex` binary as `agentx` for release distribution.
#
# Inputs (env vars):
#   TARGET      Rust target triple (e.g. x86_64-unknown-linux-musl)
#   PLATFORM    One of: linux, macos, windows
#   OUTDIR      Output directory (created if needed)
#
# Reads:        codex-rs/target/${TARGET}/release/codex[.exe]
#               (macOS only) codex-rs/target/${TARGET}/release/codex-${TARGET}.dmg
#
# Produces:     ${OUTDIR}/agentx-${TARGET}.tar.gz                    (linux + macos)
#               ${OUTDIR}/agentx-${TARGET}.dmg                       (macos only)
#               ${OUTDIR}/agentx-${TARGET}.exe.zip                   (windows)
#               ${OUTDIR}/SHA256SUMS                                 (always; appended)

set -euo pipefail

: "${TARGET:?TARGET env var required}"
: "${PLATFORM:?PLATFORM env var required (linux|macos|windows)}"
: "${OUTDIR:?OUTDIR env var required}"

mkdir -p "$OUTDIR"

case "$PLATFORM" in
  linux|macos)
    src="codex-rs/target/${TARGET}/release/codex"
    [[ -f "$src" ]] || { echo "missing: $src"; exit 1; }

    workdir="$(mktemp -d)"
    cp "$src" "${workdir}/agentx"
    chmod +x "${workdir}/agentx"
    tar -C "$workdir" -czf "${OUTDIR}/agentx-${TARGET}.tar.gz" agentx
    rm -rf "$workdir"

    if [[ "$PLATFORM" == "macos" ]]; then
      dmg_src="codex-rs/target/${TARGET}/release/codex-${TARGET}.dmg"
      [[ -f "$dmg_src" ]] || { echo "missing: $dmg_src"; exit 1; }
      cp "$dmg_src" "${OUTDIR}/agentx-${TARGET}.dmg"
    fi
    ;;

  windows)
    src="codex-rs/target/${TARGET}/release/codex.exe"
    [[ -f "$src" ]] || { echo "missing: $src"; exit 1; }

    workdir="$(mktemp -d)"
    cp "$src" "${workdir}/agentx.exe"
    (cd "$workdir" && zip -q "${OUTDIR}/agentx-${TARGET}.exe.zip" agentx.exe)
    rm -rf "$workdir"
    ;;

  *)
    echo "unknown PLATFORM: $PLATFORM" >&2
    exit 2
    ;;
esac

# Refresh SHA256SUMS to cover everything currently in OUTDIR (except itself).
(
  cd "$OUTDIR"
  : > SHA256SUMS
  # shellcheck disable=SC2046
  sha256sum $(ls -1 | grep -v '^SHA256SUMS$' | sort) >> SHA256SUMS
)
```

```bash
chmod +x /root/codex/.github/workflows/agentx-release-package.sh
```

- [ ] **Step 2: Run the test harness**

Run:
```bash
bash /root/codex/.github/workflows/tests/test_package.sh
```
Expected:
```
PASS: linux tarball
PASS: windows zip
PASS: macos outputs
PASS: sha256sums
Results: 4 passed, 0 failed
```

- [ ] **Step 3: Run shellcheck on the script**

Run:
```bash
shellcheck /root/codex/.github/workflows/agentx-release-package.sh \
           /root/codex/.github/workflows/tests/test_package.sh
```
Expected: no output, exit 0. If warnings appear, fix them inline (most likely candidates: SC2086 unquoted expansion, SC2155 declare-and-assign separation).

- [ ] **Step 4: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release-package.sh
git commit -m "feat(agentx-release): packaging script (codex→agentx rename + archive)"
```

---

## Task 4: Add Makefile target for cutting releases

**Files:**
- Create: `/root/codex/Makefile`

- [ ] **Step 1: Verify no existing Makefile**

Run:
```bash
ls /root/codex/Makefile 2>&1
```
Expected: `ls: cannot access '/root/codex/Makefile': No such file or directory`. If a Makefile exists, switch the verb in the next step from "create" to "extend" — do NOT clobber existing targets.

- [ ] **Step 2: Create the Makefile**

Write `/root/codex/Makefile`:

```makefile
# Fork-only release helpers for agentx. Upstream codex has no top-level Makefile.

.PHONY: agentx-release agentx-release-prerelease

# Cut an agentx release. Bumps Cargo.toml workspace.package.version, refreshes
# Cargo.lock, commits, and tags.
#
# Usage: make agentx-release VERSION=0.128.0-agentx.1
#
# After this target completes, push to remote:
#   git push origin main && git push origin agentx-v$(VERSION)
agentx-release:
	@test -n "$(VERSION)" || (echo "VERSION=x.y.z[-agentx.N] required"; exit 1)
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-agentx\.[0-9]+)?$$' \
		|| (echo "VERSION must match x.y.z or x.y.z-agentx.N (got: $(VERSION))"; exit 1)
	sed -i 's/^version = .*/version = "$(VERSION)"/' codex-rs/Cargo.toml
	cd codex-rs && cargo update --workspace --quiet
	git commit -am "chore(release): agentx $(VERSION)"
	git tag agentx-v$(VERSION)
	@echo
	@echo "Tagged agentx-v$(VERSION). Now run:"
	@echo "  git push origin main && git push origin agentx-v$(VERSION)"

# Convenience for the very first dry-run release.
agentx-release-prerelease:
	$(MAKE) agentx-release VERSION=0.128.0-agentx.0
```

- [ ] **Step 3: Sanity-check via make -n**

Run:
```bash
cd /root/codex && make -n agentx-release VERSION=0.99.0-agentx.5
```
Expected output (the actual commands that would run):
```
sed -i 's/^version = .*/version = "0.99.0-agentx.5"/' codex-rs/Cargo.toml
cd codex-rs && cargo update --workspace --quiet
git commit -am "chore(release): agentx 0.99.0-agentx.5"
git tag agentx-v0.99.0-agentx.5
```

- [ ] **Step 4: Verify version validation rejects bad input**

Run:
```bash
cd /root/codex && make agentx-release VERSION=foo 2>&1
```
Expected: `VERSION must match x.y.z or x.y.z-agentx.N (got: foo)` and non-zero exit. Run also without VERSION:
```bash
cd /root/codex && make agentx-release 2>&1
```
Expected: `VERSION=x.y.z[-agentx.N] required` and non-zero exit.

- [ ] **Step 5: Commit**

```bash
cd /root/codex
git add Makefile
git commit -m "feat(agentx-release): make agentx-release target for tagging"
```

---

## Task 5: Create Chocolatey package skeleton

The Chocolatey job in the CI does sed-substitution on these files at release time to inject the version, download URL, and SHA256.

**Files:**
- Create: `/root/codex/.github/chocolatey/agentx.nuspec.template`
- Create: `/root/codex/.github/chocolatey/tools/chocolateyinstall.ps1.template`
- Create: `/root/codex/.github/chocolatey/tools/chocolateyuninstall.ps1`
- Create: `/root/codex/.github/chocolatey/tools/LICENSE.txt`
- Create: `/root/codex/.github/chocolatey/tools/VERIFICATION.txt`

- [ ] **Step 1: Create the directory structure**

```bash
mkdir -p /root/codex/.github/chocolatey/tools
```

- [ ] **Step 2: Write the nuspec template**

Write `/root/codex/.github/chocolatey/agentx.nuspec.template`:

```xml
<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2015/06/nuspec.xsd">
  <metadata>
    <id>agentx</id>
    <version>__VERSION__</version>
    <title>agentx</title>
    <authors>agentserver</authors>
    <owners>agentserver</owners>
    <licenseUrl>https://github.com/agentserver/codex/blob/main/LICENSE</licenseUrl>
    <projectUrl>https://github.com/agentserver/codex</projectUrl>
    <packageSourceUrl>https://github.com/agentserver/codex/tree/main/.github/chocolatey</packageSourceUrl>
    <requireLicenseAcceptance>false</requireLicenseAcceptance>
    <description>agentx is the agentserver fork of OpenAI Codex — a lightweight coding agent that runs in your terminal.</description>
    <summary>Lightweight coding agent that runs in your terminal (agentserver fork of Codex).</summary>
    <tags>agentx codex cli ai coding-agent</tags>
  </metadata>
  <files>
    <file src="tools\**" target="tools" />
  </files>
</package>
```

- [ ] **Step 3: Write the install script template**

Write `/root/codex/.github/chocolatey/tools/chocolateyinstall.ps1.template`:

```powershell
$ErrorActionPreference = 'Stop'

$packageName = 'agentx'
$url64       = '__URL64__'
$checksum64  = '__SHA256_64__'

$packageArgs = @{
  packageName    = $packageName
  url64bit       = $url64
  checksumType64 = 'sha256'
  checksum64     = $checksum64
  unzipLocation  = "$(Split-Path -Parent $MyInvocation.MyCommand.Definition)"
}

Install-ChocolateyZipPackage @packageArgs

# Mark the unzipped agentx.exe as a shim (Chocolatey auto-shims executables in
# tools\ directory, so this is informational).
Write-Host "agentx installed via Chocolatey. Run 'agentx --help' to get started."
```

- [ ] **Step 4: Write the uninstall script (no template needed — static)**

Write `/root/codex/.github/chocolatey/tools/chocolateyuninstall.ps1`:

```powershell
$ErrorActionPreference = 'Stop'
# Choco auto-removes shims and the package directory; nothing extra to do.
```

- [ ] **Step 5: Write LICENSE and VERIFICATION static files**

Copy upstream LICENSE:
```bash
cp /root/codex/LICENSE /root/codex/.github/chocolatey/tools/LICENSE.txt
```

Write `/root/codex/.github/chocolatey/tools/VERIFICATION.txt`:

```
VERIFICATION

agentx is built from open-source code at https://github.com/agentserver/codex
(a fork of https://github.com/openai/codex).

The Chocolatey package downloads the official Windows release asset published
to the GitHub Release for the corresponding tag and verifies its SHA256
checksum (recorded in chocolateyinstall.ps1 at package time).

To verify a downloaded asset matches the published checksum, run:

    Get-FileHash -Algorithm SHA256 <downloaded-file>

and compare to the value in tools\chocolateyinstall.ps1.
```

- [ ] **Step 6: Commit**

```bash
cd /root/codex
git add .github/chocolatey/
git commit -m "feat(agentx-release): chocolatey package skeleton"
```

---

## Task 6: Workflow scaffold — header + permissions + tag-check

**Files:**
- Create: `/root/codex/.github/workflows/agentx-release.yml`

- [ ] **Step 1: Write the workflow scaffold**

Write `/root/codex/.github/workflows/agentx-release.yml`:

```yaml
# Release workflow for the agentserver/codex fork (distributed as "agentx").
# To release:
#   make agentx-release VERSION=0.128.0-agentx.1
#   git push origin main && git push origin agentx-v0.128.0-agentx.1
#
# Upstream rust-release.yml is left untouched and stays dormant (we never push
# rust-v* tags).

name: agentx-release
on:
  push:
    tags:
      - "agentx-v*.*.*"

concurrency:
  group: ${{ github.workflow }}
  cancel-in-progress: true

permissions:
  contents: write    # GitHub Release create + asset upload
  id-token: write    # OIDC for any future cloud signing

jobs:
  tag-check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
      - name: Validate tag matches Cargo.toml version
        shell: bash
        run: |
          set -euo pipefail
          echo "::group::Tag validation"

          [[ "${GITHUB_REF_TYPE}" == "tag" ]] \
            || { echo "❌  Not a tag push"; exit 1; }
          # Accept either x.y.z (stable, will publish to winget/choco) or
          # x.y.z-agentx.N (iteration, GitHub Release only). Same regex as
          # the Makefile's agentx-release target.
          [[ "${GITHUB_REF_NAME}" =~ ^agentx-v[0-9]+\.[0-9]+\.[0-9]+(-agentx\.[0-9]+)?$ ]] \
            || { echo "❌  Tag '${GITHUB_REF_NAME}' doesn't match expected format"; exit 1; }

          tag_ver="${GITHUB_REF_NAME#agentx-v}"
          cargo_ver="$(grep -m1 '^version' codex-rs/Cargo.toml \
                        | sed -E 's/version *= *"([^"]+)".*/\1/')"

          [[ "${tag_ver}" == "${cargo_ver}" ]] \
            || { echo "❌  Tag ${tag_ver} ≠ Cargo.toml ${cargo_ver}"; exit 1; }

          echo "✅  Tag and Cargo.toml agree (${tag_ver})"
          echo "::endgroup::"
```

- [ ] **Step 2: Run actionlint on the new workflow**

Run:
```bash
actionlint /root/codex/.github/workflows/agentx-release.yml
```
Expected: no output, exit 0. If errors appear, fix them inline (most likely: missing required fields, indentation, or `shellcheck` complaints from embedded run blocks).

- [ ] **Step 3: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release.yml
git commit -m "feat(agentx-release): workflow scaffold + tag-check"
```

---

## Task 7: Add build-linux job

**Files:**
- Modify: `/root/codex/.github/workflows/agentx-release.yml`

- [ ] **Step 1: Append the build-linux job**

Append the following at the end of `.github/workflows/agentx-release.yml` (preserve the existing `tag-check` job; add this as a sibling under `jobs:`):

```yaml
  build-linux:
    needs: tag-check
    name: build-linux - ${{ matrix.target }}
    runs-on: ${{ matrix.runner }}
    timeout-minutes: 90
    permissions:
      contents: read
      id-token: write
    defaults:
      run:
        working-directory: codex-rs
    env:
      CARGO_PROFILE_RELEASE_LTO: "thin"

    strategy:
      fail-fast: false
      matrix:
        include:
          - runner: ubuntu-24.04
            target: x86_64-unknown-linux-musl
          - runner: ubuntu-24.04-arm
            target: aarch64-unknown-linux-musl

    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
      - uses: dtolnay/rust-toolchain@a0b273b48ed29de4470960879e8381ff45632f26 # 1.93.0
        with:
          targets: ${{ matrix.target }}

      - name: Configure musl rusty_v8 artifact overrides and verify checksums
        uses: ./.github/actions/setup-rusty-v8-musl
        with:
          target: ${{ matrix.target }}

      - name: Cargo build (codex)
        shell: bash
        run: cargo build --target ${{ matrix.target }} --release --bin codex

      - name: Package as agentx-${{ matrix.target }}.tar.gz
        shell: bash
        working-directory: ${{ github.workspace }}
        env:
          TARGET: ${{ matrix.target }}
          PLATFORM: linux
          OUTDIR: ${{ github.workspace }}/dist
        run: bash .github/workflows/agentx-release-package.sh

      - uses: actions/upload-artifact@bbbca2ddaa5d8feaa63e36b76fdaad77386f024f # v7
        with:
          name: agentx-${{ matrix.target }}
          path: dist/*
          if-no-files-found: error
```

- [ ] **Step 2: Run actionlint**

Run:
```bash
actionlint /root/codex/.github/workflows/agentx-release.yml
```
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release.yml
git commit -m "feat(agentx-release): build-linux job (x86_64 + aarch64 musl)"
```

---

## Task 8: Add build-macos job

This job mirrors the upstream macOS path: build → sign binary → build dmg → sign dmg → rename to agentx-* → upload. The upstream `macos-code-sign` composite action is used unmodified (it expects the binary at `codex-rs/target/${TARGET}/release/codex` and the dmg at `codex-rs/target/${TARGET}/release/codex-${TARGET}.dmg` — we keep those names through signing, then rename in the packaging step).

**Files:**
- Modify: `/root/codex/.github/workflows/agentx-release.yml`

- [ ] **Step 1: Append the build-macos job**

Append at end of `agentx-release.yml`:

```yaml
  build-macos:
    needs: tag-check
    name: build-macos - ${{ matrix.target }}
    runs-on: macos-15
    timeout-minutes: 90
    permissions:
      contents: read
      id-token: write
    defaults:
      run:
        working-directory: codex-rs
    env:
      CARGO_PROFILE_RELEASE_LTO: "thin"

    strategy:
      fail-fast: false
      matrix:
        include:
          - target: aarch64-apple-darwin
          - target: x86_64-apple-darwin

    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
      - uses: dtolnay/rust-toolchain@a0b273b48ed29de4470960879e8381ff45632f26 # 1.93.0
        with:
          targets: ${{ matrix.target }}

      - name: Cargo build (codex)
        shell: bash
        run: cargo build --target ${{ matrix.target }} --release --bin codex

      - name: MacOS code signing (binary)
        uses: ./.github/actions/macos-code-sign
        with:
          target: ${{ matrix.target }}
          binaries: codex
          sign-binaries: "true"
          sign-dmg: "false"
          apple-certificate: ${{ secrets.APPLE_CERTIFICATE_P12 }}
          apple-certificate-password: ${{ secrets.APPLE_CERTIFICATE_PASSWORD }}
          apple-notarization-key-p8: ${{ secrets.APPLE_NOTARIZATION_KEY_P8 }}
          apple-notarization-key-id: ${{ secrets.APPLE_NOTARIZATION_KEY_ID }}
          apple-notarization-issuer-id: ${{ secrets.APPLE_NOTARIZATION_ISSUER_ID }}

      - name: Build macOS dmg
        shell: bash
        run: |
          set -euo pipefail
          target="${{ matrix.target }}"
          release_dir="target/${target}/release"
          dmg_root="${RUNNER_TEMP}/agentx-dmg-root"
          # The dmg file name stays codex-... here so the next macos-code-sign
          # step (which hardcodes that path) can find it. We rename to agentx-*
          # in the packaging step after signing+stapling.
          dmg_path="${release_dir}/codex-${target}.dmg"
          volname="AgentX (${target})"

          rm -rf "$dmg_root"
          mkdir -p "$dmg_root"
          # Inside the dmg the binary is named 'agentx' for end-user clarity.
          ditto "${release_dir}/codex" "${dmg_root}/agentx"

          rm -f "$dmg_path"
          hdiutil create \
            -volname "$volname" \
            -srcfolder "$dmg_root" \
            -format UDZO \
            -ov \
            "$dmg_path"

          [[ -f "$dmg_path" ]] || { echo "dmg missing"; exit 1; }

      - name: MacOS code signing (dmg)
        uses: ./.github/actions/macos-code-sign
        with:
          target: ${{ matrix.target }}
          sign-binaries: "false"
          sign-dmg: "true"
          apple-certificate: ${{ secrets.APPLE_CERTIFICATE_P12 }}
          apple-certificate-password: ${{ secrets.APPLE_CERTIFICATE_PASSWORD }}
          apple-notarization-key-p8: ${{ secrets.APPLE_NOTARIZATION_KEY_P8 }}
          apple-notarization-key-id: ${{ secrets.APPLE_NOTARIZATION_KEY_ID }}
          apple-notarization-issuer-id: ${{ secrets.APPLE_NOTARIZATION_ISSUER_ID }}

      - name: Package as agentx-${{ matrix.target }}.{tar.gz,dmg}
        shell: bash
        working-directory: ${{ github.workspace }}
        env:
          TARGET: ${{ matrix.target }}
          PLATFORM: macos
          OUTDIR: ${{ github.workspace }}/dist
        run: bash .github/workflows/agentx-release-package.sh

      - uses: actions/upload-artifact@bbbca2ddaa5d8feaa63e36b76fdaad77386f024f # v7
        with:
          name: agentx-${{ matrix.target }}
          path: dist/*
          if-no-files-found: error
```

- [ ] **Step 2: Run actionlint**

Run:
```bash
actionlint /root/codex/.github/workflows/agentx-release.yml
```
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release.yml
git commit -m "feat(agentx-release): build-macos job with code signing + dmg notarization"
```

---

## Task 9: Add build-windows job

GitHub-hosted `windows-latest`, no signing, just a zip per the WinGet regex.

**Files:**
- Modify: `/root/codex/.github/workflows/agentx-release.yml`

- [ ] **Step 1: Append the build-windows job**

Append at end:

```yaml
  build-windows:
    needs: tag-check
    name: build-windows - ${{ matrix.target }}
    runs-on: windows-latest
    timeout-minutes: 90
    permissions:
      contents: read
    defaults:
      run:
        working-directory: codex-rs
    env:
      CARGO_PROFILE_RELEASE_LTO: "thin"

    strategy:
      fail-fast: false
      matrix:
        include:
          - target: x86_64-pc-windows-msvc

    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
      - uses: dtolnay/rust-toolchain@a0b273b48ed29de4470960879e8381ff45632f26 # 1.93.0
        with:
          targets: ${{ matrix.target }}

      - name: Cargo build (codex)
        shell: bash
        run: cargo build --target ${{ matrix.target }} --release --bin codex

      - name: Package as agentx-${{ matrix.target }}.exe.zip
        shell: bash
        working-directory: ${{ github.workspace }}
        env:
          TARGET: ${{ matrix.target }}
          PLATFORM: windows
          OUTDIR: ${{ github.workspace }}/dist
        run: bash .github/workflows/agentx-release-package.sh

      - uses: actions/upload-artifact@bbbca2ddaa5d8feaa63e36b76fdaad77386f024f # v7
        with:
          name: agentx-${{ matrix.target }}
          path: dist/*
          if-no-files-found: error
```

- [ ] **Step 2: Run actionlint**

Run:
```bash
actionlint /root/codex/.github/workflows/agentx-release.yml
```
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release.yml
git commit -m "feat(agentx-release): build-windows job (windows-latest, unsigned)"
```

---

## Task 10: Add release job

Downloads all build artifacts, extracts version from tag, creates the GitHub Release.

**Files:**
- Modify: `/root/codex/.github/workflows/agentx-release.yml`

- [ ] **Step 1: Append the release job**

Append at end:

```yaml
  release:
    needs: [build-linux, build-macos, build-windows]
    name: release
    runs-on: ubuntu-latest
    permissions:
      contents: write
    outputs:
      version: ${{ steps.derive.outputs.version }}
      clean_version: ${{ steps.derive.outputs.clean_version }}
      tag: ${{ github.ref_name }}
      is_stable: ${{ steps.derive.outputs.is_stable }}

    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6

      - name: Generate release notes from tag commit message
        id: release_notes
        shell: bash
        run: |
          set -euo pipefail
          commit="$(git rev-parse "${GITHUB_SHA}^{commit}")"
          notes_path="${RUNNER_TEMP}/release-notes.md"
          git log -1 --format=%B "${commit}" > "${notes_path}"
          echo >> "${notes_path}"
          echo "path=${notes_path}" >> "${GITHUB_OUTPUT}"

      - uses: actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c # v8
        with:
          path: dist
          pattern: agentx-*
          merge-multiple: true

      - name: List staged artifacts
        run: ls -R dist/

      - name: Regenerate combined SHA256SUMS across all platforms
        shell: bash
        run: |
          set -euo pipefail
          cd dist
          rm -f SHA256SUMS
          # shellcheck disable=SC2046
          sha256sum $(ls -1 | sort) > SHA256SUMS
          cat SHA256SUMS

      - name: Derive version strings
        id: derive
        shell: bash
        run: |
          set -euo pipefail
          version="${GITHUB_REF_NAME#agentx-v}"     # 0.128.0-agentx.1
          clean_version="${version%%-*}"            # 0.128.0
          if [[ "${version}" == "${clean_version}" ]]; then
            is_stable="true"
          else
            is_stable="false"
          fi
          echo "version=${version}"           >> "${GITHUB_OUTPUT}"
          echo "clean_version=${clean_version}" >> "${GITHUB_OUTPUT}"
          echo "is_stable=${is_stable}"       >> "${GITHUB_OUTPUT}"

      - name: Create GitHub Release
        uses: softprops/action-gh-release@153bb8e04406b158c6c84fc1615b65b24149a1fe # v2
        with:
          name: ${{ steps.derive.outputs.version }}
          tag_name: ${{ github.ref_name }}
          body_path: ${{ steps.release_notes.outputs.path }}
          files: dist/**
          prerelease: ${{ steps.derive.outputs.is_stable == 'false' }}
```

- [ ] **Step 2: Run actionlint**

Run:
```bash
actionlint /root/codex/.github/workflows/agentx-release.yml
```
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release.yml
git commit -m "feat(agentx-release): release job (GitHub Release + SHA256SUMS)"
```

---

## Task 11: Add WinGet job

**Files:**
- Modify: `/root/codex/.github/workflows/agentx-release.yml`

- [ ] **Step 1: Append the winget job**

Append at end:

```yaml
  winget:
    name: winget
    needs: release
    if: ${{ needs.release.outputs.is_stable == 'true' }}
    runs-on: ubuntu-latest
    permissions:
      contents: read

    steps:
      - name: Publish to WinGet
        uses: vedantmgoyal9/winget-releaser@7bd472be23763def6e16bd06cc8b1cdfab0e2fd5
        with:
          identifier: Agentserver.AgentX
          version: ${{ needs.release.outputs.clean_version }}
          release-tag: ${{ needs.release.outputs.tag }}
          fork-user: agentserver
          installers-regex: '^agentx-x86_64-pc-windows-msvc\.exe\.zip$'
          token: ${{ secrets.WINGET_PUBLISH_PAT }}
```

- [ ] **Step 2: Run actionlint**

Run:
```bash
actionlint /root/codex/.github/workflows/agentx-release.yml
```
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release.yml
git commit -m "feat(agentx-release): winget publish job (stable only)"
```

---

## Task 12: Add Chocolatey job

Downloads the published Windows release asset, computes its SHA256, sed-substitutes the nuspec/install templates, and pushes to chocolatey.org.

**Files:**
- Modify: `/root/codex/.github/workflows/agentx-release.yml`

- [ ] **Step 1: Append the choco job**

Append at end:

```yaml
  choco:
    name: chocolatey
    needs: release
    if: ${{ needs.release.outputs.is_stable == 'true' }}
    runs-on: windows-latest
    permissions:
      contents: read

    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6

      - name: Stage chocolatey package
        shell: bash
        env:
          VERSION:       ${{ needs.release.outputs.clean_version }}
          TAG:           ${{ needs.release.outputs.tag }}
        run: |
          set -euo pipefail
          asset="agentx-x86_64-pc-windows-msvc.exe.zip"
          url="https://github.com/${GITHUB_REPOSITORY}/releases/download/${TAG}/${asset}"

          mkdir -p staged/tools
          curl -sSLo "staged/tools/${asset}" "$url"
          sha256="$(sha256sum "staged/tools/${asset}" | awk '{print $1}')"
          rm "staged/tools/${asset}"   # choco install script downloads it at install time

          # Static files
          cp .github/chocolatey/tools/chocolateyuninstall.ps1 staged/tools/
          cp .github/chocolatey/tools/LICENSE.txt             staged/tools/
          cp .github/chocolatey/tools/VERIFICATION.txt        staged/tools/

          # Templated files
          sed -e "s|__VERSION__|${VERSION}|g" \
              .github/chocolatey/agentx.nuspec.template \
              > staged/agentx.nuspec

          sed -e "s|__URL64__|${url}|g" \
              -e "s|__SHA256_64__|${sha256}|g" \
              .github/chocolatey/tools/chocolateyinstall.ps1.template \
              > staged/tools/chocolateyinstall.ps1

          ls -R staged/

      - name: Pack and push
        shell: pwsh
        env:
          CHOCO_API_KEY: ${{ secrets.CHOCO_API_KEY }}
        working-directory: staged
        run: |
          choco pack agentx.nuspec
          choco push agentx.${{ needs.release.outputs.clean_version }}.nupkg `
            --api-key "$env:CHOCO_API_KEY" `
            --source "https://push.chocolatey.org/"
```

- [ ] **Step 2: Run actionlint**

Run:
```bash
actionlint /root/codex/.github/workflows/agentx-release.yml
```
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
cd /root/codex
git add .github/workflows/agentx-release.yml
git commit -m "feat(agentx-release): chocolatey publish job (stable only)"
```

---

## Task 13: Manual prerequisites + dry-run prerelease tag

This task is the integration test. It requires real GitHub repo access, real Apple Developer secrets, and a real chocolatey.org account. Steps 1–4 are manual setup that the human operator must do; subsequent steps push the prerelease tag and verify the run.

**Files:**
- Modify: `/root/codex/codex-rs/Cargo.toml` (workspace `version` line, line 112)

- [ ] **Step 1 (HUMAN, in a browser): set up operational prerequisites**

  1. **Fork `microsoft/winget-pkgs`** into the `agentserver` org. Visit https://github.com/microsoft/winget-pkgs and click Fork → choose `agentserver` as destination.
  2. **Register on chocolatey.org**, verify publisher email, reserve package id `agentx`. Note the API key from https://community.chocolatey.org/account.
  3. **Apple Developer Program**: in App Store Connect → Users and Access → Keys, create an API Key with the "Developer" role. Download the `.p8` file. Note the Key ID and Issuer ID.
  4. **Export the Developer ID Application certificate** from Keychain Access on a Mac with the certificate installed: select the cert → File → Export → format `.p12`, set a password.

- [ ] **Step 2 (HUMAN): populate repository secrets**

  Visit https://github.com/agentserver/codex/settings/secrets/actions and add these eight secrets:

  - `APPLE_CERTIFICATE_P12` — `base64 -i cert.p12 | pbcopy`
  - `APPLE_CERTIFICATE_PASSWORD` — the password from Step 1.4
  - `APPLE_NOTARIZATION_KEY_P8` — `base64 -i AuthKey_XXX.p8 | pbcopy`
  - `APPLE_NOTARIZATION_KEY_ID` — 10-char ID from Step 1.3
  - `APPLE_NOTARIZATION_ISSUER_ID` — UUID from Step 1.3
  - `WINGET_PUBLISH_PAT` — a GitHub PAT with `public_repo` scope from an account that has write access to `agentserver/winget-pkgs`
  - `CHOCO_API_KEY` — from Step 1.2

  (`GITHUB_TOKEN` is provided automatically — do not add it.)

- [ ] **Step 3: Bump Cargo.toml workspace version to the prerelease**

```bash
cd /root/codex
sed -i 's/^version = .*/version = "0.128.0-agentx.0"/' codex-rs/Cargo.toml
grep -n '^version =' codex-rs/Cargo.toml | head -1
```
Expected output: `112:version = "0.128.0-agentx.0"`.

- [ ] **Step 4: Refresh Cargo.lock**

```bash
cd /root/codex/codex-rs
cargo update --workspace --quiet
cd /root/codex
git diff --stat codex-rs/Cargo.toml codex-rs/Cargo.lock
```
Expected: both files show changes (Cargo.toml has the version line; Cargo.lock has the workspace member version updates).

- [ ] **Step 5: Commit and push the bump + tag (on the feature branch)**

```bash
cd /root/codex
git status                                             # confirm we're on feature/agentx-release-pipeline
git add codex-rs/Cargo.toml codex-rs/Cargo.lock
git commit -m "chore(release): agentx 0.128.0-agentx.0 (dry-run prerelease)"
git tag agentx-v0.128.0-agentx.0
git push agentserver-fork feature/agentx-release-pipeline
git push agentserver-fork agentx-v0.128.0-agentx.0
```

(Remote name is `agentserver-fork` per `git remote -v` in /root/codex. The workflow triggers on the tag push regardless of which branch it points at, so this validates the pipeline end-to-end without merging to main first.)

- [ ] **Step 6: Watch the workflow run**

```bash
gh run list --repo agentserver/codex --workflow agentx-release.yml --limit 3
# Get the most recent run ID, then:
gh run watch <RUN_ID> --repo agentserver/codex
```
Expected: workflow completes with `success`. Because the version contains `-agentx.0`, the `winget` and `choco` jobs are skipped (their `if:` evaluates to false on prereleases) — only `tag-check`, `build-linux`, `build-macos`, `build-windows`, `release` run.

If any job fails, do **not** retry with the same tag. Bump to `agentx-v0.128.0-agentx.1`, push the new tag, and re-run.

- [ ] **Step 7: Validate artifacts**

```bash
mkdir -p /tmp/agentx-validate && cd /tmp/agentx-validate
gh release download agentx-v0.128.0-agentx.0 --repo agentserver/codex
ls -la
sha256sum -c SHA256SUMS
```
Expected: every file in SHA256SUMS reports `OK`.

For the macOS DMG (must be done on a Mac with `spctl` and `xcrun`):
```bash
xcrun stapler validate agentx-aarch64-apple-darwin.dmg
hdiutil attach agentx-aarch64-apple-darwin.dmg
spctl --assess --type execute -vv "/Volumes/AgentX (aarch64-apple-darwin)/agentx"
hdiutil detach "/Volumes/AgentX (aarch64-apple-darwin)"
```
Expected: `stapler validate` says "The validate action worked!"; `spctl` says "accepted, source=Developer ID".

For the Linux tarball:
```bash
tar -xzf agentx-x86_64-unknown-linux-musl.tar.gz
./agentx --help
```
Expected: agentx prints its help text (the help text still says "codex" internally — this is expected per the spec).

For the Windows zip (test on a Windows 11 machine):
```powershell
Expand-Archive agentx-x86_64-pc-windows-msvc.exe.zip -DestinationPath .
.\agentx.exe --help
```
Expected: SmartScreen warning on first run (click "More info" → "Run anyway"); agentx prints its help text.

- [ ] **Step 8: Open PR and merge to main**

Once the dry-run validates, open a PR from `feature/agentx-release-pipeline` to `main`:

```bash
cd /root/codex
gh pr create --base main --head feature/agentx-release-pipeline \
  --title "feat(release): agentx release pipeline + rebrand" \
  --body "Implements docs/superpowers/specs/2026-05-05-agentx-rebrand-and-release-design.md.

Validated end-to-end via dry-run prerelease tag agentx-v0.128.0-agentx.0
(see GitHub Release for artifacts)."
```

After PR review + merge, all subsequent release work happens on `main`.

- [ ] **Step 9: Cut iteration releases (GitHub-Release-only) as needed**

Iteration releases keep the `-agentx.N` suffix. Their `is_stable` evaluates to `false` (because the version contains a `-`), so the `winget` and `choco` jobs are skipped. Use this for any release where you don't (yet) want to publish to package managers — bug-fix iterations, reflowing upstream merges, etc.:

```bash
cd /root/codex
git checkout main && git pull agentserver-fork main
make agentx-release VERSION=0.128.0-agentx.1
git push agentserver-fork main
git push agentserver-fork agentx-v0.128.0-agentx.1
```

- [ ] **Step 10: Cut the first stable release (publishes to winget + choco)**

Stable releases use the bare `x.y.z` form with no suffix, which makes `is_stable=true` and triggers `winget` + `choco`. Pick a version that doesn't collide with what upstream codex itself ships at the same triple (e.g., if upstream is at `0.128.0`, you can still ship agentx `0.128.0` because the package names — `OpenAI.Codex` vs `Agentserver.AgentX`, `codex` vs `agentx` — are distinct namespaces; the Cargo.toml conflict on merge is trivial since both contents are the same value).

```bash
cd /root/codex
git checkout main && git pull agentserver-fork main
make agentx-release VERSION=0.128.0
git push agentserver-fork main
git push agentserver-fork agentx-v0.128.0
```

After the run completes, verify:
- https://github.com/microsoft/winget-pkgs/pulls?q=is%3Apr+author%3Aagentserver+agentx for the WinGet manifest PR
- https://community.chocolatey.org/packages/agentx for the Chocolatey package (initial submission goes through moderation; allow 1–7 days)

---

## Plan complete

After Task 13 Step 10 confirms winget + choco are live, the agentx release pipeline is fully operational. Subsequent releases are a single `make agentx-release VERSION=...` + `git push` per the spec's "Release flow" section.

**Files created (all in /root/codex):**
- `.github/workflows/agentx-release.yml`
- `.github/workflows/agentx-release-package.sh`
- `.github/workflows/tests/test_package.sh`
- `.github/workflows/tests/fixtures/.gitkeep`
- `.github/chocolatey/agentx.nuspec.template`
- `.github/chocolatey/tools/chocolateyinstall.ps1.template`
- `.github/chocolatey/tools/chocolateyuninstall.ps1`
- `.github/chocolatey/tools/LICENSE.txt`
- `.github/chocolatey/tools/VERIFICATION.txt`
- `Makefile`

**Files modified (all in /root/codex):**
- `codex-rs/Cargo.toml` (workspace `version` line, bumped per release)

**No upstream files touched** beyond the single Cargo.toml version line.
