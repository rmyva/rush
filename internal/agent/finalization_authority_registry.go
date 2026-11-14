package agent

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/session"
)

// FinalizationAuthorityRegistry maps an owner session id to the in-process
// finalization authority bound at that owner's root-run start. The batch
// commit hook runs on runner goroutines without the submitting call's
// context, so this registry is the only way a per-run authority can admit a
// commit. The composition root builds ONE registry when
// options.finalization.enabled and shares it between the wiring and the
// batch commit identity. The zero value is not usable; build through
// NewFinalizationAuthorityRegistry. All methods are nil-receiver safe.
type FinalizationAuthorityRegistry struct {
	mu      sync.Mutex
	byOwner map[string]*session.AgentFinalizationAuthority
}

func NewFinalizationAuthorityRegistry() *FinalizationAuthorityRegistry {
	return &FinalizationAuthorityRegistry{byOwner: make(map[string]*session.AgentFinalizationAuthority)}
}

// Register stores the authority under owner and returns the unregister
// callback. The callback removes the entry only while it still holds THIS
// authority (pointer comparison), so a stale unregister can never drop a
// newer registration for the same owner.
func (r *FinalizationAuthorityRegistry) Register(owner string, authority *session.AgentFinalizationAuthority) func() {
	if r == nil || owner == "" || authority == nil {
		return func() {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byOwner[owner] = authority
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.byOwner[owner] == authority {
				delete(r.byOwner, owner)
			}
		})
	}
}

// Lookup returns the registered authority for owner, or nil when none is
// registered.
func (r *FinalizationAuthorityRegistry) Lookup(owner string) *session.AgentFinalizationAuthority {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byOwner[owner]
}
