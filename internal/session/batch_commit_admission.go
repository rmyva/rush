package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/google/uuid"
)

type batchFinalizationTicket struct {
	identity AgentFinalizationIdentity
	ticket   string
}

type batchFinalizationAdmission struct {
	ancestry finalizationAncestry
	tickets  []batchFinalizationTicket
}

// admitBatchCommitOwners resolves one ordered, distinct ticket set before SQL.
func admitBatchCommitOwners(store *BatchStore, ctx context.Context, id BatchCommitIdentity, transition jobbatch.Transition, owners []string) (func() error, *batchFinalizationAdmission, error) {
	if len(owners) == 0 {
		return nil, nil, nil
	}
	owners = slices.Clone(owners)
	slices.Sort(owners)
	owners = slices.Compact(owners)
	if owners[0] == "" {
		return nil, nil, errors.New("batch commit admission: missing owner")
	}

	if id.finalizationService == nil {
		if len(id.finalizationAuthorities) != 0 {
			return nil, nil, errors.New("batch commit admission: authority service missing")
		}
		if id.AdmitOwners == nil {
			return nil, nil, nil
		}
		release, err := id.AdmitOwners(ctx, transition, owners)
		if err == nil && release == nil {
			return nil, nil, errors.New("batch commit admission: missing release")
		}
		return release, nil, err
	}

	service := id.finalizationService
	if service.db == nil || store == nil || store.writer != service.db {
		return nil, nil, errors.New("batch commit admission: authority and batch writer must share the service database")
	}
	ancestry, err := readFinalizationAncestry(ctx, service.db, owners)
	if err != nil {
		return nil, nil, fmt.Errorf("batch commit admission: resolve managed ancestors: %w", err)
	}
	owners = ancestry.owners
	authorities := make(map[string]*AgentFinalizationAuthority, len(id.finalizationAuthorities))
	for _, authority := range id.finalizationAuthorities {
		if authority == nil || authority.service != service || authority.service.db != service.db || !authority.identity.valid() {
			return nil, nil, errors.New("batch commit admission: conflicting authority binding")
		}
		owner := authority.identity.SessionID
		if _, exists := authorities[owner]; exists {
			return nil, nil, fmt.Errorf("batch commit admission: duplicate authority for owner %q", owner)
		}
		authorities[owner] = authority
	}

	tickets := make([]batchFinalizationTicket, 0, len(owners))
	defer func() {
		if value := recover(); value != nil {
			if release := finalizationTicketRelease(ctx, service, tickets); release != nil {
				_ = release() // A failed release remains a durable hold.
			}
			panic(value)
		}
	}()
	for _, owner := range owners {
		authority := authorities[owner]
		if authority == nil && id.AuthorityFor != nil {
			authority = id.AuthorityFor(owner)
			if authority != nil && (authority.service != service || authority.service.db != service.db || !authority.identity.valid()) {
				return finalizationTicketRelease(ctx, service, tickets), nil, errors.New("batch commit admission: conflicting authority binding")
			}
		}
		if authority == nil {
			exists, err := service.hasFinalizationScope(ctx, owner)
			if err != nil {
				return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: inspect owner %q: %w", owner, err)
			}
			if exists {
				return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: explicit authority required for owner %q", owner)
			}
			continue
		}
		if err := authority.revalidate(ctx); err != nil {
			return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: authority for owner %q: %w", owner, err)
		}
		ticket := uuid.NewString()
		store := NewAgentFinalizationStore(service.db)
		if _, err := store.Admit(ctx, authority.identity, ticket); err != nil {
			return finalizationTicketRelease(ctx, service, tickets), nil, fmt.Errorf("batch commit admission: admit owner %q: %w", owner, err)
		}
		tickets = append(tickets, batchFinalizationTicket{identity: authority.identity, ticket: ticket})
	}

	var callbackRelease func() error
	if id.AdmitOwners != nil {
		var err error
		callbackRelease, err = id.AdmitOwners(ctx, transition, owners)
		if err != nil {
			callbackReleases := combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, service, tickets))
			if callbackRelease == nil {
				return callbackReleases, nil, errors.Join(err, errors.New("batch commit admission: callback error without release"))
			}
			return callbackReleases, nil, err
		}
		if callbackRelease == nil {
			return finalizationTicketRelease(ctx, service, tickets), nil, errors.New("batch commit admission: missing release")
		}
	}
	admission := &batchFinalizationAdmission{ancestry: ancestry, tickets: slices.Clone(tickets)}
	return combineTicketReleases(callbackRelease, finalizationTicketRelease(ctx, service, tickets)), admission, nil
}

func finalizationTicketRelease(ctx context.Context, service *service, tickets []batchFinalizationTicket) func() error {
	if len(tickets) == 0 {
		return nil
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			for _, ticket := range tickets {
				if err := NewAgentFinalizationStore(service.db).Release(cleanupCtx, ticket.identity, ticket.ticket); err != nil {
					releaseErr = errors.Join(releaseErr, fmt.Errorf("batch commit admission: release owner %q: %w", ticket.identity.SessionID, err))
				}
			}
		})
		return releaseErr
	}
}

func combineTicketReleases(first, second func() error) func() error {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			firstErr := first()
			secondErr := second()
			releaseErr = errors.Join(firstErr, secondErr)
		})
		return releaseErr
	}
}

func batchCommitReleaseError(primary, release error) error {
	if release == nil {
		return primary
	}
	if primary == nil {
		return release
	}
	return errors.Join(primary, release)
}
