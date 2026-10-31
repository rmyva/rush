import { atom, computed } from "nanostores";
import type { Session, Message, ConfigPayload, MCPState, Todo, SkillInfo } from "./types";
import { isAsyncCompletionNotice, indexAsyncJobCompletions } from "./asyncJobCompletion";
import { buildMyPrompts, completionStatuses, mergeCompletionIndex } from "./completionIndex";
import { $batchWorkBySession } from "./batchWork";
import { drainSendableQueue } from "./queueOnIdle";

// ── Connection state ─────────────────────────────────────────────────────────
export const $connected = atom(false);
export const $authed = atom(false);

// True once the server confirmed our shutdown_server request (task #714).
// Terminal for the page: App renders a full-screen "server is shutting
// down" state and suppresses the reconnect banner — the server is
// intentionally gone, not transiently unreachable.
export const $serverShuttingDown = atom(false);

// ── Data ─────────────────────────────────────────────────────────────────────
export const $skills = atom<SkillInfo[]>([]);
export const $sessions = atom<Session[]>([]);
export const $activeSessionID = atom<string | null>(null);
export const $messages = atom<Message[]>([]);
// Completion indexes (R12-C): structured batch completions merged over the
// prose fallback — see completionIndex.ts for the merge rules and the
// structured-vs-prose behaviour boundary.
export const $asyncJobCompletions = computed(
  [$messages, $batchWorkBySession, $activeSessionID],
  (msgs, batchMap, sessionID) =>
    mergeCompletionIndex(
      // indexAsyncJobCompletions lives in asyncJobCompletion.ts; wired via
      // mergeCompletionIndex so the prose path stays read-only there.
      indexAsyncJobCompletions(msgs),
      batchMap.get(sessionID ?? "")?.lastGood?.completions ?? null,
    ),
);
export const $asyncJobStatuses = computed($asyncJobCompletions, completionStatuses);
export const $config = atom<ConfigPayload | null>(null);
// Set once per server start, and only when a newer release exists — the
// backend sends nothing otherwise, so a non-null value always means
// "there is an update". Replaces the notification path that was lost when
// the TUI was removed.
export const $updateAvailable = atom<{ current: string; latest: string } | null>(null);
export const $mcpState = atom<MCPState | null>(null);
export const $busySessions = atom<Set<string>>(new Set());
// Sessions where the user has queued a compact/summarise request.
export const $summarizeQueued = atom<Set<string>>(new Set());

// ── Sub-agent state ─────────────────────────────────────────────────────────
// Maps sub-agent session ID → parent session ID
export const $subAgentSessions = atom<Map<string, string>>(new Map());
// Maps sub-agent session ID → messages
export const $subAgentMessages = atom<Map<string, Message[]>>(new Map());

// Block breaks for zebra pattern: indices where a new visual block starts (5s gap)
export const $messageBlockBreaks = atom<Map<string, Set<number>>>(new Map());

// Monotonic counter incremented whenever the operator clicks "Collapse all
// spoilers" in the toolbar. Every collapsible component (ToolActivityGroup,
// ThinkingPart, SummaryMessage, BackgroundJobNotice) subscribes to it and
// closes its local open-state on each tick. Components use the value via
// a useEffect that skips the initial render (so just mounting doesn't
// collapse anything already open by default).
export const $collapseAllNonce = atom<number>(0);

export function collapseAllSpoilers() {
  $collapseAllNonce.set($collapseAllNonce.get() + 1);
}

// ── Actions ──────────────────────────────────────────────────────────────────
export function setSkills(skills: SkillInfo[]) {
  $skills.set(skills);
}

const LAST_SKILL_KEY = "rush_last_skill";
export const $lastUsedSkill = atom<string>(localStorage.getItem(LAST_SKILL_KEY) ?? "");
export function setLastUsedSkill(name: string) {
  $lastUsedSkill.set(name);
  localStorage.setItem(LAST_SKILL_KEY, name);
}

export function setSessions(sessions: Session[]) {
  $sessions.set(sessions);
}

export interface SessionsLiveDelta {
  upserts: Session[];
  deletedIDs: string[];
}

/** Applies a sessions_list snapshot, replaying any live session pushes that
 * raced the request (task #690). With an empty delta this is a plain
 * wholesale replace — the convergent behavior the 5s poll depends on. Returns
 * the reconciled list so routing decisions downstream use it, not the raw
 * snapshot. */
export function applySessionsSnapshot(sessions: Session[], live: SessionsLiveDelta): Session[] {
  let next = [...sessions];
  for (const id of live.deletedIDs) {
    next = next.filter((s) => s.ID !== id);
  }
  for (const s of live.upserts) {
    const idx = next.findIndex((x) => x.ID === s.ID);
    if (idx === -1) {
      next = [s, ...next];
    } else {
      next = [...next];
      next[idx] = s;
    }
  }
  $sessions.set(next);
  return next;
}

// ── Model History ────────────────────────────────────────────────────────────
// Recent models are stored on the backend and synced via WebSocket config.
// Local state is initialized empty and populated when config arrives.
//
// ModelRole covers all four session model slots (task #1061 generalized
// ModelSelector from smart/fast-only to all four; worker/reviewer already
// had session DB + set_session_models wire support from task #466/#1060).
export type ModelRole = "smart" | "fast" | "worker" | "reviewer";

export const $recentSmartModels = atom<string[]>([]);
export const $recentFastModels = atom<string[]>([]);
export const $recentWorkerModels = atom<string[]>([]);
export const $recentReviewerModels = atom<string[]>([]);

const RECENT_MODEL_STORES: Record<ModelRole, typeof $recentSmartModels> = {
  smart: $recentSmartModels,
  fast: $recentFastModels,
  worker: $recentWorkerModels,
  reviewer: $recentReviewerModels,
};

export function trackModelUsage(role: ModelRole, modelKey: string) {
  const store = RECENT_MODEL_STORES[role];

  const current = store.get();
  const next = [modelKey, ...current.filter((k) => k !== modelKey)].slice(0, 5);

  store.set(next);

  // Sync to backend - parse modelKey into provider and model
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("track_model_usage", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}

export function removeRecentModel(role: ModelRole, modelKey: string) {
  const store = RECENT_MODEL_STORES[role];

  const next = store.get().filter((k) => k !== modelKey);
  store.set(next);

  // Persist removal to server
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("remove_recent_model", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}

export function upsertSession(s: Session) {
  const list = $sessions.get();
  const idx = list.findIndex((x) => x.ID === s.ID);
  if (idx === -1) {
    $sessions.set([s, ...list]);
  } else {
    const next = [...list];
    next[idx] = s;
    $sessions.set(next);
  }
}

export function removeSession(id: string) {
  $sessions.set($sessions.get().filter((s) => s.ID !== id));
  // Drop the ws-layer request bookkeeping with the row (task #698): the
  // stale-reply floor and live-event epochs only matter while replies for
  // this session can still arrive and be applied.
  forgetSessionRequestState(id);
  // A session that no longer exists can never receive the
  // agent_busy=false that flushes its queue (task #726).
  removeSessionQueueAndBusy(id);
  // Sub-agent state, delete tombstones and block breaks are keyed
  // independently of $sessions — clear them with the row too, or they
  // grow once per session ever seen for the tab's lifetime (task #727).
  removeSubAgentState(id);
}

export function setActiveSession(id: string | null) {
  $activeSessionID.set(id);
  $messages.set([]);
  $agentError.set(null);
  if (id) {
    window.location.hash = `#/${id}`;
  } else {
    window.location.hash = "";
  }
}

export function setMessages(msgs: Message[]) {
  $messages.set(msgs);
}

export { mergePreserveContent } from "./mergeMessage";
import { mergePreserveContent } from "./mergeMessage";

export function upsertMessage(msg: Message) {
  const list = $messages.get();
  const idx = list.findIndex((m) => m.ID === msg.ID);
  if (idx === -1) {
    $messages.set([...list, msg]);
  } else {
    const prev = list[idx];
    const next = [...list];
    if (prev.Role === "assistant" && msg.Role === "assistant") {
      const merged = mergePreserveContent(prev, msg);
      if (merged !== msg) {
        console.warn("[upsertMessage] content regression blocked", {
          id: msg.ID,
        });
      }
      next[idx] = merged;
    } else {
      next[idx] = msg;
    }
    $messages.set(next);
  }
}

export function removeMessage(id: string) {
  $messages.set($messages.get().filter((m) => m.ID !== id));
}

export function registerSubAgentSession(subSessionID: string, parentSessionID: string) {
  const map = new Map($subAgentSessions.get());
  map.set(subSessionID, parentSessionID);
  $subAgentSessions.set(map);
}

export function isSubAgentSession(sessionID: string): boolean {
  return $subAgentSessions.get().has(sessionID);
}

export function getParentSessionID(subSessionID: string): string | undefined {
  return $subAgentSessions.get().get(subSessionID);
}

export function upsertSubAgentMessage(sessionID: string, msg: Message) {
  const map = new Map($subAgentMessages.get());
  const msgs = [...(map.get(sessionID) ?? [])];
  const idx = msgs.findIndex((m) => m.ID === msg.ID);
  if (idx === -1) {
    msgs.push(msg);
  } else {
    msgs[idx] = msg;
  }
  map.set(sessionID, msgs);
  $subAgentMessages.set(map);
}

export function setSubAgentMessages(sessionID: string, msgs: Message[]) {
  const map = new Map($subAgentMessages.get());
  map.set(sessionID, msgs);
  $subAgentMessages.set(map);
}

// removeSubAgentMessage drops one message from a sub-agent session's
// slice. The WS message_deleted handler routes compaction deletions here
// so a compacted sub-agent block stops rendering messages the backend
// already deleted (sub-agent slices are otherwise only replaced wholesale
// by a fresh messages_list reply).
export function removeSubAgentMessage(sessionID: string, messageID: string) {
  const map = new Map($subAgentMessages.get());
  const msgs = map.get(sessionID);
  if (!msgs) return;
  const next = msgs.filter((m) => m.ID !== messageID);
  if (next.length === msgs.length) return;
  map.set(sessionID, next);
  $subAgentMessages.set(map);
}

// ── messages_list snapshot application (task #689) ──────────────────────────
//
// A load_messages reply is a DB snapshot read at some instant between the
// request's send and its reply. Live pushes (message_created/_updated/
// _deleted) may have been applied locally inside that window. When they
// were (detected via ws.ts's per-session live-event epoch), replacing the
// list wholesale would erase them, so the snapshot is MERGED instead:
//
//   - messages deleted by a live push are tombstoned and filtered out of
//     every merge until an epoch-clean snapshot clears the tombstones;
//   - an ID present in both lists keeps the fresher row: assistant pairs
//     go through mergePreserveContent (streaming growth can only win),
//     other roles compare UpdatedAt;
//   - IDs only in the live list (created after the snapshot's read) are
//     appended after the snapshot rows — they are by definition newer.
//
// An epoch-clean reply (nothing raced it) still replaces wholesale: that
// path is what makes server-side deletes/compaction converge on the
// client.
//
// Task #731 augments the "epoch-clean" verdict itself with a per-session
// delete watermark (ws.ts's isSnapshotStaleForDeletes/bumpDeleteHighWaterMark),
// now backed by a delete-GENERATION counter (task #737, replacing the
// original rowid-based one — see internal/message/content.go's
// Message.DeleteGeneration doc comment for why): the epoch counter only
// proves "no live push landed since I sent this request", which is a
// statement about wall-clock event count, not about whether THIS reply's
// underlying DB read actually postdates a delete already applied. A
// worker-pool dispatch with no FIFO guarantee, reading off a separate
// read-only connection pool, can serve a reply whose read legitimately
// predates a delete even when the epoch counter says clean.
// applyMessagesSnapshot/applySubAgentMessagesSnapshot now take
// `liveRaced` as an OR of the epoch check and the watermark check — either
// one alone is enough to force the merge path instead of a wholesale
// replace that would silently clear tombstones and risk resurrecting the
// deleted row.

// Per-session IDs deleted by a live message_deleted push. Keyed by
// session so the main chat and each sub-agent transcript tombstone
// independently. Same lifetime class as ws.ts's latestLoadMessagesID:
// never eagerly cleared per event; bounded by sessions seen this tab.
const deletedMessageIDs = new Map<string, Set<string>>();

/** Records that messageID was deleted from sessionID by a live push, so
 * later merged snapshots that still contain the row (their DB read
 * predated the delete) keep it filtered out.
 *
 * generation is the delete watermark carried on the push's payload
 * (message_deleted's DeleteGeneration field — see
 * internal/message/content.go's Message.DeleteGeneration doc comment).
 * Forwarded to ws.ts's bumpDeleteHighWaterMark so a LATER snapshot reply
 * whose own watermark is provably older than this delete is recognized as
 * stale even when the epoch heuristic alone would call it clean (task
 * #731/#737). Undefined/0 (missing/pre-watermark server) is a harmless
 * no-op there — the tombstone itself still protects this messageID via the
 * existing epoch mechanism, unchanged. */
export function tombstoneMessage(sessionID: string, messageID: string, generation?: number) {
  const set = deletedMessageIDs.get(sessionID);
  if (set) set.add(messageID);
  else deletedMessageIDs.set(sessionID, new Set([messageID]));
  bumpDeleteHighWaterMark(sessionID, generation);
}

// mergeMessageLists combines a still-latest-but-stale snapshot with the
// live state that raced it. Pure: reads the tombstone set, writes
// nothing.
function mergeMessageLists(sessionID: string, existing: Message[], snapshot: Message[]): Message[] {
  const tomb = deletedMessageIDs.get(sessionID);
  const snap = tomb ? snapshot.filter((m) => !tomb.has(m.ID)) : snapshot;
  const existingByID = new Map(existing.map((m) => [m.ID, m]));
  const merged = snap.map((sm) => {
    const live = existingByID.get(sm.ID);
    if (!live) return sm;
    if (live.Role === "assistant" && sm.Role === "assistant") {
      return mergePreserveContent(live, sm);
    }
    // Non-assistant overlap: take the row with the newer UpdatedAt; a
    // tie keeps the live row (what the operator is looking at).
    return sm.UpdatedAt > live.UpdatedAt ? sm : live;
  });
  const snapIDs = new Set(snap.map((m) => m.ID));
  const liveTail = existing.filter((m) => !snapIDs.has(m.ID));
  return [...merged, ...liveTail];
}

/** Applies a messages_list snapshot to the main chat. `liveRaced` says a
 * live push was applied since the request was sent (ws.ts epoch check). */
export function applyMessagesSnapshot(sessionID: string, msgs: Message[], liveRaced: boolean) {
  if (!liveRaced) {
    setMessages(msgs);
    // A clean read postdates any recorded delete, so it cannot contain
    // tombstoned rows — dropping the set bounds memory.
    deletedMessageIDs.delete(sessionID);
    return;
  }
  $messages.set(mergeMessageLists(sessionID, $messages.get(), msgs));
}

/** Sub-agent transcript counterpart of applyMessagesSnapshot. */
export function applySubAgentMessagesSnapshot(sessionID: string, msgs: Message[], liveRaced: boolean) {
  if (!liveRaced) {
    setSubAgentMessages(sessionID, msgs);
    deletedMessageIDs.delete(sessionID);
    return;
  }
  const map = new Map($subAgentMessages.get());
  map.set(sessionID, mergeMessageLists(sessionID, map.get(sessionID) ?? [], msgs));
  $subAgentMessages.set(map);
}

const _msgPartTracker = new Map<string, { time: number; count: number }>();

export function trackMessageParts(msgID: string, parts: { type: string }[]) {
  const count = parts.length;
  const now = Date.now();
  const prev = _msgPartTracker.get(msgID);

  if (prev && count > prev.count && now - prev.time > 5000) {
    const newPart = parts[prev.count];
    if (newPart && newPart.type !== "tool_result" && newPart.type !== "finish") {
      const map = new Map($messageBlockBreaks.get());
      const set = new Set(map.get(msgID) ?? []);
      set.add(prev.count);
      map.set(msgID, set);
      $messageBlockBreaks.set(map);
    }
  }

  _msgPartTracker.set(msgID, { time: now, count });
}

export function setSessionBusy(sessionID: string, busy: boolean) {
  const s = new Set($busySessions.get());
  if (busy) s.add(sessionID);
  else s.delete(sessionID);
  $busySessions.set(s);
}

// Per-session model overrides: removed in favor of global selection
// Now using the session object from DB as source of truth.

export function getDefaultModelKey(role: ModelRole, config: ConfigPayload | null): string {
  const entry = config?.models?.[role];
  if (entry) return `${entry.Provider}:::${entry.Model}`;
  return "";
}

// roleFieldPrefix maps a ModelRole to its Session field prefix
// (Smart/Fast/Worker/Reviewer), so the generic per-role setters below can
// read/write `${prefix}ModelProvider` etc. without a role-keyed literal
// object at every call site.
function roleFieldPrefix(role: ModelRole): string {
  return `${role[0].toUpperCase()}${role.slice(1)}`;
}

import { ws, forgetSessionRequestState, bumpDeleteHighWaterMark } from "./ws";

// The outbound command wrappers moved to store_commands.ts when the
// 1000-line limit landed. Re-exported here so every existing
// `from "../store"` import keeps working: this split is about file length,
// not about the module's public surface. The cycle is safe --
// store_commands.ts touches these atoms only inside function bodies, never
// at module-initialization time.
export * from "./store_commands";

// The wire shape of an attachment. Defined here, not in store_commands.ts,
// because both halves speak it: the queue below stores it, the send wrappers
// transmit it.
export interface WireAttachment {
  fileName: string;
  mimeType: string;
  data: string; // base64
}
import { logClientEvent } from "./telemetry";

export function updateTodos(sessionID: string, todos: Todo[]) {
  // Optimistic local update
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = { ...next[idx], Todos: todos };
    $sessions.set(next);
  }
  logClientEvent("update_todos", { sessionID, count: todos.length, todos: todos.map((t) => ({ content: t.content, status: t.status })) });
  ws.send("update_todos", { sessionID, todos });
}

// setSessionModel sets exactly ONE role's session override, leaving the
// other three roles untouched — the set_session_models wire convention
// (task #461/#466): an omitted `<role>Model` key means "don't touch this
// slot", so the other three are simply never included in the payload.
// Generalizes the old two-arg (smart, fast) setSessionModels to all four
// roles (task #1061) now that ModelSelector renders worker/reviewer too.
export function setSessionModel(sessionID: string, role: ModelRole, key: string) {
  const idx = key.indexOf(":::");
  if (idx === -1) return;
  const provider = key.slice(0, idx);
  const model = key.slice(idx + 3);
  const prefix = roleFieldPrefix(role);

  // Optimistic local update so the UI reflects the change immediately.
  const sessions = $sessions.get();
  const sIdx = sessions.findIndex((s) => s.ID === sessionID);
  if (sIdx !== -1) {
    const next = [...sessions];
    next[sIdx] = { ...next[sIdx], [`${prefix}ModelProvider`]: provider, [`${prefix}ModelID`]: model };
    $sessions.set(next);
  }

  ws.send("set_session_models", { sessionID, [`${role}Model`]: { provider, model } });
}

// clearSessionModelSlot removes the session's explicit override for ONE
// slot, falling back to inheriting the folder/system default (task #467).
//
// This is NOT the same as calling setSessionModels with an empty key: an
// empty/null key there means "don't touch this slot" (parse() maps both to
// JS `null`, which is omitted from the WS payload and read as "leave alone"
// server-side — see internal/server/handlers.go's handleSetSessionModels).
// Clearing instead requires sending a NON-null object with an explicit empty
// provider/model, which internal/session's UpdateModels reads as "wipe this
// override" — distinct from a nil argument ("don't touch"). Only ONE of the
// two commands is sent (the untouched slot key is omitted entirely), so the
// other slot's override is left exactly as it was.
export function clearSessionModelSlot(sessionID: string, modelType: "smart" | "fast" | "worker" | "reviewer") {
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  const clearedFields: Record<string, string> = {
    smart: "SmartModel",
    fast: "FastModel",
    worker: "WorkerModel",
    reviewer: "ReviewerModel",
  };
  const prefix = clearedFields[modelType];
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = { ...next[idx], [`${prefix}Provider`]: "", [`${prefix}ID`]: "" };
    $sessions.set(next);
  }

  ws.send("set_session_models", {
    sessionID,
    [`${modelType}Model`]: { provider: "", model: "" },
  });
}

// setSessionRoleEffort sets ONE role's reasoning effort, re-sending that
// role's currently-stored provider/model alongside it (the backend backfills
// an omitted provider/model within the SAME slot, but the slot's own effort
// write needs its own provider/model present — see handleSetSessionModels's
// resolveEffortPair). Generalizes the old two-arg (smart, fast)
// setSessionReasoningEffort to all four roles (task #1061).
export function setSessionRoleEffort(sessionID: string, role: ModelRole, effort: string) {
  const prefix = roleFieldPrefix(role);
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  if (idx === -1) return;

  const next = [...sessions];
  next[idx] = { ...next[idx], [`${prefix}ModelReasoningEffort`]: effort };
  $sessions.set(next);

  const session = sessions[idx] as unknown as Record<string, string>;
  ws.send("set_session_models", {
    sessionID,
    [`${role}Model`]: {
      provider: session[`${prefix}ModelProvider`],
      model: session[`${prefix}ModelID`],
      reasoning_effort: effort || undefined,
    },
  });
}

// ── Theme ─────────────────────────────────────────────────────────────────────
// Theme is stored on the backend. localStorage is only used as a cache for
// instant page load (before WS connects) to avoid white flash.

const STORAGE_KEY_THEME = "rush_theme";

export function applyTheme(theme: string) {
  if (theme === "dark") {
    document.documentElement.classList.add("dark");
  } else {
    document.documentElement.classList.remove("dark");
  }
  // Cache to localStorage for instant load on next page refresh
  try { localStorage.setItem(STORAGE_KEY_THEME, theme); } catch {}
}

// Apply cached theme immediately on module load (before WS connects)
// This prevents white flash while waiting for backend connection.
;(function () {
  try {
    const cached = localStorage.getItem(STORAGE_KEY_THEME);
    if (cached) applyTheme(cached);
  } catch {}
})();

export function setTheme(theme: "light" | "dark") {
  applyTheme(theme);
  // Send to backend as source of truth
  ws.send("set_theme", { theme });
}

// ── Batch selection ───────────────────────────────────────────────────────────
export const $selectedMessageIDs = atom<Set<string>>(new Set());

export function toggleMessageSelection(id: string) {
  const s = new Set($selectedMessageIDs.get());
  if (s.has(id)) s.delete(id); else s.add(id);
  $selectedMessageIDs.set(s);
}

export function clearSelection() {
  $selectedMessageIDs.set(new Set());
}

// Select (only add, never remove) all IDs in the given array
export function selectMessageIDs(ids: string[]) {
  const s = new Set($selectedMessageIDs.get());
  for (const id of ids) s.add(id);
  $selectedMessageIDs.set(s);
}

// Last agent error to display in chat
export const $agentError = atom<string | null>(null);

// ── Message queue (client-side) ────────────────────────────────────────────────
export interface QueuedMessage {
  id: string;
  content: string;
  // Attachments captured from the composer when this message was queued;
  // they ride on the single flushed send when the turn ends.
  attachments?: WireAttachment[];
  // Explicit failure classification keeps unsafe-to-resend messages out of
  // automatic busy=false flushes. User actions can still take and retry them.
  failureState?: "rejected" | "ambiguous";
  error?: string;
}

export const $messageQueue = atom<Map<string, QueuedMessage[]>>(new Map());

let _queueIDCounter = 0;
function newQueueID() { return `q-${++_queueIDCounter}`; }

export function enqueueMessage(
  sessionID: string,
  content: string,
  attachments?: WireAttachment[]
) {
  const q = new Map($messageQueue.get());
  q.set(sessionID, [
    ...(q.get(sessionID) ?? []),
    { id: newQueueID(), content, attachments },
  ]);
  $messageQueue.set(q);
}

export interface FlushedQueue {
  content: string;
  attachments: WireAttachment[];
}

// Drains only messages that have not already failed a per-item send. Rejected
// and ambiguous items remain queued for an explicit user retry, avoiding both
// silent loss and accidental duplicates when an agent_busy=false event lands.
export function dequeueAllMessages(sessionID: string): FlushedQueue | undefined {
  const q = new Map($messageQueue.get());
  const msgs = q.get(sessionID) ?? [];
  if (!msgs.length) return undefined;
  const { sendable, retained } = drainSendableQueue(msgs);
  if (retained.length) q.set(sessionID, retained); else q.delete(sessionID);
  $messageQueue.set(q);
  if (!sendable.length) return undefined;
  return {
    content: sendable.map((m) => m.content).join("\n\n"),
    attachments: sendable.flatMap((m) => m.attachments ?? []),
  };
}

/** Drops the queued messages and busy flag for a session that no longer
 * exists. A removed session can never receive the agent_busy=false that
 * flushes its queue, so keeping either would strand the other (task #726).
 * Called from removeSession (live deletion) and the sessions_list handler
 * in useWS.ts (deletion that happened while this tab was offline). */
export function removeSessionQueueAndBusy(id: string) {
  const q = new Map($messageQueue.get());
  if (q.delete(id)) $messageQueue.set(q);
  if ($busySessions.get().has(id)) setSessionBusy(id, false);
}

/** Clears every piece of state that outlives a removed session's row
 * (task #727): the sub-agent registration and transcript keyed by the id
 * itself when it is a sub-agent session, plus the same for every
 * sub-agent session parented by it — the server cascades sub-session
 * deletion with the parent (internal/server/handlers_sessions.go
 * handleDeleteSession → Sessions.Delete), so a removed parent strands
 * its whole branch. Also drops the per-session delete tombstones and
 * whichever message-block breaks can still be attributed:
 * $messageBlockBreaks is keyed by MESSAGE id, so the only attribution
 * available is the message lists themselves — the active session's
 * $messages and each doomed sub-agent transcript. Called from
 * removeSession (live deletion) and the sessions_list handler in
 * useWS.ts (deletion that happened while this tab was offline). */
export function removeSubAgentState(id: string) {
  // Reaps one level of nesting: direct children of `id` only, not
  // grandchildren. Safe today because sub-agents cannot spawn sub-agents —
  // AgentTask's allowed toolset (resolveReadOnlyTools plus
  // workerToolNames, see internal/config/config.go and
  // internal/agent/coordinator_tools.go) never includes the `agent` tool.
  // If that ever changes, this needs to walk the whole subtree instead.
  const doomed = [id];
  for (const [sub, parent] of $subAgentSessions.get()) {
    if (parent === id) doomed.push(sub);
  }

  const msgIDs = new Set<string>();
  if ($activeSessionID.get() === id) {
    for (const m of $messages.get()) msgIDs.add(m.ID);
  }
  const subs = new Map($subAgentSessions.get());
  const subMsgs = new Map($subAgentMessages.get());
  for (const d of doomed) {
    subs.delete(d);
    for (const m of subMsgs.get(d) ?? []) msgIDs.add(m.ID);
    subMsgs.delete(d);
    deletedMessageIDs.delete(d);
  }
  if (subs.size !== $subAgentSessions.get().size) $subAgentSessions.set(subs);
  if (subMsgs.size !== $subAgentMessages.get().size) $subAgentMessages.set(subMsgs);
  if (msgIDs.size > 0) {
    const breaks = new Map($messageBlockBreaks.get());
    let dropped = false;
    for (const mid of msgIDs) dropped = breaks.delete(mid) || dropped;
    if (dropped) $messageBlockBreaks.set(breaks);
  }
}

export function removeQueuedMessage(sessionID: string, id: string) {
  const q = new Map($messageQueue.get());
  const msgs = (q.get(sessionID) ?? []).filter((m) => m.id !== id);
  if (!msgs.length) q.delete(sessionID); else q.set(sessionID, msgs);
  $messageQueue.set(q);
}

export function updateQueuedMessage(sessionID: string, id: string, content: string) {
  const q = new Map($messageQueue.get());
  // Editing clears the displayed error, but preserves failureState so the
  // message still requires an explicit send/retry action.
  const msgs = (q.get(sessionID) ?? []).map((m) => m.id === id ? { ...m, content, error: undefined } : m);
  q.set(sessionID, msgs);
  $messageQueue.set(q);
}

// ── Settings actions ───────────────────────────────────────────────────────────

export function addContextPath(path: string) {
  ws.send("add_context_path", { path });
}

export function removeContextPath(path: string) {
  ws.send("remove_context_path", { path });
}

export function addSkillsPath(path: string) {
  ws.send("add_skills_path", { path });
}

export function removeSkillsPath(path: string) {
  ws.send("remove_skills_path", { path });
}

// Scope is "global" or "local" (workspace); omitted/undefined defaults to
// "global" server-side, matching every scope-aware CLI command.
export type ConfigScope = "global" | "local";

export function addCustomProvider(payload: {
  id: string; name?: string; type: string; baseUrl: string; apiKey?: string;
  models?: { id: string; name: string; contextWindow?: number; costPer1mIn?: number; costPer1mOut?: number }[];
  peakHours?: { start: string; end: string; message?: string } | null;
  scope?: ConfigScope;
}, msgID?: string) {
  ws.send("add_custom_provider", payload, msgID);
}

export function removeCustomProvider(id: string, scope?: ConfigScope, msgID?: string) {
  ws.send("remove_custom_provider", { id, scope }, msgID);
}

export function updateCustomProvider(payload: {
  oldId: string; id: string; name?: string; type: string; baseUrl: string; apiKey?: string;
  models?: { id: string; name: string; contextWindow?: number; costPer1mIn?: number; costPer1mOut?: number }[];
  peakHours?: { start: string; end: string; message?: string } | null;
  scope?: ConfigScope;
}, msgID?: string) {
  ws.send("update_custom_provider", payload, msgID);
}

// setProviderPeakHours sets/clears ONLY the peak_hours field on ANY
// provider (built-in/catwalk-known like "anthropic"/"zai", or custom) — a
// targeted single-field write, unlike addCustomProvider/updateCustomProvider
// which replace every field and are only safe on a custom provider the
// client fully owns. This is what lets the UI manage peak hours for a
// built-in provider without knowing/round-tripping its type, base URL, API
// key, or model list.
export function setProviderPeakHours(payload: {
  id: string;
  peakHours: { start: string; end: string; message?: string } | null;
  scope?: ConfigScope;
}, msgID?: string) {
  ws.send("set_provider_peak_hours", payload, msgID);
}

// ── My prompts history ─────────────────────────────────────────────────────
//
// Derived from $messages (current active session), feeding shell-style
// recall (ArrowUp/Down) and the history dropdown. Eligibility is decided by
// isComposerTypedMessage / buildMyPrompts in completionIndex.ts (moved there
// with the rest of the completion code — it is the prose notice predicate's
// consumer); only the store wiring remains here.
export type { MyPromptItem } from "./completionIndex";
export const $myPrompts = computed($messages, (msgs) => buildMyPrompts(msgs, isAsyncCompletionNotice));

// jumpToMessage — scrolls the transcript to the given message ID and flashes
// it briefly. The Message component renders id={`msg-${ID}`} on its row and
// the .msg-flash CSS class drives a one-second highlight.
export function jumpToMessage(id: string) {
  const el = document.getElementById(`msg-${id}`);
  if (!el) return;
  el.scrollIntoView({ behavior: "smooth", block: "center" });
  el.classList.remove("msg-flash"); // restart animation if already there
  // Reflow so the class re-add re-triggers the keyframes.
  void (el as HTMLElement).offsetWidth;
  el.classList.add("msg-flash");
  window.setTimeout(() => el.classList.remove("msg-flash"), 1100);
}
