package tools

import (
	"context"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/fsext"
)

// osDisk is the stateless, real-filesystem DiskProvider: the exact
// os/filepath and search calls the fs_* family made before the provider
// seam existed, with no behavioural difference whatsoever. It has no
// state, so it is safe to hand out as a package-level singleton.
type osDisk struct{}

// osDiskMarker identifies the canonical real-filesystem provider without
// using interface equality as the identity mechanism. The unexported method
// prevents providers outside this package from accidentally claiming the
// canonical identity.
type osDiskMarker interface {
	isOSDisk()
}

func (osDisk) isOSDisk() {}

var osDiskSingleton DiskProvider = osDisk{}

// OSDisk returns the real-filesystem DiskProvider: the default every
// fs_* constructor falls back to when no DiskProvider is supplied.
func OSDisk() DiskProvider { return osDiskSingleton }

// HasDiskProvider reports whether d is a non-nil interface value containing a
// provider. The type switch keeps this presence check separate from the
// canonical-identity check below; it does not impose a comparability
// requirement on a provider's dynamic value.
func HasDiskProvider(d DiskProvider) bool {
	switch d.(type) {
	case nil:
		return false
	default:
		return true
	}
}

// IsOSDisk reports whether d is the canonical real-filesystem provider by
// asserting the private marker interface. The marker is useful as an
// explicit identity predicate and avoids making identity depend on interface
// equality. It is not a workaround for a reachable panic in the former
// comparisons: comparison with nil is always safe, and a custom provider has
// a different dynamic type from the comparable osDisk value.
func IsOSDisk(d DiskProvider) bool {
	_, ok := d.(osDiskMarker)
	return ok
}

// IsCustomDiskProvider reports whether d is a non-nil provider other than
// the canonical real filesystem.
func IsCustomDiskProvider(d DiskProvider) bool {
	return HasDiskProvider(d) && !IsOSDisk(d)
}

func (osDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (osDisk) Lstat(_ context.Context, name string) (fs.FileInfo, error) {
	return os.Lstat(name)
}

func (osDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	return filepath.EvalSymlinks(name)
}

func (osDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	return os.Open(name)
}

func (osDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	return os.ReadFile(name)
}

func (osDisk) MkdirAll(_ context.Context, dir string, perm fs.FileMode) error {
	return os.MkdirAll(dir, perm)
}

func (osDisk) WriteFile(_ context.Context, name string, data []byte, perm fs.FileMode) error {
	return fsext.AtomicWriteFile(name, data, perm)
}

func (osDisk) Remove(_ context.Context, name string) error {
	return os.Remove(name)
}

func (osDisk) List(_ context.Context, req ListRequest) (ListResult, error) {
	entries, truncated, err := fsext.ListDirectory(req.Dir, req.IgnorePatterns, req.Depth, req.Limit)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Entries: entries, Truncated: truncated}, nil
}

func (osDisk) Find(ctx context.Context, req FindRequest) (FindResult, error) {
	paths, truncated, err := globFiles(ctx, req.Pattern, req.Dir, req.Limit)
	if err != nil {
		return FindResult{}, err
	}
	return FindResult{Paths: paths, Truncated: truncated}, nil
}

// Search reproduces today's fs_grep dispatch verbatim: ripgrep invoked
// with -C N when available, falling back to the in-process regex walk
// when rg is unavailable or its run fails (fsGrepSearchContext, defined
// in fs_grep.go, already implements both engines and the handover
// between them — that dispatch and its clear(files) reset on fallback
// stay untouched here). Results land in a local map + budget and are
// then flattened into []SearchLine.
//
// Text normalisation is preserved verbatim, INCLUDING its existing
// inconsistency between the two engines: the rg parser stores
// strings.TrimSpace(...) (parseRipgrepContextStream) while the fallback
// stores strings.TrimSuffix(line, "\r") plus fallbackTruncateSuffix on
// overlong lines (scanFileWithContext) — leading whitespace survives on
// one path and not the other. Do not "clean this up": it would change
// rendered output that existing tests pin.
//
// req.MaxLines is only a hint (see SearchRequest.MaxLines): when > 0 it
// caps this call's local budget so the search does not do unbounded
// work; when 0, the local budget is effectively unlimited and the real
// truncation is left entirely to the caller, which re-applies its own
// budget (fsGrepFileHits.add) to the returned lines.
func (osDisk) Search(ctx context.Context, req SearchRequest) (DiskSearchResult, error) {
	remaining := req.MaxLines
	if remaining <= 0 {
		remaining = math.MaxInt
	}
	budget := fsGrepBudget{remaining: remaining}

	files := make(map[string]*fsGrepFileHits)
	if err := fsGrepSearchContext(ctx, req.Pattern, req.Dir, req.Include,
		req.ContextLines, files, &budget); err != nil {
		return DiskSearchResult{}, err
	}

	var lines []SearchLine
	for path, collector := range files {
		for lineNum, line := range collector.lines {
			lines = append(lines, SearchLine{
				Path: path,
				Line: lineNum,
				Text: line.text,
				Hit:  line.hit,
			})
		}
	}
	return DiskSearchResult{Lines: lines}, nil
}
