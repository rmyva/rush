package agent

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session"
)

// newFinalizationPhysicalHoldsFacts is the single assembly point W1 wires:
// it builds the composite physical-holds facts reader over the in-process
// providers of one installed batch runtime and folds in the host facts
// reader via the Sum combinator. The sources are exactly three: batch
// roots (ledger-attributed through the root lease store), tracked process
// trees (process-level conservative over-count), and host facts (the
// session-package transaction reader). A nil install or a nil host facts
// reader fails closed with an error instead of panicking or silently
// reading zero.
func newFinalizationPhysicalHoldsFacts(install *BatchInstall, hostFacts session.FinalizationPhysicalHoldsFacts) (session.FinalizationPhysicalHoldsFacts, error) {
	if install == nil {
		return nil, fmt.Errorf("%w: batch install is nil",
			ErrFinalizationHoldsRegistryUnreadable)
	}
	if hostFacts == nil {
		return nil, fmt.Errorf("%w: finalization physical holds host facts are nil",
			session.ErrFinalizationPhysicalHoldsUnreadable)
	}
	composite := session.NewFinalizationPhysicalHoldsFactsFromProviders(
		NewFinalizationBatchRootHoldsProviderForInstall(install),
		NewFinalizationTrackedProcessHoldsProvider(),
	)
	return session.NewFinalizationPhysicalHoldsFactsSum(composite, hostFacts), nil
}
