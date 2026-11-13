package agent

import (
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// ownerForRoot returns the owning session recorded for one root by the
// lease ledger, or "" when the ledger holds no entry for it (a nil store
// also reads as ""). This is the attribution reader the finalization
// batch-root holds provider uses: acquire recorded the owner at submit
// time, so the read is one mutex-guarded map lookup with no fallback
// index.
func (s *batchRootLeaseStore) ownerForRoot(root jobbatch.ID) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[root].owner
}
