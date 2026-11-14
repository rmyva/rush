package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// ErrBatchValueOverflow rejects uint64 Token/Sequence values at or above 2^63:
// they cannot be stored in SQL INTEGER columns without truncation, so the
// commit is refused before any DB write (documented sentinel).
var ErrBatchValueOverflow = errors.New("batch commit: uint64 value exceeds sql int64 range")

// BatchCommitIdentity is the Runner identity a commit hook writes under:
// host identity plus the fresh Runner incarnation UUID, and the owner session
// whose Submit reservations this Runner creates (one Runner, one owner).
type BatchCommitIdentity struct {
	Host, Runner, Owner string

	// R11F (P1-1), minimal additive hunk: optional per-Submit owner
	// resolver. When non-nil, the Submit reservation is written under the
	// returned owner — the INVOKING session carried in the one-item spec
	// payload — instead of the fixed Owner (which remains only a dormant
	// default). An empty return keeps the fixed Owner. Non-Submit
	// transitions are unaffected: batchCommitProject already resolves each
	// root's owner from the row (RootOwner).
	OwnerFor func(spec jobbatch.Spec) string

	// AuthorityFor resolves the runner-wide finalization authority for one
	// owner at commit time (the process registry the composition root
	// installs). The commit hook runs on runner goroutines without the
	// submitting call's context, so a per-run authority is reachable only
	// through this lookup. Nil keeps the exact legacy behavior: an owner
	// with a bound scope but no static authority stays refused.
	AuthorityFor func(owner string) *AgentFinalizationAuthority

	// AdmitOwners runs after durable tickets and before batch writer I/O. Its
	// error-returning release is attempted after success or failure.
	AdmitOwners func(context.Context, jobbatch.Transition, []string) (func() error, error)

	finalizationService     *service
	finalizationAuthorities []*AgentFinalizationAuthority
}

// NewBatchCommitHook returns a RunnerConfig.CommitHook adapter. It maps one
// accepted Transition into one BatchProjection per touched root inside ONE
// store transaction (Submit: one atomic reservation-plus-view transaction).
// It reads no unpublished Runner views, dispatches nothing, holds no locks
// across writer I/O, never retries or reopens a faulted writer, and never
// synthesizes terminal outcomes. Errors are returned as-is, unwrapped.
func NewBatchCommitHook(store *BatchStore, id BatchCommitIdentity) func(context.Context, jobbatch.Transition) error {
	// Closing stays sticky once a HostClosing transition has committed: later
	// Settled settlements of interrupted tasks must also wake=0. Commits are
	// serialized by the Runner, so a plain bool is safe.
	closing := false
	return func(ctx context.Context, t jobbatch.Transition) error {
		activeIdentity, err := batchFinalizationIdentityFromContext(ctx, id)
		if err != nil {
			return err
		}
		err = batchCommitTransition(ctx, store, activeIdentity, closing, t)
		if err == nil && t.Event.Type == jobbatch.EventHostClosing {
			closing = true
		}
		return err
	}
}

const batchCommitInt63 = uint64(1) << 63

func batchCommitUint64(v uint64) (int64, error) {
	if v >= batchCommitInt63 {
		return 0, ErrBatchValueOverflow
	}
	return int64(v), nil
}

func batchCommitTransition(ctx context.Context, store *BatchStore, id BatchCommitIdentity, closing bool, t jobbatch.Transition) error {
	// Sequence 0 is never a real Runner transition; guard it like overflow.
	if t.Sequence == 0 || t.Sequence >= batchCommitInt63 {
		return ErrBatchValueOverflow
	}
	for _, v := range t.Changed {
		if _, err := batchCommitUint64(v.Token); err != nil {
			return err
		}
	}
	if t.Event.Type == jobbatch.EventSubmit {
		return batchCommitSubmit(ctx, store, id, t)
	}
	return batchCommitProject(ctx, store, id, closing, t)
}

// batchCommitSubmit reserves the complete accepted tree under the Runner's
// exact root ID with its initial Changed views written by the same insert, so
// reservation and initial projection are atomic (R5(a)). Live attachment is
// keyed by (owner, tool_call_id=e.Ref); the input hash is the canonical hash
// of the accepted Spec, so concurrent matching retries attach once and
// mismatched Specs are refused with ErrBatchInputMismatch.
func batchCommitSubmit(ctx context.Context, store *BatchStore, id BatchCommitIdentity, t jobbatch.Transition) (retErr error) {
	e := t.Event
	// P1-1: the reservation's owner_session_id REFERENCES sessions(id), so
	// it must be the invoking session, never the host UUID. The resolver
	// reads the owner out of the accepted spec (batchLeafPayload.SessionID).
	owner := id.Owner
	if id.OwnerFor != nil {
		if resolved := id.OwnerFor(e.Spec); resolved != "" {
			owner = resolved
		}
	}
	views := make(map[jobbatch.ID]jobbatch.NodeView, len(t.Changed))
	for _, v := range t.Changed {
		views[v.ID] = v
	}
	root, err := batchSpecNode(e.Spec)
	if err != nil {
		return err
	}
	if err := batchSpecOverlay(&root, string(e.Root), views); err != nil {
		return err
	}
	notices := make([]db.SelectBatchNodeNoticeParams, len(t.Notices))
	for i, n := range t.Notices {
		p, err := batchCommitNotice(n, i, false)
		if err != nil {
			return err
		}
		notices[i] = p
	}
	release, admission, err := admitBatchCommitOwners(store, ctx, id, t, []string{owner})
	if release != nil {
		defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	res, err := store.ReserveIdentity(ctx, BatchReservation{
		finalizationAdmission: admission,
		Owner:                 owner, Host: id.Host, RunnerID: id.Runner, RootID: string(e.Root),
		ToolCallID: e.Ref, InputHash: batchCommitSpecHash(e.Spec),
		OriginCLI: len(e.Ref) >= len("batch-cli:") && e.Ref[:len("batch-cli:")] == "batch-cli:",
		Sequence:  int64(t.Sequence), Root: root, Notices: notices,
		Checkpoint: &db.UpdateBatchRunnerCheckpointParams{
			HostID: id.Host, RunnerID: id.Runner, Sequence: int64(t.Sequence),
			EventType: batchCommitCheckpointEvent(t), Final: batchCommitFlag(t.Final),
			UpdatedAt: time.Now().Unix(),
		},
	})
	if err != nil {
		return err
	}
	if res.Snapshot.Root.RootID != string(e.Root) {
		return ErrBatchProjectionConflict
	}
	return nil
}

func batchCommitSpecHash(s jobbatch.Spec) string {
	raw, err := json.Marshal(s)
	if err != nil {
		raw = []byte("unmarshalable")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// batchSpecNode maps one immutable accepted Spec onto reservation values.
// Task kind, group, payload and timeout are reserved here because Changed
// views never describe them (R5(e); existing columns task_kind, task_group,
// task_payload and timeout_ns suffice — no schema change needed).
func batchSpecNode(s jobbatch.Spec) (BatchReservationNode, error) {
	var v db.ReserveBatchNodeParams
	var n BatchReservationNode
	switch {
	case s.Task != nil:
		v.Kind = "task"
		v.TaskKind = sql.NullString{String: s.Task.Kind, Valid: true}
		v.TaskGroup = s.Task.Group
		if len(s.Task.Payload) > 0 {
			v.TaskPayload = sql.NullString{String: string(s.Task.Payload), Valid: true}
		}
		v.TimeoutNs = int64(s.Task.Timeout)
		v.State, v.ControlState = "pending", "none"
	case s.Batch != nil:
		v.Kind = "batch"
		v.Mode = sql.NullString{String: string(s.Batch.Mode), Valid: true}
		if s.Batch.OnFail != "" {
			v.OnFail = sql.NullString{String: string(s.Batch.OnFail), Valid: true}
		}
		if s.Batch.Notify != "" {
			v.Notify = sql.NullString{String: string(s.Batch.Notify), Valid: true}
		}
		v.MaxParallel = int64(s.Batch.MaxParallel)
		v.State, v.ControlState = "pending", "none"
		n.Children = make([]BatchReservationNode, len(s.Batch.Items))
		for i, item := range s.Batch.Items {
			c, err := batchSpecNode(item)
			if err != nil {
				return n, err
			}
			n.Children[i] = c
		}
	default:
		return n, jobbatch.ErrInvalidSpec
	}
	n.Values = v
	return n, nil
}

// batchSpecOverlay applies each reserved node's initial Changed view so the
// reservation insert already projects the accepted initial state.
func batchSpecOverlay(n *BatchReservationNode, path string, views map[jobbatch.ID]jobbatch.NodeView) error {
	values, err := batchNodeReserveValues(n.Values, views[jobbatch.ID(path)])
	if err != nil {
		return err
	}
	n.Values = values
	for i := range n.Children {
		if err := batchSpecOverlay(&n.Children[i], fmt.Sprintf("%s.%d", path, i+1), views); err != nil {
			return err
		}
	}
	return nil
}

func batchNodeReserveValues(v db.ReserveBatchNodeParams, view jobbatch.NodeView) (db.ReserveBatchNodeParams, error) {
	if view.ID == "" {
		return v, nil
	}
	v.State, v.Cause = string(view.State), string(view.Cause)
	var err error
	if v.Token, err = batchCommitUint64(view.Token); err != nil {
		return v, err
	}
	v.Attempts = int64(view.Attempts)
	v.ControlState = batchCommitControlState(view.Control)
	v.ControlError = view.ControlError
	if view.ControlPending {
		v.ControlPending = 1
	}
	v.OutcomeState, v.OutcomeCode, v.OutcomeSummary = string(view.Outcome.State), view.Outcome.Code, view.Outcome.Summary
	// Batch nodes store the ENGINE-NORMALIZED policy (Notify ""→"all",
	// sequential OnFail ""→"stop"), never the raw Spec values.
	if view.Kind == jobbatch.KindBatch {
		v.Mode = sql.NullString{String: string(view.Mode), Valid: true}
		v.OnFail = sql.NullString{String: string(view.OnFail), Valid: true}
		v.Notify = sql.NullString{String: string(view.Notify), Valid: true}
		v.MaxParallel = int64(view.MaxParallel)
	}
	return v, nil
}

func batchCommitControlState(c jobbatch.ControlState) string {
	if c == "" {
		return string(jobbatch.ControlNone)
	}
	return string(c)
}

// batchCommitProject groups Changed and Notices by root into one
// BatchProjection per root set inside one store transaction. Add appends
// subtrees at accepted parent ordinals inside Project without a root Reserve;
// Release and transitions touching other roots group exactly like any event.
func batchCommitProject(ctx context.Context, store *BatchStore, id BatchCommitIdentity, closing bool, t jobbatch.Transition) (retErr error) {
	e := t.Event
	groups := make(map[jobbatch.ID]*BatchRootProjection)
	group := func(root jobbatch.ID) *BatchRootProjection {
		g := groups[root]
		if g == nil {
			g = &BatchRootProjection{}
			groups[root] = g
		}
		return g
	}
	views := make(map[jobbatch.ID]jobbatch.NodeView, len(t.Changed))
	for _, v := range t.Changed {
		views[v.ID] = v
	}
	// Created subtrees are inserted at their accepted ordinals with their
	// whole final views written by the insert; their Changed views are never
	// re-written as updates (the update fence would find 0 rows).
	inserted := make(map[jobbatch.ID]bool)
	if e.Type == jobbatch.EventAdd {
		item := 0
		for _, v := range t.Changed {
			if v.Parent != e.Parent {
				continue
			}
			if item >= len(e.Items) {
				return jobbatch.ErrInvalidSpec
			}
			node, err := batchSpecNode(e.Items[item])
			if err != nil {
				return err
			}
			if err := batchSpecOverlay(&node, string(v.ID), views); err != nil {
				return err
			}
			batchCollectInserted(&node, string(v.ID), inserted)
			g := group(v.Root)
			g.Additions = append(g.Additions, BatchAddition{Parent: string(e.Parent), Ordinal: int64(v.Ordinal), Node: node})
			item++
		}
	}
	for _, v := range t.Changed {
		if e.Type == jobbatch.EventAdd && v.Parent == e.Parent {
			continue
		}
		if inserted[v.ID] {
			continue
		}
		update, err := batchNodeUpdate(v)
		if err != nil {
			return err
		}
		g := group(v.Root)
		g.Updates = append(g.Updates, update)
	}
	// Settled events persist the full response envelope (State, Code, Summary,
	// Details) and its reference on the reported node's row (R5(d): the new
	// response_envelope column — no existing column could hold the opaque
	// Details payload; see batch_commit_doc.go). COALESCE in ProjectBatchNode
	// preserves it across later unrelated updates of the same row.
	if e.Type == jobbatch.EventSettled {
		envelope, err := json.Marshal(batchCommitEnvelope{
			State: string(e.Result.State), Code: e.Result.Code,
			Summary: e.Result.Summary, Details: e.Result.Details,
		})
		if err != nil {
			return err
		}
		for root, g := range groups {
			for i := range g.Updates {
				u := &g.Updates[i]
				if u.NodeID == string(e.Node) {
					u.ResponseEnvelope = sql.NullString{String: string(envelope), Valid: true}
					u.ResponseEnvelopeRef = sql.NullString{String: fmt.Sprintf("%s#%d", e.Node, t.Sequence), Valid: true}
				}
			}
			_ = root
		}
	}
	for i, n := range t.Notices {
		notice, err := batchCommitNotice(n, i, closing)
		if err != nil {
			return err
		}
		group(n.Root).Notices = append(group(n.Root).Notices, notice)
	}
	p := BatchProjection{Host: id.Host, RunnerID: id.Runner, Sequence: int64(t.Sequence)}
	p.Checkpoint = &db.UpdateBatchRunnerCheckpointParams{
		HostID: id.Host, RunnerID: id.Runner, Sequence: int64(t.Sequence),
		EventType: batchCommitCheckpointEvent(t), Final: batchCommitFlag(t.Final),
		UpdatedAt: time.Now().Unix(),
	}
	owners := make([]string, 0, len(groups))
	for root, g := range groups {
		owner, err := store.RootOwner(ctx, string(root))
		if err != nil {
			return err
		}
		g.Root = string(root)
		g.Owner = owner
		owners = append(owners, owner)
		p.Roots = append(p.Roots, *g)
	}
	release, admission, err := admitBatchCommitOwners(store, ctx, id, t, owners)
	if release != nil {
		defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	p.finalizationAdmission = admission
	return store.Project(ctx, p)
}

// batchCollectInserted records every node ID of one inserted subtree,
// keyed by the engine-derived ordinal paths (root, root.N, ...).
func batchCollectInserted(n *BatchReservationNode, id string, into map[jobbatch.ID]bool) {
	into[jobbatch.ID(id)] = true
	for i := range n.Children {
		batchCollectInserted(&n.Children[i], fmt.Sprintf("%s.%d", id, i+1), into)
	}
}

func batchCommitCheckpointEvent(t jobbatch.Transition) string {
	if t.Final {
		return "final"
	}
	return string(t.Event.Type)
}

func batchCommitFlag(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// batchNodeUpdate maps a NodeView onto ProjectBatchNodeParams. Envelope,
// reference and child-session fields stay NULL: the SQL COALESCEs preserve
// facts written outside the projection — the durable child-link bind
// (BatchBindChildSession) and response envelopes.
func batchNodeUpdate(v jobbatch.NodeView) (db.ProjectBatchNodeParams, error) {
	token, err := batchCommitUint64(v.Token)
	if err != nil {
		return db.ProjectBatchNodeParams{}, err
	}
	p := db.ProjectBatchNodeParams{
		NodeID:         string(v.ID),
		State:          string(v.State),
		Cause:          string(v.Cause),
		Token:          token,
		Attempts:       int64(v.Attempts),
		ControlState:   batchCommitControlState(v.Control),
		ControlError:   v.ControlError,
		OutcomeState:   string(v.Outcome.State),
		OutcomeCode:    v.Outcome.Code,
		OutcomeSummary: v.Outcome.Summary,
	}
	if v.ControlPending {
		p.ControlPending = 1
	}
	return p, nil
}

// batchCommitNotice maps one selected Notice. The ordinal is the Notice's
// position in Transition.Notices, never a structural node ordinal.
// Host-closing notices never wake; every other selected notice is pending
// delivery with wake=1 (exactly the Runner's selection, never reconstructed).
func batchCommitNotice(n jobbatch.Notice, ordinal int, closing bool) (db.SelectBatchNodeNoticeParams, error) {
	p := db.SelectBatchNodeNoticeParams{
		NodeID:            string(n.Node),
		NoticeKind:        sql.NullString{String: string(n.Kind), Valid: true},
		NoticeState:       sql.NullString{String: string(n.State), Valid: true},
		NoticeCause:       sql.NullString{String: string(n.Cause), Valid: true},
		NoticeOrdinal:     sql.NullInt64{Int64: int64(ordinal), Valid: true},
		NoticePending:     int64(n.Counts.Pending),
		NoticeActive:      int64(n.Counts.Active),
		NoticeCompleted:   int64(n.Counts.Completed),
		NoticeFailed:      int64(n.Counts.Failed),
		NoticeCancelled:   int64(n.Counts.Cancelled),
		NoticeTimedOut:    int64(n.Counts.TimedOut),
		NoticeInterrupted: int64(n.Counts.Interrupted),
		NoticeSkipped:     int64(n.Counts.Skipped),
		Wake:              1,
	}
	if closing {
		p.Wake = 0
	}
	return p, nil
}

// batchCommitEnvelope is the durable full response envelope of one Settled
// result. Summary and Details are opaque to the store and never logged.
type batchCommitEnvelope struct {
	State   string          `json:"state"`
	Code    string          `json:"code,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// RootOwner resolves the owning session of any projected root; cross-root
// transitions (admission, Release) project under the row's owner, never the
// hook identity's owner. It is a single read, not a lock across writer I/O.
func (s *BatchStore) RootOwner(ctx context.Context, root string) (string, error) {
	q, err := db.PdaBatchWriterQueries(ctx, s.writer)
	if err != nil {
		return "", err
	}
	row, err := q.GetBatchRootIdentity(ctx, root)
	if err != nil {
		return "", err
	}
	return row.OwnerSessionID, nil
}
