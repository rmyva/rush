package server

// Message CRUD handlers: loading a session's messages and deleting,
// editing, or pinning individual messages and message parts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
)

// updateMessageAndVerify wraps a.Messages.Update for the four message-part
// editing handlers below (update content, update thinking, delete part,
// update part).
//
// Mid-stream edits are refused outright (see below), because they cannot be
// made durable from this layer: the agent turn that owns a mid-stream
// message keeps its own in-memory currentAssistant and, independently of
// anything this handler does, will write that in-memory copy back to the
// same row -- either from the next auto-checkpoint tick (conditional,
// fenced on checkpoint_generation) or, unconditionally and unfenced, from
// the terminal write at the end of the turn (internal/agent/agent_turn.go
// OnStepFinish -> a.messages.Update with a non-Partial Finish, which takes
// Update's "terminal update always wins" branch -- see
// internal/message/message.go's Update). Neither of those writers has any
// way to know an edit landed in between, so an edit that landed while the
// message was still mid-stream WOULD be overwritten by stale in-memory
// content, deterministically, at the very latest by that terminal write --
// and a same-instant client-side read-back could not detect it, because it
// runs before the overwrite happens.
//
// So instead of trying to make a mid-stream edit durable (that needs a real
// edit protocol reconciled with the agent's in-memory state -- out of reach
// from the WS handler layer, which has no visibility into currentAssistant),
// this function refuses the edit outright while the message is still
// mid-stream. It re-reads the row (never trusts the caller's possibly-stale
// in-memory `m`) and rejects with a conflict unless the freshly-read row is
// already terminally finished (!IsFinished() == still owned by the turn).
// This makes the guarantee the caller gets honest: "ok" is only ever
// returned for a row nothing else is going to touch again.
//
// task #569 / release-blocker F1 established that reporting "ok" over an
// edit that silently vanished is the actual bug; task #590 established that
// a same-instant read-back proves the write landed but not that it stays
// landed. Refusing mid-stream edits closes both: there is no window left in
// which the handler can reply "ok" and have the row change out from under
// the operator afterward.
func updateMessageAndVerify(ctx context.Context, a *appPkg.App, c *Client, msgID string, m message.Message) bool {
	// Re-read the row rather than trusting m's Finish part: m was loaded by
	// the caller (Get) before it mutated Parts for the edit, so it already
	// reflects the row's state at that moment, but re-reading here keeps this
	// check independent of caller discipline and closes the same race the
	// mid-stream gate below exists to close -- a concurrent terminal finish
	// landing between the caller's Get and this call.
	current, err := a.Messages.Get(ctx, m.ID)
	if err != nil {
		c.reply(msgID, EventError, nil, err.Error())
		return false
	}
	// The caller's edit was based on an unfinished assistant message, but a
	// terminal finish arrived before this fresh read. Reject rather than
	// applying the stale edit: its Parts may have been changed (including
	// removal of the Finish part), and it could overwrite fresh terminal text
	// or other parts as well as the Finish.
	if m.Role == message.Assistant && !m.IsFinished() && current.IsFinished() {
		c.reply(msgID, EventError, nil, "message changed while it was being edited; please reload and try again")
		return false
	}
	// Only assistant messages ever carry a Finish part -- user/system/tool
	// rows are written once and never streamed, so IsFinished() on them is
	// always false and must NOT be read as "still owned by a turn". Gate the
	// mid-stream check on Role == Assistant, or every user-message edit
	// would be refused too.
	if current.Role == message.Assistant && !current.IsFinished() {
		// The message is still owned by an in-flight agent turn (no terminal
		// Finish yet, or only a Partial one from the checkpoint ticker). Any
		// write here would be overwritten by that turn's next checkpoint or,
		// unconditionally, its terminal write -- see the doc comment above.
		// Refuse now rather than report an "ok" that will not stick.
		c.reply(msgID, EventError, nil, "message is still streaming and cannot be edited yet; please wait for it to finish")
		return false
	}
	if err := a.Messages.Update(ctx, m); err != nil {
		c.reply(msgID, EventError, nil, err.Error())
		return false
	}
	// current was already terminal, so Update took the unconditional
	// "terminal update always wins" branch (message.Service.Update) -- no
	// re-read needed to confirm the write landed.
	return true
}

func handleLoadMessages(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p LoadMessagesPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	// ListWithWatermark over List: the reply must carry the session's
	// delete-generation watermark (task #737: an in-memory counter bumped
	// once on every delete for the session, read BEFORE the List query --
	// see that method's doc comment) so the client can tell a snapshot
	// whose read is PROVABLY older than a delete it already applied from
	// one that merely looks that way because of a reordered push (see
	// message.Message.DeleteGeneration's doc comment for the full
	// mechanism this closes). The wire field is still named "Watermark"
	// for back-compat with cached frontends that only understand the
	// numeric-comparison contract, not the old rowid semantics.
	msgs, watermark, err := a.Messages.ListWithWatermark(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if msgs == nil {
		msgs = []message.Message{}
	}
	// Wrap with the source sessionID so the frontend can route even an
	// EMPTY list to the right store (sub-agent vs main session). Without
	// this, a lazy load_messages for a sub-agent session that's still empty
	// returns [], and the frontend can't tell whether to clear the active
	// chat or the sub-agent buffer — it would blindly clear the active.
	c.reply(msg.ID, EventMessagesList, map[string]any{
		"SessionID": p.SessionID,
		"Messages":  toMessagesWire(msgs),
		"Watermark": watermark,
	}, "")
}

// deleteMessageRescuingOrphan attempts to delete a message, with an orphan
// rescue branch for unfinished assistant messages. If Delete fails with
// ErrMessageStillStreaming, it checks whether the session is idle via
// AgentCoordinator.IsSessionBusy; if so, it calls ForceDelete to clean up
// the orphan. Returns nil on successful delete or rescue, or the original
// Delete error if the message could not be deleted (busy session, nil
// coordinator, or any other error).
//
// The orphan-rescue decision lives in the handler layer (not message.Service)
// because message.Service cannot depend on agent.Coordinator (dependency
// direction: agent → message). IsSessionBusy is the same discriminator
// handleRerunMessage uses for its orphan cleanup. Nil coordinator fails
// closed: "no coordinator to prove idle" must not weaken the streaming guard.
//
// Task #622 (F-1) parity, redesigned in #631: in-process idle alone does
// not prove orphanhood — a `rush run --session S` in a DIFFERENT process
// writing to the same row is invisible to IsSessionBusy. The rescue
// therefore also holds the kernel-attested SHARED lock probe on the
// session's OS lock file (holdExternalSilenceProof — including its
// fail-closed unresolvable-data-directory and probe-error cases, see that
// doc) across the ForceDelete below, returning the original Delete error
// instead of force-deleting a row an external owner may still terminate
// cleanly. The probe is released immediately after the single delete.
func deleteMessageRescuingOrphan(ctx context.Context, a *appPkg.App, id string) error {
	err := a.Messages.Delete(ctx, id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, message.ErrMessageStillStreaming) {
		return err
	}

	// Orphan rescue: the message is still streaming, but if the session is
	// idle, this is a true orphan from a crashed/killed turn that will never
	// receive a terminal Finish.
	if a.AgentCoordinator == nil {
		// Fail closed: no coordinator to prove the session is idle.
		return err
	}

	// Get the message to retrieve its SessionID for the idle check.
	m, getErr := a.Messages.Get(ctx, id)
	if getErr != nil {
		// Original Delete error is more relevant to the caller.
		return err
	}

	if a.AgentCoordinator.IsSessionBusy(m.SessionID) {
		// Session is still busy — not an orphan, fail with original error.
		return err
	}

	// Task #622 (F-1)/#631: the session is idle in THIS process, but an
	// exclusive OS lock holder — in any process, including one whose
	// on-disk bytes still look unowned — means a writer we cannot see. The
	// shared probe denies while any exclusive holder lives; fail closed
	// with the original Delete error rather than force-deleting the row.
	// Held across the ForceDelete below so no external writer can start
	// mid-delete.
	probe, refuse, why := holdExternalSilenceProof(a, m.SessionID)
	if refuse {
		slog.Warn("ws: delete_message: refusing orphan rescue: "+why,
			"id", id, "session_id", m.SessionID)
		return err
	}
	defer probe.Release() // nil-safe; released after the single delete below

	// Session is idle and the message is still streaming: this is an orphan.
	// Force-delete it to avoid corrupting the transcript with partial text.
	slog.Info("ws: delete_message: orphaned streaming message, force-deleting",
		"id", id, "session_id", m.SessionID)
	if forceErr := a.Messages.ForceDelete(ctx, id); forceErr != nil {
		slog.Warn("ws: delete_message: failed to force-delete orphaned streaming message",
			"id", id, "err", forceErr)
		// Return the original Delete error — the rescue failed.
		return err
	}
	// Rescue succeeded: message is gone, report no error.
	return nil
}

func handleDeleteMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	// Delete publishes DeletedEvent internally; events.go broadcasts EventMessageDeleted.
	if err := deleteMessageRescuingOrphan(ctx, a, p.MessageID); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleDeleteMessages(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagesPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	// task #595 (P1-1): this used to swallow every per-ID error into a slog
	// warning and unconditionally reply {"status":"ok"} regardless of how
	// many of the requested deletes actually happened -- including the
	// specific case this task closes, where one selected ID is a still-
	// streaming assistant message that message.Service.Delete now correctly
	// refuses (ErrMessageStillStreaming). The web client fires this request
	// without waiting for a response (see web/src/components/Chat.tsx's
	// requestDeleteSelected / store.ts's deleteMessages) and relies entirely
	// on the per-message DeletedEvent -> EventMessageDeleted broadcast to
	// update the UI, so a bare "ok" here does not by itself mislead the
	// operator about individual rows -- but replying "ok" when some deletes
	// failed is still a lie about the overall operation, and useWS.ts's
	// generic `ws.on("error", ...)` handler surfaces ANY EventError reply as
	// an 8s visible toast regardless of which request it answers, so
	// reporting failures this way reaches the operator through an existing,
	// already-wired path with no frontend change required.
	var failed []string
	for _, id := range p.MessageIDs {
		if err := deleteMessageRescuingOrphan(ctx, a, id); err != nil {
			slog.Warn("ws: failed to delete message", "id", id, "err", err)
			failed = append(failed, id)
		}
	}
	if len(failed) > 0 {
		c.reply(msg.ID, EventError, map[string]any{
			"status":  "partial",
			"failed":  failed,
			"deleted": len(p.MessageIDs) - len(failed),
			"total":   len(p.MessageIDs),
		}, fmt.Sprintf("failed to delete %d of %d selected messages", len(failed), len(p.MessageIDs)))
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessageContent(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessageContentPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Replace text parts with the new content, keep all other parts intact
	newParts := make([]message.ContentPart, 0, len(m.Parts))
	replaced := false
	for _, part := range m.Parts {
		if _, ok := part.(message.TextContent); ok && !replaced {
			newParts = append(newParts, message.TextContent{Text: p.Content})
			replaced = true
		} else if _, ok := part.(message.TextContent); ok {
			// skip additional text parts — merged into first
		} else {
			newParts = append(newParts, part)
		}
	}
	if !replaced {
		newParts = append([]message.ContentPart{message.TextContent{Text: p.Content}}, newParts...)
	}
	m.Parts = newParts
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func updatedReasoningContent(rc message.ReasoningContent, thinking string) message.ReasoningContent {
	return message.ReasoningContent{
		Thinking:         thinking,
		Signature:        rc.Signature,
		ThoughtSignature: rc.ThoughtSignature,
		ToolID:           rc.ToolID,
		ResponsesData:    rc.ResponsesData,
		StartedAt:        rc.StartedAt,
		FinishedAt:       rc.FinishedAt,
	}
}

func handleUpdateMessageThinking(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p struct {
		MessageID string `json:"messageID"`
		Thinking  string `json:"thinking"`
		PartIndex *int   `json:"partIndex,omitempty"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	found := false
	if p.PartIndex != nil {
		if *p.PartIndex < 0 || *p.PartIndex >= len(m.Parts) {
			c.reply(msg.ID, EventError, nil, "part index out of range")
			return
		}
		rc, ok := m.Parts[*p.PartIndex].(message.ReasoningContent)
		if !ok {
			c.reply(msg.ID, EventError, nil, "part type not editable")
			return
		}
		m.Parts[*p.PartIndex] = updatedReasoningContent(rc, p.Thinking)
		found = true
	} else {
		// Older clients omit PartIndex; preserve their historical behavior by
		// updating the first reasoning part.
		for i, part := range m.Parts {
			if rc, ok := part.(message.ReasoningContent); ok {
				m.Parts[i] = updatedReasoningContent(rc, p.Thinking)
				found = true
				break
			}
		}
	}
	if !found {
		c.reply(msg.ID, EventError, nil, "message has no thinking part")
		return
	}
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleDeleteMessagePart(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteMessagePartPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if p.PartIndex < 0 || p.PartIndex >= len(m.Parts) {
		c.reply(msg.ID, EventError, nil, "part index out of range")
		return
	}
	// Part-level delete is a user-facing affordance for text/thinking rows only.
	// Finish/tool_call/tool_result are structural (provider-history/turn-termination),
	// and a wrong index must not be able to remove them.
	switch m.Parts[p.PartIndex].(type) {
	case message.TextContent, message.ReasoningContent:
		// Allowed deletions.
	default:
		c.reply(msg.ID, EventError, nil, "part type not deletable")
		return
	}
	m.Parts = append(m.Parts[:p.PartIndex], m.Parts[p.PartIndex+1:]...)
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleUpdateMessagePart(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateMessagePartPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	m, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if p.PartIndex < 0 || p.PartIndex >= len(m.Parts) {
		c.reply(msg.ID, EventError, nil, "part index out of range")
		return
	}
	switch part := m.Parts[p.PartIndex].(type) {
	case message.TextContent:
		m.Parts[p.PartIndex] = message.TextContent{Text: p.Content}
	case message.ReasoningContent:
		m.Parts[p.PartIndex] = message.ReasoningContent{
			Thinking:         p.Content,
			Signature:        part.Signature,
			ThoughtSignature: part.ThoughtSignature,
			ToolID:           part.ToolID,
			ResponsesData:    part.ResponsesData,
			StartedAt:        part.StartedAt,
			FinishedAt:       part.FinishedAt,
		}
	case message.ToolCall:
		// Copy-and-override to preserve ProviderExecuted and other fields the web UI renders (Metadata → inline diff).
		updated := part
		updated.Input = p.Content
		m.Parts[p.PartIndex] = updated
	case message.ToolResult:
		// Copy-and-override to preserve Data/MIMEType/Metadata.
		updated := part
		updated.Content = p.Content
		m.Parts[p.PartIndex] = updated
	default:
		c.reply(msg.ID, EventError, nil, "part type not editable")
		return
	}
	if !updateMessageAndVerify(ctx, a, c, msg.ID, m) {
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleTogglePinMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p TogglePinMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MessageID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Messages.SetPinned(ctx, p.MessageID, p.Pinned); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
