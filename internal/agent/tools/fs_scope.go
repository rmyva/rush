package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/permission"
)

// resolveScopedPath resolves a raw (absolute or workingDir-relative) path
// into an absolute, symlink-resolved form suitable for
// permission.FolderScope.Check (or for pre-resolving a
// FolderScopeEntry.Dir before compilation).
//
// Only the longest EXISTING prefix of the path is symlink-resolved; the
// non-existent tail is appended unchanged. This produces a deliberate
// asymmetry: a create on a not-yet-existing file is judged by its
// RESOLVED PARENT directory (catching a symlinked parent that escapes a
// granted scope), while a read/write/delete on an existing file is
// judged by its own fully-resolved path (catching a symlink pointing
// outside the scope directly). Any ambiguity — an unreadable component,
// a file used as a directory, or a failed symlink evaluation — is an
// error, never a silently-produced path, so consumers fail closed.
//
// workingDir/raw are joined via filepathext.SmartJoin, then made
// absolute WITHOUT calling filepath.Abs whenever the joined result is
// already filepathext.SmartIsAbs (R5-6, P1 review finding). This
// matters because filepath.Abs is not a safe no-op on an
// already-absolute path on every OS: on Windows it unconditionally
// normalizes through syscall.FullPath, which silently re-roots a
// SmartIsAbs-but-driveless path (a leading "/" with no drive letter --
// e.g. a caller-supplied FolderScope entry spelled Unix-style against
// their own virtual DiskProvider) against the RUSH HOST PROCESS's own
// current drive/working directory. The same request could then resolve
// to a different real path if the host process's CWD changes between
// calls -- exactly the leak this guards against. A genuinely relative
// joined result with no workingDir at all has no safe root to resolve
// against, so it is rejected with an explicit error instead of silently
// falling back to the host process's CWD.
//
// LibraryVirtualRoot (this file) is no longer an example of the
// SmartIsAbs-but-driveless case above (R6-1 fix): it is now computed
// per-OS to satisfy the REAL filepath.IsAbs on every platform, so it
// takes the plain filepathext.SmartIsAbs(joined) branch just like any
// other genuinely absolute path. The guard immediately below is what
// actually keeps it inert: it fails closed before the first real
// disk.Stat whenever the resolved path would land under that sentinel
// AND disk is the real OSDisk.
func resolveScopedPath(ctx context.Context, disk DiskProvider, workingDir, raw string) (abs string, err error) {
	disk = diskOrOS(disk)
	joined := filepathext.SmartJoin(workingDir, raw)

	var cleaned string
	switch {
	case filepathext.SmartIsAbs(joined):
		cleaned = filepath.Clean(joined)
	case workingDir == "":
		return "", fmt.Errorf(
			"fs: resolve %q: relative path with no working directory to resolve it against", raw)
	default:
		cleaned, err = filepath.Abs(joined)
		if err != nil {
			return "", fmt.Errorf("fs: resolve %q: %w", raw, err)
		}
		cleaned = filepath.Clean(cleaned)
	}

	// R6-1 (P0 review finding): fail closed BEFORE the first real disk.Stat
	// call below when this resolution would touch the real filesystem
	// under the ephemeral library-mode sentinel. LibraryVirtualRoot is a
	// synthesized WorkingDir placeholder, never a real sandbox; a caller's
	// own DiskProvider is unaffected (see the helper's doc).
	if err := rejectRealDiskUnderLibraryVirtualRoot(disk, cleaned); err != nil {
		return "", err
	}

	// Walk up to the longest existing prefix. Anything other than a
	// clean miss (EACCES, ELOOP, ...) is unresolvable: fail closed.
	prefix := cleaned
	for {
		info, statErr := disk.Stat(ctx, prefix)
		if statErr == nil {
			if !info.IsDir() && prefix != cleaned {
				return "", fmt.Errorf(
					"fs: resolve %q: path component is a file, not a directory: %s",
					raw, prefix)
			}
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf("fs: resolve %q: stat %s: %w", raw, prefix, statErr)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			break
		}
		prefix = parent
	}

	resolvedPrefix, err := disk.EvalSymlinks(ctx, prefix)
	if err != nil {
		// A prefix we cannot evaluate cannot be judged safely.
		return "", fmt.Errorf("fs: resolve %q: evaluate symlinks in %s: %w",
			raw, prefix, err)
	}

	tail, err := filepath.Rel(prefix, cleaned)
	if err != nil {
		return "", fmt.Errorf("fs: resolve %q: %w", raw, err)
	}
	return filepath.Join(resolvedPrefix, tail), nil
}

// CanonicalizeFolderScopeSpec resolves spec.WorkingDir and every entry's
// Dir through resolveScopedPath's own longest-existing-prefix +
// EvalSymlinks algorithm — the SAME algorithm and the SAME disk provider
// every REQUESTED item path is resolved through before a
// permission.FolderScope ever sees it (R5-2, P0 security review finding).
//
// Production folder-scope compilation used to only filepath.Join a
// relative entry onto WorkingDir and filepath.Clean the result: no
// symlink resolution, no provider consultation. Every requested path,
// meanwhile, went through resolveScopedPath first. A scope's granted and
// denied roots and an actual request's item path could therefore land in
// two different namespaces — one lexical, one symlink-resolved — letting
// a symlinked deny carve-out silently stop matching while its broader
// parent grant kept matching. Calling this before
// permission.BuildFolderScope closes that gap: both sides of every
// FolderScope.Check are now produced by the same code path, in the same
// namespace.
//
// disk selects the filesystem roots are canonicalized against; nil means
// the real OS disk (see diskOrOS). A spec compiled with a caller-supplied
// DiskProvider must be canonicalized with that SAME provider — a
// durable-queue rebuild, which never carries a DiskProvider (see
// CallOptions.DiskProvider's doc comment), always canonicalizes with the
// real disk instead.
//
// Malformed entries (an empty Dir, or a relative Dir with an empty
// WorkingDir) are passed through UNCHANGED: permission.BuildFolderScope
// still owns rejecting them with its existing error text, and resolving
// either case here would either produce an uninformative error or
// silently invent the process's own working directory behind the host's
// back. Any OTHER resolution error (an unreadable path component, a
// failed symlink evaluation, ...) is returned to the caller, which MUST
// treat it exactly like a BuildFolderScope compile error: never
// substitute a partially-resolved spec, and never let the failure be
// mistaken for "no scope" — the zero FolderScope denies everything.
//
// Residual boundary, not addressed here (out of scope): this resolves
// every path once, before the matcher compiles and before any operation
// runs. A hostile local process that swaps a symlink between this
// resolution and the later filesystem operation (TOCTOU) is not defended
// against; closing that would need handle-relative filesystem
// primitives. Reopening the resolved pathname can therefore observe a
// different target.
func CanonicalizeFolderScopeSpec(ctx context.Context, disk DiskProvider, spec permission.FolderScopeSpec) (permission.FolderScopeSpec, error) {
	disk = diskOrOS(disk)

	out := permission.FolderScopeSpec{
		WorkingDir:       spec.WorkingDir,
		KeepCommandTools: spec.KeepCommandTools,
	}

	workingDir := strings.TrimSpace(spec.WorkingDir)
	if workingDir != "" {
		resolved, err := resolveScopedPath(ctx, disk, "", spec.WorkingDir)
		if err != nil {
			return permission.FolderScopeSpec{}, fmt.Errorf(
				"folder scope: resolve WorkingDir %q: %w", spec.WorkingDir, err)
		}
		out.WorkingDir = resolved
		workingDir = resolved
	}

	if len(spec.Entries) == 0 {
		return out, nil
	}
	out.Entries = make([]permission.FolderScopeEntry, len(spec.Entries))
	for i, entry := range spec.Entries {
		dir := strings.TrimSpace(entry.Dir)
		if dir == "" || (!filepath.IsAbs(dir) && workingDir == "") {
			// Leave it exactly as BuildFolderScope will see it: it owns
			// rejecting an empty Dir or a relative Dir with no
			// WorkingDir with its own error text.
			out.Entries[i] = entry
			continue
		}
		resolved, err := resolveScopedPath(ctx, disk, workingDir, entry.Dir)
		if err != nil {
			return permission.FolderScopeSpec{}, fmt.Errorf(
				"folder scope entry %d: resolve %q: %w", i, entry.Dir, err)
		}
		out.Entries[i] = permission.FolderScopeEntry{Dir: resolved, Ops: entry.Ops}
	}
	return out, nil
}
