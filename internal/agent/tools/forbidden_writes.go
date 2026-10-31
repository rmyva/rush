package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ForbiddenWritesEnv is the env var orchestrators set to blacklist
// specific paths from the write / edit / multiedit tools. Comma-separated
// list of absolute or working-dir-relative paths. Designed to be set by
// a parent process before exec'ing `rush run`, so a wrapper like
// `rush run --json > /tmp/out.json` can guarantee the model can't
// collide with its own stdout-redirect target — the failure mode where
// the JSON envelope ended up on line 1 and write-tool content on lines
// 2+ in the same file, observed in the shamir-db .tmp-audit-D.json case.
const ForbiddenWritesEnv = "RUSH_FORBID_WRITES"

// CheckForbiddenWrite returns an error if path is on the comma-separated
// RUSH_FORBID_WRITES env-var blacklist. Comparison uses absolute, cleaned
// paths; case-insensitive on Windows. Empty env var means no
// restrictions.
func CheckForbiddenWrite(path string) error {
	return checkForbiddenWrite(path, normalizeForbiddenPath)
}

// CheckForbiddenWriteResolved checks the target and blacklist entries after
// resolving their longest existing path prefix through symlinks. Call this
// immediately before the filesystem mutation.
func CheckForbiddenWriteResolved(path string) error {
	raw := os.Getenv(ForbiddenWritesEnv)
	if raw == "" {
		return nil
	}
	target, err := normalizeResolvedForbiddenPath(path)
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		forbidden, err := normalizeResolvedForbiddenPath(entry)
		if err != nil {
			return fmt.Errorf("cannot resolve forbidden write path %q: %w", entry, err)
		}
		if forbidden == target {
			return forbiddenWriteError(path)
		}
	}
	return nil
}

func checkForbiddenWrite(path string, normalize func(string) string) error {
	raw := os.Getenv(ForbiddenWritesEnv)
	if raw == "" {
		return nil
	}
	target := normalize(path)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" && normalize(entry) == target {
			return forbiddenWriteError(path)
		}
	}
	return nil
}

func forbiddenWriteError(path string) error {
	return fmt.Errorf(
		"write to %q forbidden by %s (this path is on the orchestrator's blacklist — typically a shell stdout-redirect target). Return the content as your tool output or in final_text, or write to a different path",
		path, ForbiddenWritesEnv,
	)
}

func normalizeResolvedForbiddenPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	prefix := abs
	for {
		resolved, resolveErr := filepath.EvalSymlinks(prefix)
		if resolveErr == nil {
			tail, relErr := filepath.Rel(prefix, abs)
			if relErr != nil {
				return "", relErr
			}
			return normalizeForbiddenPath(filepath.Join(resolved, tail)), nil
		}
		if !os.IsNotExist(resolveErr) {
			return "", resolveErr
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return "", resolveErr
		}
		prefix = parent
	}
}

func normalizeForbiddenPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		return strings.ToLower(abs)
	}
	return abs
}
