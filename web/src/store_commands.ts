import { ws, wsRequest, WSRequestError } from "./ws";
import { $messages, $summarizeQueued, $activeSessionID, $config, $sessions, $messageQueue, dequeueAllMessages, enqueueMessage, type WireAttachment, type QueuedMessage } from "./store";
import { sendQueuedItem, flushQueueOnIdle } from "./queueOnIdle";
import { logClientEvent } from "./telemetry";

// Outbound commands: thin wrappers over the WebSocket protocol. Each one is
// fire-and-forget -- the server answers with a broadcast that the store's own
// reducers apply, so none of these mutate state directly. Split out of
// store.ts when the 1000-line file limit landed.

// setKeepAliveEnabled toggles the WebAudio keep-alive preference. The
// backend persists it to the global rush.json under
// options.keep_alive_enabled and broadcasts a fresh `config` event;
// useWS.ts reacts to that broadcast and starts/stops the local audio.
export function setKeepAliveEnabled(enabled: boolean) {
  ws.send("set_keep_alive", { enabled });
}

export function setProviderKey(providerID: string, apiKey: string) {
  ws.send("set_provider_key", { providerID, apiKey });
}

export function removeProviderKey(providerID: string) {
  ws.send("remove_provider_key", { providerID });
}

export function deleteMessage(messageID: string) {
  ws.send("delete_message", { messageID });
}

export function deleteMessages(messageIDs: string[]) {
  ws.send("delete_messages", { messageIDs });
}

export function updateMessageContent(messageID: string, content: string) {
  ws.send("update_message_content", { messageID, content });
}

export function updateMessageThinking(messageID: string, thinking: string, partIndex?: number) {
  ws.send("update_message_thinking", { messageID, thinking, partIndex });
}

export function summarizeSession(sessionID: string) {
  ws.send("summarize_session", { sessionID });
}

export function cancelQueuedSummarize(sessionID: string) {
  ws.send("cancel_queued_summarize", { sessionID });
}

export function setSummarizeQueued(sessionID: string, queued: boolean) {
  const s = new Set($summarizeQueued.get());
  if (queued) s.add(sessionID);
  else s.delete(sessionID);
  $summarizeQueued.set(s);
}

export function deleteMessagePart(messageID: string, partIndex: number) {
  ws.send("delete_message_part", { messageID, partIndex });
}

export function updateMessagePart(messageID: string, partIndex: number, content: string) {
  ws.send("update_message_part", { messageID, partIndex, content });
}

export function togglePinMessage(messageID: string, pinned: boolean) {
  ws.send("toggle_pin_message", { messageID, pinned });
}

export function rerunFromMessage(messageID: string) {
  const sessionID = $activeSessionID.get();
  if (!sessionID) return;
  logClientEvent("rerun_message", { sessionID, messageID });
  ws.send("rerun_message", { messageID });
}

// collectTurnContent gathers the agent's response to one user prompt:
// thinking + text from every assistant message that follows the given user
// message, stopping at the next user message (or end of conversation). Tool
// calls and tool results are excluded — the operator wants the agent's prose,
// not its action log.
//
// Accepts EITHER a user message ID (canonical) OR any assistant/tool message
// ID belonging to that turn — the function walks backwards to find the turn's
// user message either way. This lets a "Copy all" button live on the agent's
// final message just as well as on the user's prompt.
//
// Returns "" if the user message has no agent response yet.
export function collectTurnContent(anyMessageID: string): string {
  const msgs = $messages.get();
  let startIdx = msgs.findIndex((m) => m.ID === anyMessageID);
  if (startIdx === -1) return "";
  // If we were handed a non-user message, walk back to the turn's user message.
  if (msgs[startIdx].Role !== "user") {
    let walk = startIdx;
    while (walk > 0 && msgs[walk].Role !== "user") walk--;
    if (msgs[walk].Role !== "user") return "";
    startIdx = walk;
  }

  const chunks: string[] = [];
  for (let i = startIdx + 1; i < msgs.length; i++) {
    const m = msgs[i];
    if (m.Hidden) continue;
    if (m.Role === "user") break; // next user turn — stop
    if (m.Role !== "assistant") continue; // skip tool-role messages
    for (const p of m.Parts) {
      if (p.type === "thinking") {
        const t = (p as { type: "thinking"; Thinking: string }).Thinking;
        if (t && t.trim()) chunks.push(`<thinking>\n${t}\n</thinking>`);
      } else if (p.type === "text") {
        const t = (p as { type: "text"; Text: string }).Text;
        if (t && t.trim()) chunks.push(t);
      }
    }
  }
  return chunks.join("\n\n");
}

// Wire shape of one file attachment in a send_message / inject_message
// frame. Mirrors the backend's SendMessagePayload.Attachments entry.
export function sendWithFastModel(
  sessionID: string,
  content: string,
  attachments?: WireAttachment[]
) {
  const config = $config.get();
  const sess = $sessions.get().find((s) => s.ID === sessionID);
  let fastModel: { provider: string; model: string } | undefined;
  if (sess && sess.FastModelID) {
    fastModel = { provider: sess.FastModelProvider, model: sess.FastModelID };
  } else if (config?.models?.fast) {
    fastModel = { provider: config.models.fast.Provider, model: config.models.fast.Model };
  }
  const payload: Record<string, unknown> = { sessionID, content };
  if (fastModel) {
    payload.smartModel = fastModel;
  }
  if (attachments && attachments.length > 0) {
    payload.attachments = attachments;
  }
  ws.send("send_message", payload);
}

export function flushQueuedMessagesOnIdle(sessionID: string, send: (payload: Record<string, unknown>) => boolean): void {
  flushQueueOnIdle(sessionID, {
    dequeue: (id) => dequeueAllMessages(id),
    send,
    restore: (id, content, attachments) => enqueueMessage(id, content, attachments as WireAttachment[]),
  });
}

// ── Queue flush used by agent_busy=false (task #1057) ───────────────────────
// for the turn to end. Both share one atomicity contract: the message is
// removed from $messageQueue SYNCHRONOUSLY, before the wsRequest round-trip
// even starts. Store updates and WS event handling both run on the main
// thread, so by the time any later event (in particular the agent_busy=false
// handler in useWS.ts, which drains the whole queue via dequeueAllMessages)
// gets to run, the item is already gone -- it can never be flushed a second
// time, no matter how the send and a busy-flip interleave. A failed request
// restores the item to its original index via restoreQueuedMessage below.
// Restored failed or ambiguous items must not be automatically retried.

/** Removes one message from a session's queue and returns it together with
 * its original index, or undefined if it's no longer there (already sent/
 * removed by something else). */
function takeQueuedMessage(sessionID: string, id: string): { item: QueuedMessage; index: number } | undefined {
  const q = new Map($messageQueue.get());
  const msgs = q.get(sessionID) ?? [];
  const index = msgs.findIndex((m) => m.id === id);
  if (index === -1) return undefined;
  const next = msgs.filter((m) => m.id !== id);
  if (next.length) q.set(sessionID, next); else q.delete(sessionID);
  $messageQueue.set(q);
  return { item: msgs[index], index };
}

/** Restores a message taken by takeQueuedMessage to its original position
 * (clamped to the current length, in case the queue changed size while the
 * send was in flight) after a failed send, attaching the error so the
 * QueuedMessageItem UI can show it. */
function restoreQueuedMessage(sessionID: string, item: QueuedMessage, index: number, error: string, state: "rejected" | "ambiguous") {
  const q = new Map($messageQueue.get());
  const msgs = [...(q.get(sessionID) ?? [])];
  msgs.splice(Math.min(index, msgs.length), 0, { ...item, error, failureState: state });
  q.set(sessionID, msgs);
  $messageQueue.set(q);
}

function failureState(err: unknown): "rejected" | "ambiguous" {
  return err instanceof WSRequestError && err.ambiguous ? "ambiguous" : "rejected";
}

function queuedMessagePayload(sessionID: string, item: QueuedMessage): Record<string, unknown> {
  const payload: Record<string, unknown> = { sessionID, content: item.content };
  if (item.attachments && item.attachments.length > 0) {
    payload.attachments = item.attachments;
  }
  return payload;
}

// interrupt_and_send bounds its own server-side work at 30s
// (handleInterruptAndSend's context.WithTimeout, cancelling a turn that may
// be stuck inside a tool call) -- this must sit ABOVE that bound, or a
// merely-slow-but-successful interrupt times out client-side while the
// server still delivers it, and the item gets restored with an error for a
// message that already went out (task #1057 review).
const INTERRUPT_AND_SEND_TIMEOUT_MS = 40_000;
// inject_message has no server-side bound (a DB write + in-memory mailbox
// enqueue, no provider call) but does save attachments to disk first --
// more headroom than the 10s default for a large attachment on a slow disk.
const INJECT_MESSAGE_TIMEOUT_MS = 15_000;

/** Turns a wsRequest rejection into a short, UI-ready message. A DEFINITE
 * failure (server explicitly rejected the request, or the frame never left
 * the browser) keeps its own text: the message is known not to have gone
 * out. An AMBIGUOUS one (timeout, or a disconnect after the frame was
 * written) means the server may have already acted on it, so resending
 * could duplicate it -- the message says so instead of "failed". */
function describeSendFailure(err: unknown): string {
  if (err instanceof WSRequestError && err.ambiguous) {
    return "May already be sent — check chat before resending.";
  }
  return err instanceof Error ? err.message : String(err);
}

/** Sends one queued message right now via inject_message: it merges into
 * the next step of the CURRENT turn without interrupting it. See the
 * atomicity contract above. */
export async function sendQueuedMessageNow(sessionID: string, id: string): Promise<void> {
  return sendQueuedItem(sessionID, id, {
    take: takeQueuedMessage,
    request: (targetSessionID, item) => wsRequest("inject_message", queuedMessagePayload(targetSessionID, item), { timeoutMs: INJECT_MESSAGE_TIMEOUT_MS }),
    restore: (targetSessionID, item, index, failure: { message: string; state: ReturnType<typeof failureState> }) => restoreQueuedMessage(targetSessionID, item, index, failure.message, failure.state),
    describeFailure: (err) => ({ message: describeSendFailure(err), state: failureState(err) }),
  });
}

/** Sends one queued message via interrupt_and_send: cancels the running
 * turn and immediately starts a new one with this message. Same atomicity
 * contract as sendQueuedMessageNow. */
export async function interruptAndSendQueuedMessage(sessionID: string, id: string): Promise<void> {
  const taken = takeQueuedMessage(sessionID, id);
  if (!taken) return;
  try {
    await wsRequest("interrupt_and_send", queuedMessagePayload(sessionID, taken.item), { timeoutMs: INTERRUPT_AND_SEND_TIMEOUT_MS });
  } catch (err) {
    restoreQueuedMessage(sessionID, taken.item, taken.index, describeSendFailure(err), failureState(err));
  }
}

/** Sends one queued message via the normal send_message path -- for a
 * message sitting in the queue while the session is IDLE (task #1057):
 * inject/interrupt only make sense against a running turn. Fire-and-forget,
 * exactly like the composer's own Send button when idle (ChatInput.tsx):
 * send_message runs the whole turn server-side and replies only on
 * failure (a bare error, no request/response pairing on success), so
 * wsRequest would time out on every ordinary long-running success. The only
 * failure observable here is the frame never reaching the socket at all
 * (offline outbox full); that's what gets restored. Never auto-resent from
 * here -- a persistent failure would loop. */
export function sendQueuedMessageDirect(sessionID: string, id: string): void {
  const taken = takeQueuedMessage(sessionID, id);
  if (!taken) return;
  if (!ws.sendQueued("send_message", queuedMessagePayload(sessionID, taken.item))) {
    restoreQueuedMessage(sessionID, taken.item, taken.index, "Not connected — message was not sent.", "rejected");
  }
}
