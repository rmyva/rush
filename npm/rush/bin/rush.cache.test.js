#!/usr/bin/env node
'use strict';

// Standalone regression test for the relaunch-from-cache logic added to
// rush.js (see docs/plans/2026-07-29-relaunch-from-cache.md §6). There is
// no test runner configured under npm/ (Jest/Vitest/etc. are not present —
// only web/ has its own Playwright e2e harness, which is unrelated). This
// file is intentionally a bare-Node script using only the `assert` builtin,
// so it doesn't justify pulling in a framework for one test file.
//
// Run manually:
//   node npm/rush/bin/rush.cache.test.js
//
// Exits 0 on success, non-zero (with a thrown Error / assertion message) on
// failure. Never touches the real global npm install or the real
// %LOCALAPPDATA%/rush/bin cache — every fixture lives under a fresh
// fs.mkdtempSync() directory, and RUSH_BIN_CACHE / NODE_PATH are pointed at
// those temp dirs for each spawned child process only.
//
// How resolution is faked: rush.js resolves the platform package via
// require.resolve('@phpcraftdream/rush-<platform>/package.json'). We give
// each spawned child a NODE_PATH pointing at a temp node_modules directory
// containing a fake @phpcraftdream/rush-<platform> package (package.json +
// bin/rush(.exe)), which Node's module resolution honors for any process
// that has NODE_PATH set at startup (Module._initPaths runs once, early).

const assert = require('node:assert');
const { spawnSync, spawn } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const vm = require('node:vm');

const RUSH_JS = path.join(__dirname, 'rush.js');
const PLATFORM = process.platform + '-' + process.arch;
const PKG_NAME = '@phpcraftdream/rush-' + PLATFORM;
const BIN_NAME = process.platform === 'win32' ? 'rush.exe' : 'rush';

let failures = 0;

function section(name, fn) {
  process.stdout.write('--- ' + name + ' ---\n');
  try {
    fn();
    process.stdout.write('PASS: ' + name + '\n');
  } catch (err) {
    failures++;
    process.stdout.write('FAIL: ' + name + '\n');
    process.stdout.write((err && err.stack ? err.stack : String(err)) + '\n');
  }
}

// Creates a fresh temp root with:
//   <root>/node_modules/@phpcraftdream/rush-<platform>/package.json
//   <root>/node_modules/@phpcraftdream/rush-<platform>/bin/rush(.exe)
//   <root>/cache/                (empty — RUSH_BIN_CACHE target)
// Returns { root, nodeModules, pkgDir, binPath, cacheDir }.
function makeFixture(prefix, opts) {
  opts = opts || {};
  const root = fs.mkdtempSync(path.join(os.tmpdir(), prefix + '-'));
  const nodeModules = path.join(root, 'node_modules');
  const pkgDir = path.join(nodeModules, '@phpcraftdream', 'rush-' + PLATFORM);
  fs.mkdirSync(path.join(pkgDir, 'bin'), { recursive: true });

  const version = opts.version || '0.1.7';
  fs.writeFileSync(
    path.join(pkgDir, 'package.json'),
    JSON.stringify({ name: PKG_NAME, version }),
  );

  const binPath = path.join(pkgDir, 'bin', BIN_NAME);
  if (opts.realBinary) {
    if (process.platform === 'win32') {
      fs.copyFileSync(process.execPath, binPath);
      if (opts.binSuffix) fs.appendFileSync(binPath, opts.binSuffix);
    } else {
      const label = opts.binLabel || opts.binSuffix || 'fixture';
      fs.writeFileSync(
        binPath,
        '#!/usr/bin/env node\n// ' + label + '\nprocess.stdout.write("fixture\\n");\n',
      );
      fs.chmodSync(binPath, 0o755);
    }
  } else {
    fs.writeFileSync(binPath, opts.binContent || 'fake-binary-content-v1');
  }

  const cacheDir = path.join(root, 'cache');
  fs.mkdirSync(cacheDir, { recursive: true });

  return { root, nodeModules, pkgDir, binPath, cacheDir };
}

class LauncherExit extends Error {
  constructor(code) {
    super('launcher exited');
    this.code = code;
  }
}

function runLauncherInVm(fixture, hooks) {
  hooks = typeof hooks === 'function' ? { beforeSpawn: hooks } : (hooks || {});
  const source = fs.readFileSync(RUSH_JS, 'utf8');
  const realRequire = require;
  const launchRequire = (request) => {
    if (request === 'node:child_process') {
      const childProcess = realRequire(request);
      return Object.assign({}, childProcess, {
        spawnSync(...args) {
          if (hooks.beforeSpawn) hooks.beforeSpawn(args[0]);
          if (hooks.captureExecution) hooks.captureExecution(args[0]);
          return childProcess.spawnSync(...args);
        },
      });
    }
    if (request === 'node:fs' && (hooks.beforePin || hooks.failLink || hooks.onLink || hooks.beforeSourceCopy)) {
      const fsModule = realRequire(request);
      return new Proxy(fsModule, {
        get(target, property, receiver) {
          if (property === 'copyFileSync') {
            return (...args) => {
              if (args[0] === fixture.binPath && hooks.beforeSourceCopy) {
                const hook = hooks.beforeSourceCopy;
                hooks.beforeSourceCopy = null;
                hook(args[0], args[1]);
              }
              return target.copyFileSync(...args);
            };
          }
          if (property !== 'linkSync') return Reflect.get(target, property, receiver);
          return (...args) => {
            if (hooks.beforePin) hooks.beforePin();
            if (hooks.onLink) hooks.onLink(args[1]);
            if (hooks.failLink) {
              const err = new Error('hardlinks unavailable in test');
              err.code = 'EOPNOTSUPP';
              throw err;
            }
            return target.linkSync(...args);
          };
        },
      });
    }
    return realRequire(request);
  };
  launchRequire.resolve = (request) => {
    if (request === PKG_NAME + '/package.json') return path.join(fixture.pkgDir, 'package.json');
    return realRequire.resolve(request);
  };

  const launchProcess = Object.create(process);
  launchProcess.env = Object.assign({}, process.env, {
    RUSH_BIN_CACHE: fixture.cacheDir,
  });
  launchProcess.argv = [process.execPath, RUSH_JS, '--version'];
  if (hooks.deadPid != null) {
    launchProcess.kill = (pid, signal) => {
      if (pid === hooks.deadPid && signal === 0) {
        const err = new Error('process does not exist');
        err.code = 'ESRCH';
        throw err;
      }
      return process.kill(pid, signal);
    };
  }
  launchProcess.exit = (code) => { throw new LauncherExit(code); };

  try {
    vm.runInNewContext(source, {
      Buffer,
      process: launchProcess,
      require: launchRequire,
    }, { filename: RUSH_JS });
  } catch (err) {
    if (!(err instanceof LauncherExit)) throw err;
    return err.code;
  }
  throw new Error('launcher returned without calling process.exit');
}

// Runs rush.js as a child process against a fixture. Returns the spawnSync
// result. We pass a harmless arg; the fake "binary" is not a real
// executable so the final spawnSync inside rush.js is expected to fail at
// exec time (ENOENT/EACCES/garbage-exec) — that's fine and expected, we only
// care that the caching logic itself (resolve/copy/rename/sweep) ran
// without throwing an unhandled exception.
function runWrapper(fixture, extraEnv) {
  const env = Object.assign({}, process.env, {
    NODE_PATH: fixture.nodeModules,
    RUSH_BIN_CACHE: fixture.cacheDir,
  }, extraEnv || {});
  // Make sure ambient RUSH_BIN_CACHE from the real dev environment (if any)
  // never leaks in even via extraEnv override semantics.
  return spawnSync(process.execPath, [RUSH_JS, '--version'], {
    env,
    encoding: 'utf8',
  });
}

function listCacheDirs(binCacheDir) {
  if (!fs.existsSync(binCacheDir)) return [];
  return fs
    .readdirSync(binCacheDir, { withFileTypes: true })
    .filter((e) => e.isDirectory())
    .map((e) => e.name);
}

// ---------------------------------------------------------------------
// 0. Content-hash regression (P3.4): binary content replaced while size AND
//    mtime are BOTH preserved exactly must still produce a NEW cache key.
//    This is the precise attack the size+mtime-based key could not catch —
//    a stat-only key is indistinguishable between "unchanged file" and
//    "same-size content swapped in with mtime explicitly restored" (e.g. a
//    deploy/copy tool that preserves timestamps). Content hashing (SHA-256
//    of the actual bytes) closes this: the key can only match if the bytes
//    are identical.
// ---------------------------------------------------------------------
function testContentHashCatchesSameSizeSameMtimeSwap() {
  const original = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'; // 32 bytes
  const swapped = 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'; // 32 bytes — identical size
  assert.strictEqual(original.length, swapped.length, 'test setup error: payloads must be the same size');

  const fx = makeFixture('rush-hash-regress', { version: '0.1.7', binContent: original });
  const binCacheDir = path.join(fx.cacheDir, 'rush', 'bin');

  const r1 = runWrapper(fx);
  assert.ok(!r1.error || r1.error.code !== 'undefined', 'first run must not throw: ' + (r1.error && r1.error.message));

  const dirsAfterFirst = listCacheDirs(binCacheDir);
  assert.strictEqual(dirsAfterFirst.length, 1, 'expected exactly 1 cache dir after first run, got: ' + JSON.stringify(dirsAfterFirst));
  const firstKey = dirsAfterFirst[0];

  // Capture the original mtime, swap the content in place (same size), then
  // restore the original mtime — simulating a deploy/copy tool that
  // preserves timestamps across an in-place content replacement. Compared
  // at millisecond granularity: Node's fs.utimesSync (and the underlying
  // OS utimes call) only accepts millisecond resolution, so it cannot
  // perfectly round-trip a sub-millisecond mtime some filesystems report
  // (e.g. NTFS's 100ns ticks) — that rounding is a real-world limit of any
  // "preserve mtime" tool built on the same APIs, not specific to this
  // test, so millisecond equality is the right (and realistic) bar here.
  const statBefore = fs.statSync(fx.binPath);
  fs.writeFileSync(fx.binPath, swapped);
  fs.utimesSync(fx.binPath, statBefore.atime, statBefore.mtime);
  const statAfter = fs.statSync(fx.binPath);
  assert.strictEqual(statAfter.size, statBefore.size, 'test setup error: size must be identical');
  assert.strictEqual(
    Math.round(statAfter.mtimeMs),
    Math.round(statBefore.mtimeMs),
    'test setup error: mtime must be identical at millisecond granularity (this is the scenario being tested)',
  );

  const r2 = runWrapper(fx);
  assert.ok(!r2.error || r2.error.code !== 'undefined', 'second run must not throw: ' + (r2.error && r2.error.message));

  const dirsAfterSecond = listCacheDirs(binCacheDir);
  const secondKey = dirsAfterSecond.find((k) => k !== firstKey);
  assert.ok(
    secondKey,
    'REGRESSION: no new cache key appeared after content was swapped with size+mtime both preserved — ' +
      'the cache key is still derived from size+mtime rather than content, so P3.4 is not fixed. dirs: ' +
      JSON.stringify(dirsAfterSecond),
  );

  const secondCachedBin = path.join(binCacheDir, secondKey, BIN_NAME);
  assert.ok(fs.existsSync(secondCachedBin), 'cached binary missing under new key: ' + secondCachedBin);
  assert.strictEqual(
    fs.readFileSync(secondCachedBin, 'utf8'),
    swapped,
    'cache entry under the new key does not contain the swapped content',
  );

  // The stale (pre-swap) cache entry must eventually be swept; give the
  // sweep from run 2 credit even if run1's key directory briefly coexists.
  const remaining = listCacheDirs(binCacheDir);
  assert.ok(
    remaining.includes(secondKey),
    'new key missing after sweep: ' + JSON.stringify(remaining),
  );

  fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
}

// ---------------------------------------------------------------------
// 1. Key regression: binary mutated (content, size, and mtime all differ)
//    WITHOUT bumping package version must produce a NEW cache key, and the
//    stale one must be swept. (See test 0 above for the narrower case where
//    size+mtime are both preserved across the content swap.)
// ---------------------------------------------------------------------
function testKeyRegression() {
  const fx = makeFixture('rush-key-regress', { version: '0.1.7', binContent: 'fake-binary-content-v1' });
  const binCacheDir = path.join(fx.cacheDir, 'rush', 'bin');

  const r1 = runWrapper(fx);
  assert.ok(
    !r1.error || r1.error.code !== 'undefined',
    'first run must not throw before reaching spawnSync: ' + (r1.error && r1.error.message),
  );

  const dirsAfterFirst = listCacheDirs(binCacheDir);
  assert.strictEqual(
    dirsAfterFirst.length,
    1,
    'expected exactly 1 cache dir after first run, got: ' + JSON.stringify(dirsAfterFirst),
  );
  const firstKey = dirsAfterFirst[0];
  const firstCachedBin = path.join(binCacheDir, firstKey, BIN_NAME);
  assert.ok(fs.existsSync(firstCachedBin), 'cached binary missing after first run: ' + firstCachedBin);
  assert.strictEqual(
    fs.readFileSync(firstCachedBin, 'utf8'),
    'fake-binary-content-v1',
    'cached binary content mismatch after first run',
  );

  // Mutate the "binary" in place: different size, and force mtime forward
  // to guarantee mtimeMs differs even on coarse-grained filesystems.
  // Version in package.json is deliberately left untouched — this is
  // exactly deploy.go's behavior (overwrite binary, same package version).
  const statBefore = fs.statSync(fx.binPath);
  fs.writeFileSync(fx.binPath, 'fake-binary-content-v2-longer-payload');
  const futureMs = statBefore.mtimeMs + 5000;
  const futureSec = futureMs / 1000;
  fs.utimesSync(fx.binPath, futureSec, futureSec);
  const statAfter = fs.statSync(fx.binPath);
  assert.notStrictEqual(
    statAfter.mtimeMs,
    statBefore.mtimeMs,
    'test setup error: mtimeMs did not change after utimesSync',
  );

  const r2 = runWrapper(fx);
  assert.ok(
    !r2.error || r2.error.code !== 'undefined',
    'second run must not throw before reaching spawnSync: ' + (r2.error && r2.error.message),
  );

  const dirsAfterSecond = listCacheDirs(binCacheDir);
  assert.strictEqual(
    dirsAfterSecond.length,
    1,
    'REGRESSION: expected exactly 1 cache dir after second run (stale key swept), got: ' +
      JSON.stringify(dirsAfterSecond) +
      ' — if this is 2, the sweep did not remove the stale key; if it is 1 but equal to the ' +
      'first key, the cache key did not change when size/mtime changed without a version bump ' +
      '(this is the §6.1 regression this test exists to catch)',
  );

  const secondKey = dirsAfterSecond[0];
  assert.notStrictEqual(
    secondKey,
    firstKey,
    'REGRESSION: cache key is unchanged after binary size/mtime changed without a package ' +
      'version bump — deploy.go overwrites the binary without bumping version, so a stable key ' +
      'here would silently keep serving a stale cached build forever (plan §6.1)',
  );

  const secondCachedBin = path.join(binCacheDir, secondKey, BIN_NAME);
  assert.ok(fs.existsSync(secondCachedBin), 'cached binary missing after second run: ' + secondCachedBin);
  assert.strictEqual(
    fs.readFileSync(secondCachedBin, 'utf8'),
    'fake-binary-content-v2-longer-payload',
    'REGRESSION: cache entry under the new key does not contain the NEW binary content — ' +
      'stale content would mean a version/build is being served that does not match the ' +
      'installed package',
  );

  fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
}

// ---------------------------------------------------------------------
// 2. Cache reuse: two runs with an unchanged binary must not re-copy on the
//    second run (cached file mtime must be identical before/after).
// ---------------------------------------------------------------------
function testCacheReuse() {
  const fx = makeFixture('rush-reuse', { version: '0.1.7', binContent: 'fake-binary-content-stable' });
  const binCacheDir = path.join(fx.cacheDir, 'rush', 'bin');

  const r1 = runWrapper(fx);
  assert.ok(!r1.error || r1.error.code !== 'undefined', 'first run must not throw: ' + (r1.error && r1.error.message));

  const dirs1 = listCacheDirs(binCacheDir);
  assert.strictEqual(dirs1.length, 1, 'expected exactly 1 cache dir after first run, got: ' + JSON.stringify(dirs1));
  const cachedBin = path.join(binCacheDir, dirs1[0], BIN_NAME);
  assert.ok(fs.existsSync(cachedBin), 'cached binary missing after first run');
  const mtimeBefore = fs.statSync(cachedBin).mtimeMs;

  // Second run, binary untouched.
  const r2 = runWrapper(fx);
  assert.ok(!r2.error || r2.error.code !== 'undefined', 'second run must not throw: ' + (r2.error && r2.error.message));

  const dirs2 = listCacheDirs(binCacheDir);
  assert.strictEqual(
    dirs2.length,
    1,
    'expected still exactly 1 cache dir after second (no-op) run, got: ' + JSON.stringify(dirs2),
  );
  assert.strictEqual(dirs2[0], dirs1[0], 'cache key changed even though the binary was not modified');

  const mtimeAfter = fs.statSync(cachedBin).mtimeMs;
  assert.strictEqual(
    mtimeAfter,
    mtimeBefore,
    'cached file was rewritten on the second run even though nothing changed — expected the ' +
      'wrapper to skip the copy when targetPath already exists',
  );

  fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
}

// ---------------------------------------------------------------------
// 3. First-launch race: two wrapper processes started concurrently against
//    a cold cache for the same key must both complete without an unhandled
//    exception in the caching logic, and must leave exactly one final
//    cache directory (no leftover .tmp-* staging dirs, no duplicate dirs).
// ---------------------------------------------------------------------
async function testConcurrentFirstLaunch() {
  const fx = makeFixture('rush-race', { version: '0.1.7', binContent: 'fake-binary-content-race' });
  const binCacheDir = path.join(fx.cacheDir, 'rush', 'bin');

  function spawnAsync() {
    return new Promise((resolve) => {
      const env = Object.assign({}, process.env, {
        NODE_PATH: fx.nodeModules,
        RUSH_BIN_CACHE: fx.cacheDir,
      });
      const child = spawn(process.execPath, [RUSH_JS, '--version'], { env, stdio: 'pipe' });
      let stderr = '';
      child.stderr.on('data', (d) => { stderr += d; });
      child.on('error', (err) => resolve({ launchError: err, stderr }));
      child.on('close', () => resolve({ launchError: null, stderr }));
    });
  }

  const [a, b] = await Promise.all([spawnAsync(), spawnAsync()]);

  assert.strictEqual(a.launchError, null, 'first concurrent process failed to launch at all: ' + (a.launchError && a.launchError.message));
  assert.strictEqual(b.launchError, null, 'second concurrent process failed to launch at all: ' + (b.launchError && b.launchError.message));

  // Neither process's caching logic should have reported an unhandled crash.
  // (The wrapper's own try/catch turns caching errors into a warning + safe
  // fallback, never a Node stack-trace crash; a Node "Uncaught" trace or
  // "internal/process" mention would indicate the caching logic itself blew
  // up unhandled rather than being caught by the wrapper's own try/catch.)
  for (const [label, res] of [['first', a], ['second', b]]) {
    assert.ok(
      !/Uncaught|at internal\/process/.test(res.stderr),
      label + ' concurrent process appears to have thrown an unhandled exception in the caching ' +
        'logic; stderr:\n' + res.stderr,
    );
  }

  const finalDirs = fs.existsSync(binCacheDir)
    ? fs.readdirSync(binCacheDir, { withFileTypes: true }).map((e) => e.name)
    : [];
  const realDirs = finalDirs.filter((n) => !n.startsWith('.tmp-'));
  const tmpDirs = finalDirs.filter((n) => n.startsWith('.tmp-'));

  assert.strictEqual(
    tmpDirs.length,
    0,
    'leftover .tmp-* staging directories after the race resolved: ' + JSON.stringify(tmpDirs),
  );
  assert.strictEqual(
    realDirs.length,
    1,
    'expected exactly 1 final cache dir after two concurrent first-launches raced for the same ' +
      'key, got: ' + JSON.stringify(realDirs),
  );

  fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
}

// ---------------------------------------------------------------------
// 4. Eviction before pin: an older cache key may disappear before linking,
//    so the launcher must recover by copying the installed executable.
// ---------------------------------------------------------------------
function testConcurrentDifferentBuildLaunches() {
  const fxA = makeFixture('rush-live-a', { realBinary: true });
  const fxB = makeFixture('rush-live-b', { realBinary: true, binSuffix: 'build-b' });
  try {
    fxB.cacheDir = fxA.cacheDir;

    const nestedExitCodes = [];
    let evictedKey;
    const exitA = runLauncherInVm(fxA, {
      beforePin: () => {
        evictedKey = listCacheDirs(path.join(fxA.cacheDir, 'rush', 'bin'))[0];
        nestedExitCodes.push(runLauncherInVm(fxB));
      },
    });

    assert.deepStrictEqual(
      nestedExitCodes,
      [0],
      'newer build failed while evicting the older build cache entry',
    );
    assert.strictEqual(exitA, 0, 'older build failed after the eviction sweep ran');
    assert.ok(evictedKey, 'older shared cache key was not created before eviction');
    assert.ok(
      !fs.existsSync(path.join(fxA.cacheDir, 'rush', 'bin', evictedKey)),
      'older shared cache key was not evicted before pin fallback',
    );

    const binCacheDir = path.join(fxA.cacheDir, 'rush', 'bin');
    const cacheDirs = listCacheDirs(binCacheDir);
    assert.ok(
      cacheDirs.length <= 1,
      'expected stale shared keys to be evicted: ' + JSON.stringify(cacheDirs),
    );
    assert.strictEqual(
      fs.readdirSync(binCacheDir).filter((name) => name.startsWith('.tmp-')).length,
      0,
      'private launch directory was not cleaned after both children exited',
    );
  } finally {
    fs.rmSync(fxA.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
    fs.rmSync(fxB.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  }
}

function testAbandonedLaunchDirectorySweep() {
  const fx = makeFixture('rush-launch-sweep', { realBinary: true });
  try {
    const binCacheDir = path.join(fx.cacheDir, 'rush', 'bin');
    fs.mkdirSync(binCacheDir, { recursive: true });
    const deadDir = path.join(binCacheDir, '.tmp-launch-2147483647-dead');
    const liveDir = path.join(binCacheDir, '.tmp-launch-' + process.pid + '-live');
    fs.mkdirSync(deadDir);
    fs.writeFileSync(path.join(deadDir, BIN_NAME), 'abandoned');
    fs.mkdirSync(liveDir);
    fs.writeFileSync(path.join(liveDir, BIN_NAME), 'in-use');
    const staleTime = new Date(Date.now() - 25 * 60 * 60 * 1000);
    fs.utimesSync(liveDir, staleTime, staleTime);

    let launchPath;
    const deadPid = 2147483647;
    const exitCode = runLauncherInVm(fx, {
      deadPid,
      onLink: (target) => { launchPath = target; },
      beforeSpawn: () => {
        assert.ok(!fs.existsSync(deadDir), 'abandoned launch directory was not swept');
        assert.ok(fs.existsSync(liveDir), 'directory owned by a live process was swept');
        assert.ok(Date.now() - fs.statSync(liveDir).mtimeMs > 24 * 60 * 60 * 1000, 'live stale directory timestamp was unexpectedly refreshed');
      },
    });
    assert.strictEqual(exitCode, 0, 'launcher failed during stale launch sweep');
    const key = listCacheDirs(binCacheDir).find((name) => /^[a-f0-9]{64}$/.test(name));
    assert.ok(key, 'hash-named cache entry missing');
    assert.strictEqual(path.basename(path.join(binCacheDir, key, BIN_NAME)), BIN_NAME);
    assert.ok(!launchPath || path.basename(launchPath) === BIN_NAME, 'executable basename changed');
  } finally {
    fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  }
}

function testWarmCacheLaunchDoesNotCopy() {
  const fx = makeFixture('rush-warm-cache', { realBinary: true });
  try {
    let firstCopy = false;
    const firstExit = runLauncherInVm(fx, {
      beforeSourceCopy: () => { firstCopy = true; },
    });
    assert.strictEqual(firstExit, 0, 'first launch failed');
    assert.strictEqual(firstCopy, true, 'first cold-cache launch did not copy the installed binary');

    let secondCopy = false;
    const secondExit = runLauncherInVm(fx, {
      beforeSourceCopy: () => { secondCopy = true; },
    });
    assert.strictEqual(secondExit, 0, 'second launch failed');
    assert.strictEqual(secondCopy, false, 'warm-cache launch copied the installed binary');
  } finally {
    fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  }
}

function testCopyReplacementUsesPublishedHash() {
  const fx = makeFixture('rush-copy-replacement', { realBinary: true });
  try {
    let replacementApplied = false;
    const executedBytes = [];
    const originalCopy = fs.readFileSync(fx.binPath);
    const replacement = process.platform === 'win32'
      ? Buffer.concat([fs.readFileSync(process.execPath), Buffer.from('replacement-build')])
      : Buffer.from('#!/usr/bin/env node\nprocess.stdout.write("replacement-build\\n");\n');

    const launch = () => runLauncherInVm(fx, {
      captureExecution: (launchTarget) => {
        executedBytes.push(fs.readFileSync(launchTarget));
      },
      beforeSourceCopy: () => {
        if (replacementApplied) return;
        replacementApplied = true;
        fs.writeFileSync(fx.binPath, replacement);
        if (process.platform !== 'win32') fs.chmodSync(fx.binPath, 0o755);
      },
      beforeSpawn: () => {
        const binCacheDir = path.join(fx.cacheDir, 'rush', 'bin');
        const key = listCacheDirs(binCacheDir).find((name) => /^[a-f0-9]{64}$/.test(name));
        assert.ok(key, 'hash-named cache entry missing');
        const published = fs.readFileSync(path.join(binCacheDir, key, BIN_NAME));
        assert.strictEqual(
          key,
          require('node:crypto').createHash('sha256').update(published).digest('hex'),
          'cache key does not match the bytes actually published',
        );
      },
    });

    assert.strictEqual(launch(), 0, 'first launch failed after source replacement');
    assert.strictEqual(launch(), 0, 'repeat launch failed for the genuine replacement build');
    assert.strictEqual(replacementApplied, true, 'source-copy hook did not replace the installed build');
    assert.strictEqual(executedBytes.length, 2, 'both launches should reach the executable');
    for (const bytes of executedBytes) {
      assert.deepStrictEqual(bytes, replacement, 'launcher selected bytes other than the replacement build');
    }
    assert.notDeepStrictEqual(originalCopy, replacement, 'fixture replacement must be a distinct build');
  } finally {
    fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  }
}

function testHardlinkFallback() {
  const fx = makeFixture('rush-link-fallback', { realBinary: true });
  try {
    const exitCode = runLauncherInVm(fx, {
      failLink: true,
      beforePin: () => fs.writeFileSync(fx.binPath, 'replaced-after-cache-publish'),
    });
    assert.strictEqual(exitCode, 0, 'copy fallback did not launch the cached bytes');

    const binCacheDir = path.join(fx.cacheDir, 'rush', 'bin');
    assert.strictEqual(listCacheDirs(binCacheDir).length, 1, 'cache entry missing after copy fallback');
    assert.strictEqual(
      fs.readdirSync(binCacheDir).filter((name) => name.startsWith('.tmp-')).length,
      0,
      'private copy directory was not cleaned after launch',
    );
  } finally {
    fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  }
}

function testHardlinkPinSurvivesEviction() {
  const fxA = makeFixture('rush-pinned-a', { realBinary: true });
  const fxB = makeFixture('rush-pinned-b', { realBinary: true, binSuffix: 'build-b' });
  try {
    fxB.cacheDir = fxA.cacheDir;

    const nestedExitCodes = [];
    let evictedKey;
    const exitA = runLauncherInVm(fxA, {
      beforeSpawn: () => {
        evictedKey = listCacheDirs(path.join(fxA.cacheDir, 'rush', 'bin'))[0];
        nestedExitCodes.push(runLauncherInVm(fxB));
      },
    });

    assert.deepStrictEqual(nestedExitCodes, [0], 'newer build failed during cache eviction');
    assert.strictEqual(exitA, 0, 'hardlinked older build failed after cache eviction');
    assert.ok(evictedKey, 'older shared cache key was not created before pinning');
    assert.ok(
      !fs.existsSync(path.join(fxA.cacheDir, 'rush', 'bin', evictedKey)),
      'older shared cache key was not evicted after the pinned launch began',
    );

    const binCacheDir = path.join(fxA.cacheDir, 'rush', 'bin');
    assert.strictEqual(
      fs.readdirSync(binCacheDir).filter((name) => name.startsWith('.tmp-')).length,
      0,
      'private launch directory was not cleaned after both children exited',
    );
  } finally {
    fs.rmSync(fxA.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
    fs.rmSync(fxB.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  }
}

// ---------------------------------------------------------------------
// 7. Fallback: when the cache directory cannot be resolved/used, the
//    wrapper must not throw an unhandled exception — it should print the
//    "binary cache unavailable" warning and fall back to the original
//    binary path.
// ---------------------------------------------------------------------
function testCacheUnavailableFallback() {
  const fx = makeFixture('rush-fallback', { version: '0.1.7', binContent: 'fake-binary-content-fallback' });
  try {
    // Point RUSH_BIN_CACHE at a path that cannot be created as a directory:
    // a path that has a *file* (not a directory) as one of its intermediate
    // path segments. fs.mkdirSync({recursive:true}) will fail with ENOTDIR
    // when trying to create a subdirectory under a plain file. This reliably
    // triggers the wrapper's outer catch without touching any real ACL/perms
    // machinery (which is unreliable to set up portably in a test).
    const blockerFile = path.join(fx.root, 'not-a-directory');
    fs.writeFileSync(blockerFile, 'blocker');
    const bogusCacheRoot = path.join(blockerFile, 'nested', 'cache');

    const env = Object.assign({}, process.env, {
      NODE_PATH: fx.nodeModules,
      RUSH_BIN_CACHE: bogusCacheRoot,
    });
    const result = spawnSync(process.execPath, [RUSH_JS, '--version'], { env, encoding: 'utf8' });

    assert.ok(
      !result.error || result.error.code !== 'undefined',
      'wrapper process failed to launch at all (should have run and fallen back instead): ' +
        (result.error && result.error.message),
    );
    assert.strictEqual(result.status, 1, 'genuine startup failure must remain an exit 1');

    assert.ok(
      /rush: warning: binary cache unavailable/.test(result.stderr || ''),
      'expected the "binary cache unavailable" fallback warning on stderr, got:\n' + (result.stderr || '(empty)'),
    );

    // Fallback means it attempted to launch the ORIGINAL binary path
    // directly (fx.binPath), not a cache copy. Since our fake binary isn't a
    // real executable, spawnSync inside rush.js will itself fail to exec it
    // and rush.js will report that via its own "failed to launch" message
    // referencing fx.binPath — confirming launchTarget fell back correctly.
    assert.ok(
      (result.stderr || '').includes(fx.binPath) || (result.stdout || '').includes(fx.binPath),
      'expected the fallback launch attempt to reference the original binary path ' +
        fx.binPath + ', got stderr:\n' + (result.stderr || '(empty)') + '\nstdout:\n' + (result.stdout || '(empty)'),
    );
  } finally {
    fs.rmSync(fx.root, { recursive: true, force: true, maxRetries: 20, retryDelay: 50 });
  }
}

(async () => {
  section('0. content-hash regression (same size+mtime, swapped content)', testContentHashCatchesSameSizeSameMtimeSwap);
  section('1. key regression (size/mtime change without version bump)', testKeyRegression);
  section('2. cache reuse (no re-copy on unchanged binary)', testCacheReuse);
  await (async () => {
    process.stdout.write('--- 3. concurrent first-launch race ---\n');
    try {
      await testConcurrentFirstLaunch();
      process.stdout.write('PASS: 3. concurrent first-launch race\n');
    } catch (err) {
      failures++;
      process.stdout.write('FAIL: 3. concurrent first-launch race\n');
      process.stdout.write((err && err.stack ? err.stack : String(err)) + '\n');
    }
  })();
  section('4. concurrent different-build launch', testConcurrentDifferentBuildLaunches);
  section('5. abandoned launch directory sweep and basename', testAbandonedLaunchDirectorySweep);
  section('6. warm-cache launch does not copy installed binary', testWarmCacheLaunchDoesNotCopy);
  section('7. copy replacement uses published-byte hash', testCopyReplacementUsesPublishedHash);
  section('8. hardlink fallback', testHardlinkFallback);
  section('9. hardlink pin survives eviction', testHardlinkPinSurvivesEviction);
  section('10. fallback when cache is unavailable', testCacheUnavailableFallback);

  if (failures > 0) {
    process.stderr.write('\n' + failures + ' section(s) FAILED\n');
    process.exit(1);
  }
  process.stdout.write('\nAll sections passed.\n');
  process.exit(0);
})();
