---
description: Build and safely deploy Rush without killing running sessions
---

Deploy the current checkout's production build and replace the installed Rush executable(s). Run from the repository root:

```sh
go run deploy.go
```

## What happens

- Runs the production build, including the frontend bundle, and embeds a build ID in the new binary.
- Uses `RUSH_DEPLOY_PATH` as a single explicit destination when set. Otherwise it finds replaceable Rush executables on `PATH` (excluding this checkout), including related npm platform and Windows sibling binaries; command shims are not overwritten.
- If no replaceable binary is found, first install goes to `%LOCALAPPDATA%\Programs\rush\rush.exe` on Windows or `~/.local/bin/rush` on Linux/macOS.
- On Windows, the install directory is appended to the user `PATH`; new terminals pick it up, while existing terminals need restarting. On Linux/macOS, `~/.local/bin` is already on `PATH` in most distributions; otherwise deploy prints an export line. It does not edit shell startup files.
- Before acquiring the deploy lock, sweeps leftover Windows renamed-aside files from earlier deploys. Busy files are silently left for a later sweep.
- Acquires a cross-process lock before replacing destinations and verifying them, preventing concurrent deploys from interleaving.
- On Unix, replacement uses a temporary write and rename; running processes keep using the old inode, and replacing the path does not alter their running code.
- On Windows, a running executable can be renamed but cannot be overwritten or deleted. A busy executable is renamed to `<name>.old-<token>` and the new binary takes its place. The old file remains available to running sessions and can be removed by a later deploy after those processes exit.
- Runs the installed binary with `--version` after replacement to verify each destination.
- Several destinations are replaced one after another with no joint rollback: if a later one fails, the earlier ones stay on the new build and the failed one keeps its old binary. Rerun deploy to bring them all to the current build.

## Sessions

Deploy does not kill Rush processes. New terminals or new sessions use the newly installed binary; already-running sessions continue with the old build until they exit. Check the installed build with `rush --version` in a new terminal/session.

## Never

- Never kill Rush processes by image name.
- Never manually delete `.old-*` files while Rush may still be running from them.
