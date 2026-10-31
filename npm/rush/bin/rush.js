#!/usr/bin/env node
'use strict';

// Minimal platform-binary launcher for the @phpcraftdream/rush npm
// package. Zero dependencies — Node builtins only. It resolves the
// prebuilt binary shipped by the matching optional platform package, then
// re-execs it with argv passthrough (spawnSync, argv array, never shell).

const { spawnSync } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const SCOPE = '@phpcraftdream';
const platform = process.platform + '-' + process.arch;
const pkgName = SCOPE + '/rush-' + platform;
const binName = process.platform === 'win32' ? 'rush.exe' : 'rush';

// Resolve the installed platform package directory via its package.json —
// the one file guaranteed to exist with a resolvable extension.
let pkgDir;
try {
  pkgDir = path.dirname(require.resolve(pkgName + '/package.json'));
} catch (_) {
  process.stderr.write(
    'rush: the platform package "' + pkgName + '" is not installed.\n' +
    'This is an unofficial npm build of rush (a fork of charmbracelet/crush).\n' +
    'Supported platforms: linux-x64, linux-arm64, darwin-x64, darwin-arm64, win32-x64.\n' +
    'Reinstall ' + SCOPE + '/rush, or install ' + pkgName + ' manually.\n',
  );
  process.exit(127);
}

const binary = path.join(pkgDir, 'bin', binName);
if (!fs.existsSync(binary)) {
  process.stderr.write(
    'rush: platform package "' + pkgName + '" is installed but its binary is missing:\n' +
    '  ' + binary + '\n' +
    'The package may be corrupted; try reinstalling.\n',
  );
  process.exit(127);
}

// --- Relaunch-from-cache -----------------------------------------------
//
// Never spawn `binary` directly out of node_modules: that keeps the file
// locked for the lifetime of the session, and npm can't overwrite a locked
// file on the next `npm install` (the exact failure mode that motivated
// this wrapper — see docs/plans/2026-07-29-relaunch-from-cache.md §6).
//
// Instead: copy the resolved binary into a per-build cache directory once,
// then always exec the cached copy. The cache key is the SHA-256 content
// hash of the *original* file, not size+mtimeMs+version: a stat-based key
// is spoofable — any tool that overwrites the binary in place while
// preserving its size and mtime (e.g. a deploy/copy step that explicitly
// sets mtime) would collide with the old key and keep serving the stale
// cached build forever, even though the content changed (see plan §6.1
// and P3.4 in docs/reviews). Hashing the actual bytes closes that hole:
// the cache key can only match if the content is identical.
//
// The key lives in the cache *directory* name, never in the binary's own
// file name: agentguard's Tier-3 self-block (agentguard.go) matches the
// recursive-rush denylist against the process image's base name, so the
// cached copy must still be literally "rush.exe" / "rush".
//
// Any failure anywhere in this block falls back to the pre-existing
// behaviour: spawn `binary` straight out of node_modules.
let launchTarget = binary;
let privateLaunchDir;
try {
  const cacheRoot = resolveCacheRoot();
  const binCacheDir = path.join(cacheRoot, 'rush', 'bin');

  // Hash the installed binary first so an existing cache entry can be used
  // without copying it into a staging directory. If the source changes during
  // a cache miss copy, the staged hash remains the authoritative final key.
  fs.mkdirSync(binCacheDir, { recursive: true });
  let key = hashFileSync(binary);
  let targetDir = path.join(binCacheDir, key);
  let targetPath = path.join(targetDir, binName);
  if (!fs.existsSync(targetPath)) {
    const tmpDir = path.join(
      binCacheDir,
      '.tmp-' + process.pid + '-' + process.hrtime.bigint().toString(36),
    );
    fs.mkdirSync(tmpDir);
    const tmpPath = path.join(tmpDir, binName);
    fs.copyFileSync(binary, tmpPath);
    if (process.platform !== 'win32') {
      fs.chmodSync(tmpPath, 0o755);
    }

    key = hashFileSync(tmpPath);
    targetDir = path.join(binCacheDir, key);
    targetPath = path.join(targetDir, binName);
    try {
      fs.renameSync(tmpDir, targetDir);
    } catch (renameErr) {
      // A concurrent launcher may have published the same staged bytes.
      if (fs.existsSync(targetPath)) {
        fs.rmSync(tmpDir, { recursive: true, force: true });
      } else {
        throw renameErr;
      }
    }
  }

  privateLaunchDir = path.join(
    binCacheDir,
    '.tmp-launch-' + process.pid + '-' + process.hrtime.bigint().toString(36),
  );
  fs.mkdirSync(privateLaunchDir);
  const privateLaunchPath = path.join(privateLaunchDir, binName);
  try {
    fs.linkSync(targetPath, privateLaunchPath);
  } catch (_) {
    // Fall back to the exact bytes represented by this cache key, not the
    // potentially replaced installed binary.
    fs.copyFileSync(targetPath, privateLaunchPath);
  }
  if (process.platform !== 'win32') {
    fs.chmodSync(privateLaunchPath, 0o755);
  }

  // Best-effort sweep of stale build caches. Never allowed to block or
  // fail the actual launch — every removal is individually guarded, and
  // the whole block is wrapped again below by the outer try/catch.
  try {
    for (const entry of fs.readdirSync(binCacheDir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      if (entry.name === key) continue;
      if (entry.name.startsWith('.tmp-launch-')) {
        const match = /^\.tmp-launch-(\d+)-/.exec(entry.name);
        // Unknown names and unverifiable PIDs are retained. Only ESRCH is
        // proof that an owner is gone; EPERM and other errors mean live/unknown.
        if (!match || isProcessAlive(Number(match[1]))) continue;
        try {
          fs.rmSync(path.join(binCacheDir, entry.name), { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
        } catch (_) {
          // Housekeeping only; retry on a later launch.
        }
        continue;
      }
      if (entry.name.startsWith('.tmp-')) continue; // cache staging by another process
      try {
        fs.rmSync(path.join(binCacheDir, entry.name), { recursive: true, force: true });
      } catch (_) {
        // Directory (or a file inside it) is held open by a live process
        // on this or another build's key — leave it for a later sweep.
      }
    }
  } catch (_) {
    // Sweeping is pure housekeeping; never let it affect the launch.
  }

  launchTarget = privateLaunchPath;
} catch (cacheErr) {
  process.stderr.write(
    'rush: warning: binary cache unavailable (' + cacheErr.message + '); ' +
    'running directly from the installed package.\n',
  );
  launchTarget = binary;
}

var result;
try {
  result = spawnSync(launchTarget, process.argv.slice(2), { stdio: 'inherit' });
} finally {
  releasePrivateLaunchDir(privateLaunchDir);
}

// Spawn-time failure (ENOENT/EACCES) — the binary couldn't be launched.
if (result.error) {
  process.stderr.write('rush: failed to launch ' + launchTarget + ': ' + result.error.message + '\n');
  process.exit(1);
}

// Forward a fatal signal to the launcher so the exit behaviour matches a
// native exec as closely as possible.
if (result.signal) {
  process.kill(process.pid, result.signal);
  process.exit(1); // Fallback if the signal is not delivered synchronously.
}

process.exit(result.status == null ? 1 : result.status);

// --- helpers -------------------------------------------------------------

// hashFileSync returns the lowercase hex SHA-256 of the file at
// absolutePath, computed via a stream so memory use stays flat regardless
// of binary size (rush binaries run tens of MB) — the whole file is never
// read into memory at once.
//
// Cost note: hashing runs on every invocation (see the cache-key doc
// comment above for why a stat-based size+mtime key isn't sufficient), and
// costs roughly the time to read the file once from disk/page-cache
// (order of ~100ms-1s for a ~60MB binary depending on cache warmth) — cheap
// relative to spawning a child process and negligible next to the
// non-interactive-agent-tooling workloads this CLI targets, so no
// cache-the-hash-itself shortcut is used: any such shortcut would have to
// be keyed by the same spoofable size+mtime this fix exists to stop
// trusting.
function hashFileSync(absolutePath) {
  const hash = crypto.createHash('sha256');
  const fd = fs.openSync(absolutePath, 'r');
  try {
    const buf = Buffer.alloc(1 << 20); // 1 MiB read buffer
    let bytesRead;
    let position = 0;
    while ((bytesRead = fs.readSync(fd, buf, 0, buf.length, position)) > 0) {
      hash.update(bytesRead === buf.length ? buf : buf.subarray(0, bytesRead));
      position += bytesRead;
    }
  } finally {
    fs.closeSync(fd);
  }
  return hash.digest('hex');
}

// Node has no os.UserCacheDir() equivalent — compute the per-platform
// cache root by hand, with an explicit operator override.
function resolveCacheRoot() {
  if (process.env.RUSH_BIN_CACHE) {
    return process.env.RUSH_BIN_CACHE;
  }
  if (process.platform === 'win32') {
    return process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local');
  }
  if (process.platform === 'darwin') {
    return path.join(os.homedir(), 'Library', 'Caches');
  }
  return process.env.XDG_CACHE_HOME || path.join(os.homedir(), '.cache');
}

function isProcessAlive(pid) {
  if (!Number.isSafeInteger(pid) || pid <= 0) return true;
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return !err || err.code !== 'ESRCH';
  }
}

function releasePrivateLaunchDir(privateDir) {
  if (!privateDir) return;
  try {
    // Windows keeps the just-exited child's image locked briefly after
    // spawnSync returns (EBUSY/EPERM); retry, since the sweep above skips
    // .tmp-* and a lost race here would leak the directory for good.
    fs.rmSync(privateDir, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  } catch (_) {
    // Best-effort cleanup only.
  }
}
