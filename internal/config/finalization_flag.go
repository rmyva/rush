package config

// The agent finalization opt-in switch for batch agents: the parent's
// final answer is drafted first and only sealed once its children
// finish, its reaction debts settle, and there is no cancellation and
// no open mutation. Lives in its own file so the one-line Options edit
// in config_mcp.go stays the only touch to shared config declarations.

// FinalizationOptions configures agent finalization for batch agents.
// A nil block means the feature is off.
type FinalizationOptions struct {
	// Enabled turns agent finalization on for batch agents. Pointer so
	// "absent" stays distinguishable from an explicit false; both mean
	// the feature is off.
	Enabled *bool `json:"enabled,omitempty" jsonschema:"description=Enable agent finalization for batch agents. Default false.,default=false"`
}

// AgentFinalizationEnabled reports whether agent finalization is enabled.
// Nil options, a nil Finalization block and a nil Enabled pointer all
// mean the feature is off, so callers never need a nil check.
func (o *Options) AgentFinalizationEnabled() bool {
	if o == nil || o.Finalization == nil || o.Finalization.Enabled == nil {
		return false
	}
	return *o.Finalization.Enabled
}
