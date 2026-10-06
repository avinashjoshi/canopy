# Releasing canopy

Everything users touch (`install.sh`, `canopy upgrade`) reads GitHub Releases of
`avinashjoshi/canopy`. A release is one tag push.

## Cut a release

1. Bump `version` under `[workspace.package]` in `Cargo.toml` (every crate inherits it).
   Pre-releases use a suffix: `1.0.0-beta.1`. `canopy upgrade` orders versions semver-style,
   so `1.0.0` upgrades a `1.0.0-beta.3` install, and a pre-release never replaces a release.
2. `cargo test --workspace && cargo clippy --workspace --all-targets`.
3. Commit, then tag and push the tag:

   ```
   git tag v1.0.0
   git push origin main v1.0.0
   ```

4. `.github/workflows/release.yml` builds `canopy` for
   `x86_64-unknown-linux-musl`, `aarch64-unknown-linux-musl`, `x86_64-apple-darwin`,
   `aarch64-apple-darwin`, writes `canopy-<tag>-<target>.tar.gz` plus a `.sha256` for each, and
   publishes a GitHub Release with generated notes. Tags containing `-` are marked pre-release.
5. Check it: on any machine, `curl -fsSL .../install.sh | sh` and `canopy upgrade --check`.

Linux builds are static (musl), so one binary runs on any distro. macOS builds are plain
`cargo build` on the matching runner.

## What the clients rely on

- Asset names: `canopy-<tag>-<target>.tar.gz` containing a single file `canopy`, and
  `<asset>.sha256` in `sha256sum` format (`<hex>  <asset>`). `install.sh` and
  `crates/canopy/src/upgrade.rs` both build these names; change them together.
- The tag of the latest release comes from the `https://github.com/<repo>/releases/latest`
  redirect, so there is no API call and no rate limit.
- `canopy upgrade` refuses to overwrite a binary under a cargo `target/` directory (a
  development build) and verifies the checksum before swapping. It renames the new file over
  the old one, stops the old server, and the next command starts the new one.
- After any upgrade, `canopy_client::connect` compares the server's version to its own and
  restarts a mismatched server; tmux sessions are untouched and the sidebars respawn.

## Testing the installer without a release

Serve a fake release locally and point the script at it:

```
D=$(mktemp -d); tag=v9.9.9; target=x86_64-unknown-linux-musl
tar -czf "$D/canopy-$tag-$target.tar.gz" -C target/release canopy
(cd "$D" && sha256sum canopy-$tag-$target.tar.gz > canopy-$tag-$target.tar.gz.sha256)
(cd "$D" && python3 -m http.server 8765 --bind 127.0.0.1 &)
CANOPY_DOWNLOAD_BASE=http://127.0.0.1:8765 CANOPY_VERSION=9.9.9 CANOPY_BIN_DIR=/tmp/cbin sh install.sh
```

## Later

- A Homebrew tap and an AUR `canopy-bin` package can wrap the same assets; the install line
  stays the primary path so there is one thing to document.
