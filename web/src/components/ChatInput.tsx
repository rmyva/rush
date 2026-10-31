import { useState, useRef, useCallback, useEffect, useMemo } from "react";
import { useStore } from "@nanostores/react";
import {
  $activeSessionID,
  $busySessions,
  $connected,
  $sessions,
  $skills,
  $lastUsedSkill,
  $myPrompts,
  $messageQueue,
  enqueueMessage,
  dequeueAllMessages,
  sendWithFastModel,
  setLastUsedSkill,
  jumpToMessage,
  type WireAttachment,
} from "../store";
import { ws, $wsOutboxCount } from "../ws";
import { handleSitterCommand } from "../sitter";
import { ListOrdered, Send, SendHorizonal, Paperclip, X, Zap, History, CornerLeftUp, PlusCircle, WifiOff } from "lucide-react";
import type { SkillInfo } from "../types";
import type { MyPromptItem } from "../store";
import { isAttachmentPayloadWithinLimit } from "../attachmentLimit";

interface PendingAttachment {
  fileName: string;
  mimeType: string;
  data: string; // base64
  size: number;
}

async function readFileAsBase64(file: File): Promise<PendingAttachment> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const arrayBuffer = reader.result as ArrayBuffer;
      const bytes = new Uint8Array(arrayBuffer);
      let binary = "";
      bytes.forEach((b) => (binary += String.fromCharCode(b)));
      resolve({
        fileName: file.name,
        mimeType: file.type || "application/octet-stream",
        data: btoa(binary),
        size: file.size,
      });
    };
    reader.onerror = reject;
    reader.readAsArrayBuffer(file);
  });
}

function formatBytes(n: number): string {
  if (n >= 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + " MB";
  if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
  return n + " B";
}

// Strip the local-only `size` field — the wire shape is fileName/mimeType/data.
function toWireAttachments(atts: PendingAttachment[]): WireAttachment[] {
  return atts.map((a) => ({
    fileName: a.fileName,
    mimeType: a.mimeType,
    data: a.data,
  }));
}

// Source badge colors
const SOURCE_COLORS: Record<string, string> = {
  claude: "bg-[#d97706]/15 text-[#d97706]",
  gemini: "bg-[#1a73e8]/15 text-[#1a73e8]",
  qwen: "bg-[#7c3aed]/15 text-[#7c3aed]",
  cursor: "bg-[#0ea5e9]/15 text-[#0ea5e9]",
  zed: "bg-[#059669]/15 text-[#059669]",
  windsurf: "bg-[#0891b2]/15 text-[#0891b2]",
  rush: "bg-accent/15 text-accent",
  local: "bg-surface text-text-muted",
};

function sourceBadgeClass(source?: string): string {
  return SOURCE_COLORS[source ?? ""] ?? SOURCE_COLORS.local;
}

interface SlashMenuProps {
  items: SkillInfo[];
  selectedIdx: number;
  onSelect: (skill: SkillInfo, sendNow: boolean) => void;
}

function SlashMenu({ items, selectedIdx, onSelect }: SlashMenuProps) {
  const selectedRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    selectedRef.current?.scrollIntoView({ block: "nearest" });
  }, [selectedIdx]);

  if (items.length === 0) return null;

  return (
    <div className="absolute bottom-full left-0 right-0 mb-2 bg-base-overlay border border-surface rounded-2xl shadow-2xl overflow-hidden z-50">
      <div className="max-h-72 overflow-y-auto">
        {items.map((skill, i) => (
          <button
            key={`${skill.source}-${skill.name}`}
            ref={i === selectedIdx ? selectedRef : null}
            onMouseDown={(e) => {
              e.preventDefault(); // keep focus on textarea
              onSelect(skill, false);
            }}
            className={`w-full text-left px-4 py-2.5 flex items-center gap-3 transition-colors border-b border-surface/50 last:border-0 ${
              i === selectedIdx
                ? "bg-accent/20 border-l-2 border-l-accent pl-[14px]"
                : "hover:bg-base-subtle border-l-2 border-l-transparent pl-[14px]"
            }`}
          >
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2">
                <span className={`font-semibold text-sm ${i === selectedIdx ? "text-accent" : "text-text"}`}>/{skill.name}</span>
                {skill.source && (
                  <span className={`text-[10px] px-1.5 py-0.5 rounded font-mono font-medium ${sourceBadgeClass(skill.source)}`}>
                    {skill.source}
                  </span>
                )}
              </div>
              <p className="text-text-muted text-xs truncate mt-0.5 leading-relaxed">
                {skill.description}
              </p>
            </div>
          </button>
        ))}
      </div>
      <div className="px-4 py-1.5 border-t border-surface/50 flex items-center gap-3 text-[11px] text-text-subtle">
        <span><kbd className="font-mono">↑↓</kbd> navigate</span>
        <span><kbd className="font-mono">Tab</kbd> fill</span>
        <span><kbd className="font-mono">Enter</kbd> send</span>
        <span><kbd className="font-mono">Esc</kbd> close</span>
      </div>
    </div>
  );
}

// ── History menu — recall + jump for the user's own prompts ────────────────
//
// Two-action row: clicking the body fills the textarea (recall, for editing
// or re-sending); clicking the arrow icon jumps the transcript to that
// message (preserves the current draft, just navigates). Up/Down to walk
// the list, Enter to recall the selected row, Esc to close.

interface HistoryMenuProps {
  items: MyPromptItem[];
  selectedIdx: number;
  onRecall: (item: MyPromptItem) => void;
  onJump: (item: MyPromptItem) => void;
}

function HistoryMenu({ items, selectedIdx, onRecall, onJump }: HistoryMenuProps) {
  const selectedRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    selectedRef.current?.scrollIntoView({ block: "nearest" });
  }, [selectedIdx]);

  if (items.length === 0) {
    return (
      <div className="absolute bottom-full left-0 right-0 mb-2 bg-base-overlay border border-surface rounded-2xl shadow-2xl overflow-hidden z-50">
        <div className="px-4 py-3 text-sm text-text-subtle italic">No prompts in this session yet.</div>
      </div>
    );
  }

  return (
    <div className="absolute bottom-full left-0 right-0 mb-2 bg-base-overlay border border-surface rounded-2xl shadow-2xl overflow-hidden z-50">
      <div className="max-h-72 overflow-y-auto">
        {items.map((item, i) => (
          <div
            key={item.id}
            ref={i === selectedIdx ? selectedRef : null}
            className={`group/row flex items-stretch border-b border-surface/50 last:border-0 transition-colors ${
              i === selectedIdx
                ? "bg-accent/20 border-l-2 border-l-accent"
                : "hover:bg-base-subtle border-l-2 border-l-transparent"
            }`}
          >
            <button
              onMouseDown={(e) => { e.preventDefault(); onRecall(item); }}
              className="flex-1 text-left px-4 py-2.5 min-w-0"
              title="Click to put this prompt back in the input"
            >
              <div className="text-text text-sm whitespace-pre-wrap line-clamp-3 break-words font-medium">
                {item.text}
              </div>
            </button>
            <button
              onMouseDown={(e) => { e.preventDefault(); onJump(item); }}
              title="Jump to this message in the transcript"
              className="flex items-center justify-center px-3 text-text-subtle hover:text-accent border-l border-surface/50 transition-colors"
            >
              <CornerLeftUp size={16} />
            </button>
          </div>
        ))}
      </div>
      <div className="px-4 py-1.5 border-t border-surface/50 flex items-center gap-3 text-[11px] text-text-subtle">
        <span><kbd className="font-mono">↑↓</kbd> walk</span>
        <span><kbd className="font-mono">Enter</kbd> recall</span>
        <span><kbd className="font-mono">Esc</kbd> close</span>
      </div>
    </div>
  );
}

export function ChatInput() {
  const [text, setText] = useState("");
  const [attachments, setAttachments] = useState<PendingAttachment[]>([]);
  const [attachmentError, setAttachmentError] = useState("");
  const activeSessionID = useStore($activeSessionID);
  const busySessions = useStore($busySessions);
  const connected = useStore($connected);
  const outboxCount = useStore($wsOutboxCount);
  const sessions = useStore($sessions);
  const activeSession = useMemo(
    () => sessions.find((s) => s.ID === activeSessionID) ?? null,
    [sessions, activeSessionID],
  );
  // Foreign-owned: another live rush process holds the session lock.
  // We can read the conversation but can't drive it — the input row and
  // all action buttons are replaced by a read-only banner.
  const foreignOwned = !!activeSession?.OwnedExternal;
  const ownerPID = activeSession?.OwnedByPID ?? 0;
  const skills = useStore($skills);
  const lastUsedSkill = useStore($lastUsedSkill);
  const myPrompts = useStore($myPrompts);
  const messageQueue = useStore($messageQueue);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // ── History recall state ──────────────────────────────────────────────────
  // histIdx:  -1 = live draft, 0..N-1 = a prompt from $myPrompts (newest=0).
  // stash:    text the user was typing when they started recalling, so
  //           returning to histIdx=-1 (or pressing Esc) restores it.
  // historyOpen: whether the dropdown is visible. Independent of histIdx:
  //           shell-style ArrowUp/Down can recall without opening the menu.
  const [histIdx, setHistIdx] = useState(-1);
  const [stash, setStash] = useState("");
  const [historyOpen, setHistoryOpen] = useState(false);

  // Reset recall when the session changes — prompts list is per-session.
  useEffect(() => {
    setHistIdx(-1);
    setStash("");
    setHistoryOpen(false);
  }, [activeSessionID]);

  // ── Slash menu state ──────────────────────────────────────────────────────
  const [slashOpen, setSlashOpen] = useState(false);
  const [slashIdx, setSlashIdx] = useState(0);

  // ── Offline awareness ──────────────────────────────────────────────────────
  // The composer only claims "offline" once a connection has been seen —
  // otherwise every fresh page load would flash the banner while the very
  // first socket is still connecting.
  const [everConnected, setEverConnected] = useState(false);
  useEffect(() => {
    if (connected) setEverConnected(true);
  }, [connected]);
  const offline = everConnected && !connected;

  // Text matches "/<word>" with no spaces — menu active
  const slashMatch = text.match(/^\/(\S*)$/);
  const slashQuery = slashMatch ? slashMatch[1] : null;

  const filteredSkills = useMemo(() => {
    if (slashQuery === null) return [];
    const q = slashQuery.toLowerCase();
    return skills.filter(
      (s) =>
        s.name.toLowerCase().includes(q) ||
        s.description.toLowerCase().includes(q)
    );
  }, [skills, slashQuery]);

  // Auto-open and pre-select
  useEffect(() => {
    if (slashQuery !== null && filteredSkills.length > 0) {
      setSlashOpen(true);
      // Pre-select last used if in results, otherwise best name match, otherwise first
      const lastIdx = filteredSkills.findIndex((s) => s.name === lastUsedSkill);
      if (lastIdx >= 0) {
        setSlashIdx(lastIdx);
      } else {
        // Exact prefix match ranks first
        const q = slashQuery.toLowerCase();
        const exactIdx = filteredSkills.findIndex((s) => s.name.toLowerCase().startsWith(q));
        setSlashIdx(exactIdx >= 0 ? exactIdx : 0);
      }
    } else {
      setSlashOpen(false);
    }
  }, [slashQuery, filteredSkills.length, lastUsedSkill]); // eslint-disable-line react-hooks/exhaustive-deps

  function handleSkillSelect(skill: SkillInfo, sendNow: boolean) {
    setLastUsedSkill(skill.name);
    setSlashOpen(false);
    if (sendNow) {
      if (!activeSessionID) return;
      const content = skill.instructions || `/${skill.name}`;
      if (!isAttachmentPayloadWithinLimit(content, attachments)) {
        setAttachmentError("Message exceeds the 19 MiB payload budget. Shorten the text or remove attachments before sending.");
        return;
      }
      setAttachmentError("");
      // A file picked before the skill was chosen rides on the same frame
      // (same wire shape as send(): the backend appends "[Attached file:
      // <path>]" to the prompt itself) — without this the badge would stay
      // visible while the file silently never goes out.
      const payload: Record<string, unknown> = { sessionID: activeSessionID, content };
      const wire = toWireAttachments(attachments);
      if (wire.length > 0) {
        payload.attachments = wire;
      }
      // Park in the offline outbox when the socket is down; only clear the
      // input once the frame is on the wire or queued for delivery.
      if (!ws.sendQueued("send_message", payload)) return;
      setText("");
      setAttachments([]);
      if (textareaRef.current) textareaRef.current.style.height = "auto";
    } else {
      // Tab: fill in the command name + space so user can keep typing
      setText(`/${skill.name} `);
      setTimeout(() => {
        if (textareaRef.current) {
          textareaRef.current.focus();
          const len = textareaRef.current.value.length;
          textareaRef.current.setSelectionRange(len, len);
        }
      }, 0);
    }
  }

  // ─────────────────────────────────────────────────────────────────────────

  useEffect(() => {
    if (activeSessionID && textareaRef.current) {
      textareaRef.current.focus();
    }
  }, [activeSessionID]);

  const agentBusy = activeSessionID ? busySessions.has(activeSessionID) : false;

  const send = useCallback(() => {
    const msg = text.trim();
    if (!activeSessionID) return;
    if (!isAttachmentPayloadWithinLimit(msg, attachments)) {
      setAttachmentError("Message exceeds the 19 MiB payload budget. Shorten the text or remove attachments before sending.");
      return;
    }
    setAttachmentError("");
    // Attachment-only sends are legal: the backend's send_message accepts
    // Attachments independent of Content (handlers_agent.go appends
    // "[Attached file: <path>]" to the prompt itself), so a pending
    // attachment with an empty draft still means "send".
    // Slash-command decision: command interception is deliberately
    // text-only — commands are parsed from the typed text, and an
    // attachment-only send has no text, hence no command to interpret.
    // It goes out as a raw frame with empty content.
    if (!msg && attachments.length === 0) return;
    // Intercept `/sitter`-family before any server roundtrip — it's a
    // pure-client toggle (start/stop the auto-resume loop). Other slash
    // commands flow through unchanged. The msg guard makes the
    // text-only-command rule above explicit.
    if (msg && handleSitterCommand(msg)) {
      setText("");
      setHistIdx(-1);
      setStash("");
      setHistoryOpen(false);
      if (textareaRef.current) textareaRef.current.style.height = "auto";
      return;
    }
    if (agentBusy) {
      const queuedMessages = messageQueue.get(activeSessionID) ?? [];
      const aggregateContent = [...queuedMessages.map((message) => message.content), msg].join("\n\n");
      const aggregateAttachments = [
        ...queuedMessages.flatMap((message) => message.attachments ?? []),
        ...toWireAttachments(attachments),
      ];
      if (!isAttachmentPayloadWithinLimit(aggregateContent, aggregateAttachments)) {
        setAttachmentError("Queued messages exceed the 19 MiB payload budget. Shorten the text or remove attachments before sending.");
        return;
      }
      // The attachment is captured by the queued message — it will go out
      // with the flushed send, so clearing the badge here is honest.
      enqueueMessage(activeSessionID, msg, toWireAttachments(attachments));
    } else {
      const payload: Record<string, unknown> = { sessionID: activeSessionID, content: msg };
      const wire = toWireAttachments(attachments);
      if (wire.length > 0) {
        payload.attachments = wire;
      }
      // When the socket is down the frame is parked in the offline outbox
      // and flushed on reconnect; if even that failed (outbox full) keep
      // the draft so the composer can't silently swallow the message.
      if (!ws.sendQueued("send_message", payload)) return;
    }
    setAttachments([]);
    setText("");
    setHistIdx(-1);
    setStash("");
    setHistoryOpen(false);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy, attachments, messageQueue]);

  const sendFast = useCallback(() => {
    const msg = text.trim();
    // Attachment-only fast-sends are legal for the same reason as send().
    if (!activeSessionID || agentBusy) return;
    if (!isAttachmentPayloadWithinLimit(msg, attachments)) {
      setAttachmentError("Message exceeds the 19 MiB payload budget. Shorten the text or remove attachments before sending.");
      return;
    }
    setAttachmentError("");
    if (!msg && attachments.length === 0) return;
    // sendWithFastModel fire-and-forgets its frame internally (store.ts), so
    // guard here: with the socket down, keep the draft rather than let it
    // vanish into a no-op send.
    if (!ws.isOpen()) return;
    sendWithFastModel(activeSessionID, msg, toWireAttachments(attachments));
    setText("");
    setAttachments([]);
    setHistIdx(-1);
    setStash("");
    setHistoryOpen(false);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, agentBusy, attachments]);

  // Interrupt the in-flight turn AND submit immediately. The server's
  // interrupt_and_send handler queues the new message and cancels the
  // running turn; agent.Run()'s cancel-handling branch drains the queue
  // and re-enters Run() with this message — so the partial assistant
  // output stays in history and the new instruction picks up right away.
  // We also fold anything sitting in the local front-end queue into the
  // submitted text (both text AND attachments), otherwise those would race
  // the cancel and arrive in a separate send_message after the interrupt-turn
  // finishes.
  const interrupt = useCallback(() => {
    if (!activeSessionID) return;
    const queuedMessages = messageQueue.get(activeSessionID) ?? [];
    const queued = queuedMessages.length > 0 ? {
      content: queuedMessages.map((message) => message.content).join("\n\n"),
      attachments: queuedMessages.flatMap((message) => message.attachments ?? []),
    } : undefined;
    const parts: string[] = [];
    const atts: WireAttachment[] = [];
    if (queued) {
      parts.push(queued.content);
      atts.push(...queued.attachments);
    }
    const msg = text.trim();
    if (msg) parts.push(msg);
    atts.push(...toWireAttachments(attachments));
    const content = parts.join("\n\n");
    if (!isAttachmentPayloadWithinLimit(content, atts)) {
      setAttachmentError("Message exceeds the 19 MiB payload budget. Shorten the text or remove attachments before sending.");
      return;
    }
    setAttachmentError("");
    // Attachment-only is valid (see send()) — the backend appends the saved
    // attachment paths to the prompt itself. Without this, relaxing canSend
    // would leave the Interrupt button enabled but dead.
    if (!content && atts.length === 0) return;
    const payload: Record<string, unknown> = { sessionID: activeSessionID, content };
    if (atts.length > 0) {
      payload.attachments = atts;
    }
    // Held in the offline outbox when the socket is down (the local queue
    // was already folded into this payload, so parking the whole frame is
    // the only way to not lose it) and delivered on reconnect.
    // Stale-action policy (task #726): parked as interrupt_and_send, NOT
    // rewritten to send_message. If the target turn already finished
    // server-side by delivery time, the server's idle path degrades the
    // interrupt into exactly the desired default — a fresh run of the
    // content (coordinator.InterruptAndSend → startDetachedRun durable
    // enqueue); if the turn is still running, cancel-and-replace happens
    // as the user asked. Both delivery worlds are safe, so the frame
    // keeps its original type.
    if (!ws.sendQueued("interrupt_and_send", payload)) return;
    if (queued) dequeueAllMessages(activeSessionID);
    setText("");
    setAttachments([]);
    setHistIdx(-1);
    setStash("");
    setHistoryOpen(false);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, attachments, messageQueue]);

  // Inject the new message into the running turn WITHOUT cancelling it.
  // Backend persists the user message to the DB immediately (UI updates via
  // the same pubsub path as a regular send_message) and schedules it to be
  // appended to prepared.Messages at the next PrepareStep — so the model
  // sees it on its next provider request inside the SAME Run(). Anything in
  // the front-end local queue (both text AND attachments) is folded in so
  // we don't race.
  const inject = useCallback(() => {
    if (!activeSessionID) return;
    const queuedMessages = messageQueue.get(activeSessionID) ?? [];
    const queued = queuedMessages.length > 0 ? {
      content: queuedMessages.map((message) => message.content).join("\n\n"),
      attachments: queuedMessages.flatMap((message) => message.attachments ?? []),
    } : undefined;
    const parts: string[] = [];
    const atts: WireAttachment[] = [];
    if (queued) {
      parts.push(queued.content);
      atts.push(...queued.attachments);
    }
    const msg = text.trim();
    if (msg) parts.push(msg);
    atts.push(...toWireAttachments(attachments));
    const content = parts.join("\n\n");
    if (!isAttachmentPayloadWithinLimit(content, atts)) {
      setAttachmentError("Message exceeds the 19 MiB payload budget. Shorten the text or remove attachments before sending.");
      return;
    }
    setAttachmentError("");
    // Attachment-only is valid (see send()) — same reasoning as interrupt.
    if (!content && atts.length === 0) return;
    const payload: Record<string, unknown> = { sessionID: activeSessionID, content };
    if (atts.length > 0) {
      payload.attachments = atts;
    }
    // Same offline-outbox treatment as interrupt: the dequeued local queue
    // lives in this frame now, so it must be parked or sent, never dropped.
    // Stale-action policy (task #726): "inject" means "merge into the
    // RUNNING turn", but while the socket is down we cannot know whether
    // that turn still exists when the frame is finally delivered. The
    // server's idle-inject semantics would only persist the message and
    // leave it unanswered until some later turn happens to run (see
    // sessionAgent.InjectMessage's injectIfBusy no-op path). So an inject
    // parked offline is re-shaped into a plain send_message, which is
    // correct in both worlds: if the turn ended, it starts a fresh turn
    // (immediate answer); if the turn is still running, the server queues
    // the call as the next turn's content (handleSendMessage on an owned
    // session). Nothing stays dormant. sendQueued keeps whatever type it's
    // given whether it sends immediately or parks (see ws.ts), so the type
    // must be chosen HERE based on the socket's current state — the normal
    // online case still needs the real inject_message type.
    const type = ws.isOpen() ? "inject_message" : "send_message";
    if (!ws.sendQueued(type, payload)) return;
    if (queued) dequeueAllMessages(activeSessionID);
    setText("");
    setAttachments([]);
    setHistIdx(-1);
    setStash("");
    setHistoryOpen(false);
    if (textareaRef.current) textareaRef.current.style.height = "auto";
  }, [text, activeSessionID, attachments, messageQueue]);

  // Apply the prompt at index `idx` to the textarea. idx=-1 restores the
  // user's draft from `stash`. Resizes the textarea and parks the caret
  // at the very end, matching what a user expects from "the previous prompt".
  const applyRecall = useCallback((idx: number) => {
    const next = idx === -1 ? stash : myPrompts[idx]?.text ?? "";
    setHistIdx(idx);
    setText(next);
    requestAnimationFrame(() => {
      const el = textareaRef.current;
      if (!el) return;
      el.style.height = "auto";
      el.style.height = Math.min(el.scrollHeight, 720) + "px";
      el.focus();
      const len = el.value.length;
      el.setSelectionRange(len, len);
    });
  }, [myPrompts, stash]);

  const recallPrev = useCallback(() => {
    if (myPrompts.length === 0) return;
    if (histIdx === -1) setStash(text);
    const next = Math.min(histIdx + 1, myPrompts.length - 1);
    if (next === histIdx) return;
    applyRecall(next);
  }, [applyRecall, histIdx, myPrompts.length, text]);

  const recallNext = useCallback(() => {
    if (histIdx === -1) return; // already at live draft
    applyRecall(histIdx - 1);
  }, [applyRecall, histIdx]);

  // Caret introspection — true if there is no newline before/after the caret,
  // i.e. visually on the first/last line of the textarea. This is what lets
  // ArrowUp/Down move the caret normally on multi-line inputs and ONLY
  // trigger recall at the line edges.
  const caretOnFirstLine = useCallback(() => {
    const el = textareaRef.current;
    if (!el) return true;
    return el.value.slice(0, el.selectionStart).indexOf("\n") === -1;
  }, []);

  const caretOnLastLine = useCallback(() => {
    const el = textareaRef.current;
    if (!el) return true;
    return el.value.slice(el.selectionEnd).indexOf("\n") === -1;
  }, []);

  const handleRecallFromMenu = useCallback((item: MyPromptItem) => {
    const idx = myPrompts.findIndex((p) => p.id === item.id);
    if (idx < 0) return;
    if (histIdx === -1) setStash(text);
    setHistoryOpen(false);
    applyRecall(idx);
  }, [applyRecall, histIdx, myPrompts, text]);

  const handleJumpFromMenu = useCallback((item: MyPromptItem) => {
    setHistoryOpen(false);
    jumpToMessage(item.id);
  }, []);

  const toggleHistoryMenu = useCallback(() => {
    setHistoryOpen((v) => !v);
  }, []);

  function onKey(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    // History menu walks the same Up/Down/Enter/Esc as slash menu but
    // takes priority when open; selection moves through myPrompts.
    if (historyOpen) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setHistIdx((i) => Math.max(i - 1, 0));
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setHistIdx((i) => Math.min(i + 1, Math.max(myPrompts.length - 1, 0)));
        return;
      }
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        const item = myPrompts[histIdx === -1 ? 0 : histIdx];
        if (item) handleRecallFromMenu(item);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setHistoryOpen(false);
        return;
      }
    }

    if (slashOpen && filteredSkills.length > 0) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setSlashIdx((i) => Math.min(i + 1, filteredSkills.length - 1));
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setSlashIdx((i) => Math.max(i - 1, 0));
        return;
      }
      if (e.key === "Tab") {
        e.preventDefault();
        handleSkillSelect(filteredSkills[slashIdx], false);
        return;
      }
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        handleSkillSelect(filteredSkills[slashIdx], true);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setSlashOpen(false);
        return;
      }
    }

    // Shell-style recall on bare ArrowUp/Down (no menu, no modifiers). Only
    // fires at the visual line edge so multi-line editing isn't disturbed.
    // Esc while recalling restores the user's original draft from the stash.
    if (e.key === "ArrowUp" && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
      if (caretOnFirstLine() && myPrompts.length > 0) {
        e.preventDefault();
        recallPrev();
        return;
      }
    }
    if (e.key === "ArrowDown" && !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey) {
      if (caretOnLastLine() && histIdx !== -1) {
        e.preventDefault();
        recallNext();
        return;
      }
    }
    if (e.key === "Escape" && histIdx !== -1) {
      e.preventDefault();
      applyRecall(-1);
      setStash("");
      return;
    }

    if (e.key === "Enter" && !e.shiftKey && !e.ctrlKey) {
      e.preventDefault();
      send();
    }
  }

  function onInput(e: React.ChangeEvent<HTMLTextAreaElement>) {
    setText(e.target.value);
    // The moment the user edits a recalled prompt, treat it as a fresh draft:
    // arrow-up should walk history again from "the current text" downwards.
    if (histIdx !== -1) {
      setHistIdx(-1);
      setStash("");
    }
    const el = e.target;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, 720) + "px";
  }

  async function appendAttachments(files: File[]) {
    if (files.length === 0) return;
    const sizes = files.map((file) => ({ size: file.size }));
    if (sizes.some((file) => !isAttachmentPayloadWithinLimit("", [file]))) {
      setAttachmentError("A file exceeds the 19 MiB payload limit. Choose a smaller file.");
      return;
    }
    setAttachmentError("");
    const reads = await Promise.all(files.map(readFileAsBase64));
    setAttachments((prev) => [...prev, ...reads]);
  }

  async function onFileChange(e: React.ChangeEvent<HTMLInputElement>) {
    const files = Array.from(e.target.files ?? []);
    await appendAttachments(files);
    if (fileInputRef.current) fileInputRef.current.value = "";
  }

  function removeAttachment(idx: number) {
    setAttachments((prev) => prev.filter((_, i) => i !== idx));
    setAttachmentError("");
  }

  function onDragOver(e: React.DragEvent) {
    e.preventDefault();
  }

  async function onDrop(e: React.DragEvent) {
    e.preventDefault();
    const files = Array.from(e.dataTransfer.files);
    if (files.length === 0) return;
    await appendAttachments(files);
  }

  async function onPaste(e: React.ClipboardEvent) {
    const items = Array.from(e.clipboardData.items);
    const pastedFiles: File[] = [];
    for (const item of items) {
      if (item.kind === "file") {
        const file = item.getAsFile();
        if (file) pastedFiles.push(file);
      }
    }
    if (pastedFiles.length === 0) return;
    e.preventDefault();
    await appendAttachments(pastedFiles);
  }

  // A send needs text OR a pending attachment — the backend accepts
  // attachment-only frames (content goes out empty).
  const canSend = (!!text.trim() || attachments.length > 0) && !!activeSessionID;
  const placeholder = activeSessionID
    ? agentBusy
      ? "Message… (will be queued)"
      : "Message… (/ for skills, Enter to send)"
    : "Select or create a session";

  if (foreignOwned) {
    return (
      <div className="px-5 pt-2 pb-4 bg-canvas shrink-0">
        <div
          data-test-id="chat-input-foreign-owned-banner"
          className="rounded-xl border border-yellow/40 bg-yellow/10 text-yellow px-4 py-3 text-sm flex items-center gap-2"
          title="Read-only follow mode. Another live rush process holds the session lock — this tab polls the database for updates instead of driving the agent."
        >
          <Zap size={14} className="shrink-0" />
          <span>
            Read-only follow mode — session is driven by another rush process
            {ownerPID > 0 ? ` (PID ${ownerPID})` : ""}.
          </span>
        </div>
      </div>
    );
  }

  return (
    <div className="px-5 pt-2 pb-4 bg-canvas shrink-0">
      {offline && (
        <div
          data-test-id="chat-input-offline-indicator"
          className="mb-2 rounded-xl border border-yellow/40 bg-yellow/10 text-yellow px-4 py-2.5 text-xs flex items-center gap-2"
          title="The WebSocket connection to the server is down. Messages you send from here are held locally and delivered automatically when the connection returns."
        >
          <WifiOff size={13} className="shrink-0" />
          <span>
            Offline — messages you send are held locally
            {outboxCount > 0 ? ` (${outboxCount} queued)` : ""} and go out when the connection returns.
          </span>
        </div>
      )}
      {attachmentError && (
        <div role="alert" className="mb-2 text-xs text-red-400">{attachmentError}</div>
      )}
      {/* Attachment badges */}
      {attachments.length > 0 && (
        <div className="flex flex-wrap gap-2 mb-2">
          {attachments.map((att, idx) => (
            <div
              key={idx}
              className="attachment-badge"
            >
              <Paperclip size={11} className="text-text-subtle shrink-0" />
              <span className="truncate max-w-[140px]">{att.fileName}</span>
              <span className="text-text-muted shrink-0">({formatBytes(att.size)})</span>
              <button
                onClick={() => removeAttachment(idx)}
                className="text-text-subtle hover:text-text transition-colors ml-0.5"
                title="Remove attachment"
              >
                <X size={11} />
              </button>
            </div>
          ))}
        </div>
      )}

      <div className="relative">
        {/* Slash command dropdown — opens above input */}
        {slashOpen && (
          <SlashMenu
            items={filteredSkills}
            selectedIdx={slashIdx}
            onSelect={handleSkillSelect}
          />
        )}

        {/* History dropdown — recall + jump for the user's own prompts. */}
        {historyOpen && !slashOpen && (
          <HistoryMenu
            items={myPrompts}
            selectedIdx={histIdx === -1 ? 0 : histIdx}
            onRecall={handleRecallFromMenu}
            onJump={handleJumpFromMenu}
          />
        )}

        <div
          className={`flex gap-4 items-end bg-base-overlay border rounded-2xl px-5 py-4 transition-all ${
            activeSessionID
              ? "border-surface focus-within:border-accent/50 focus-within:shadow-md focus-within:bg-canvas"
              : "border-surface opacity-60"
          }`}
          onDragOver={onDragOver}
          onDrop={onDrop}
        >
          <textarea
            ref={textareaRef}
            value={text}
            onChange={onInput}
            onKeyDown={onKey}
            onPaste={onPaste}
            placeholder={placeholder}
            disabled={!activeSessionID}
            autoFocus
            rows={1}
            data-test-id="chat-input-textarea"
            className="flex-1 bg-transparent border-none outline-none resize-none text-text leading-relaxed min-h-[28px] max-h-[960px] overflow-y-auto disabled:cursor-not-allowed placeholder:text-text-subtle font-medium" style={{ fontSize: "var(--chat-font-size)" }}
          />
          <div className="flex items-center gap-2 shrink-0">
            <input
              ref={fileInputRef}
              type="file"
              multiple
              className="hidden"
              onChange={onFileChange}
              aria-label="Attach files"
            />
            <button
              onClick={toggleHistoryMenu}
              disabled={!activeSessionID || myPrompts.length === 0}
              title="Your previous prompts in this session (↑ in input also works)"
              className={`btn-input-action ${historyOpen ? "text-accent bg-accent/10" : ""}`}
              data-test-id="chat-input-history-button"
            >
              <History size={18} />
            </button>
            {!agentBusy && (
              <button
                onClick={() => fileInputRef.current?.click()}
                disabled={!activeSessionID}
                title="Attach files"
                className="btn-input-action"
              >
                <Paperclip size={18} />
              </button>
            )}
            {!agentBusy && (
              <button
                onClick={sendFast}
                disabled={!canSend}
                title="Send with lightweight model"
                className="btn-input-action"
              >
                <SendHorizonal size={18} />
              </button>
            )}
            {agentBusy && (
              <button
                onClick={interrupt}
                disabled={!canSend}
                data-test-id="chat-input-interrupt-button"
                title="Cancel the current turn and submit this message immediately. The partial assistant reply stays in history."
                className="font-bold rounded-xl px-4 py-2.5 text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2 bg-yellow/15 border border-yellow/40 text-yellow hover:bg-yellow/25"
              >
                <Zap size={15} />
                Interrupt
              </button>
            )}
            {agentBusy && (
              <button
                onClick={inject}
                disabled={!canSend}
                data-test-id="chat-input-inject-button"
                title="Inject this message into the running turn without cancelling. The agent sees it in the next provider request inside the same Run()."
                className="font-bold rounded-xl px-4 py-2.5 text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2 bg-green/15 border border-green/40 text-green hover:bg-green/25"
              >
                <PlusCircle size={15} />
                Inject
              </button>
            )}
            <button
              onClick={send}
              disabled={!canSend}
              data-test-id="chat-input-send-button"
              className={`font-bold rounded-xl px-5 py-2.5 text-sm disabled:opacity-30 active:scale-95 transition-all shadow-sm flex items-center gap-2 ${
                agentBusy
                  ? "bg-base-overlay border border-surface text-text-subtle hover:border-accent/50 hover:text-text"
                  : "btn-primary"
              }`}
            >
              {agentBusy ? (
                <>
                  <ListOrdered size={15} />
                  Queue
                </>
              ) : (
                <>
                  <Send size={15} />
                  Send
                </>
              )}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
