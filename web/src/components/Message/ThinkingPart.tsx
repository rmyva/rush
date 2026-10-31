// Collapsible reasoning card with edit/delete affordances.
// Pure code move from the former components/Message.tsx.

import { useState, useCallback, memo } from "react";
import { BrainCircuit, Pencil, Trash2, ChevronDown, ChevronUp } from "lucide-react";
import { updateMessageThinking, deleteMessagePart } from "../../store";
import { ConfirmDialog } from "../ConfirmDialog";
import { CopyButton } from "./CopyButton";
import { EditForm } from "./EditForm";
import { EffortBadge } from "./EffortBadge";
import { useCollapseAllSignal } from "./useCollapseAllSignal";

// ── ThinkingPart — owns its own edit/delete state ─────────────────────────────

export const ThinkingPart = memo(function ThinkingPart({ thinking, messageID, partIndex, done, model, effort }: { thinking: string; messageID: string; partIndex: number; done: boolean; model?: string; effort?: string }) {
  const [editing, setEditing] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  // Controlled open-state (not <details>) so we can render a sticky
  // collapse strip at the bottom — long reasoning blocks are tedious to
  // close when the toggle is only at the top.
  const [open, setOpen] = useState(false);
  // Collapsing the card (toggle, sticky bottom strip, or "Collapse all")
  // also exits edit mode — a stashed edit form that silently reappears on
  // the next expand is the same ambush shape as the delete dialog.
  const collapse = useCallback(() => { setOpen(false); setEditing(false); }, []);
  useCollapseAllSignal(collapse);

  const closeEdit  = useCallback(() => setEditing(false), []);
  const openDel    = useCallback((e: React.MouseEvent) => { e.preventDefault(); e.stopPropagation(); setConfirmDelete(true); }, []);
  // Editing a collapsed card implicitly means the operator now wants to see
  // and edit its content, so force the card open (the EditForm renders in
  // the card body).
  const openEditEv = useCallback((e: React.MouseEvent) => { e.preventDefault(); e.stopPropagation(); setEditing(true); setOpen(true); }, []);
  const toggleOpen = useCallback(() => {
    if (open) collapse();
    else setOpen(true);
  }, [open, collapse]);

  const handleSave = useCallback((text: string) => {
    if (text && text !== thinking) updateMessageThinking(messageID, text, partIndex);
    setEditing(false);
  }, [thinking, messageID, partIndex]);

  const handleConfirmDelete = useCallback(() => {
    deleteMessagePart(messageID, partIndex);
    setConfirmDelete(false);
  }, [messageID, partIndex]);

  // While the model is still thinking we always render fully (no toggle);
  // streaming reasoning hidden behind a click is useless.
  if (!done) {
    return (
      <div data-test-id="thinking-card" className="thinking-card">
        <div className="thinking-card-header">
          <BrainCircuit size={15} className="text-accent/70 shrink-0 animate-pulse" />
          <span data-test-id="thinking-label">Thinking…</span>
          {model && <span className="text-xs text-text-subtle font-mono">{model}</span>}
          <EffortBadge effort={effort} />
        </div>
        {thinking && (
          <pre data-test-id="thinking-content" className="px-4 pb-3 font-mono whitespace-pre-wrap text-text-subtle leading-relaxed max-h-40 overflow-y-auto border-t border-surface/50" style={{ fontSize: "var(--chat-font-size)" }}>
            {thinking}
          </pre>
        )}
      </div>
    );
  }

  return (
    <div data-test-id="thinking-card" className="thinking-card-done group">
      <button
        type="button"
        onClick={toggleOpen}
        data-test-id="thinking-toggle"
        className="thinking-toggle w-full text-left"
        aria-expanded={open}
      >
        <span className="text-accent/70"><BrainCircuit size={18} /></span>
        <span data-test-id="thinking-label">Thoughts</span>
        {model && <span className="text-xs text-text-subtle font-mono">{model}</span>}
        <EffortBadge effort={effort} />
        <div className="ml-auto flex items-center gap-0.5 hover-reveal" onClick={(e) => e.stopPropagation()}>
          <CopyButton text={thinking} className="px-1.5 py-1 text-xs" />
          <button onClick={openEditEv} title="Edit thinking"   className="btn-icon-sm"><Pencil size={13} /></button>
          <button onClick={openDel}    title="Delete thinking" className="btn-icon-sm-danger"><Trash2 size={13} /></button>
        </div>
        <span className="text-text-subtle ml-1 shrink-0">
          {open ? <ChevronUp size={16} /> : <ChevronDown size={16} />}
        </span>
      </button>
      {/* The confirm dialog is a fixed overlay and must render regardless of
          the card's open state — the Delete button lives in the collapsed
          header, so gating on `open` swallowed the click and ambushed the
          operator with the destructive dialog on a later, unrelated expand
          (ActionRow.tsx renders its dialog the same unguarded way). */}
      {confirmDelete && (
        <ConfirmDialog
          title="Delete thinking"
          message="The model's reasoning will be removed from this message. This cannot be undone."
          confirmLabel="Delete"
          onConfirm={handleConfirmDelete}
          onCancel={() => setConfirmDelete(false)}
        />
      )}
      {/* The EditForm is likewise ungated: `editing` is only ever true while
          open (openEditEv forces open, collapse clears editing), and keeping
          the form outside the open guard means a future regression can never
          re-introduce the swallowed click. */}
      {editing ? (
        <div className="p-4 bg-base-overlay border-t border-surface">
          <EditForm
            initialValue={thinking}
            rows={6}
            className="w-full bg-base-subtle border border-accent/40 text-text-muted rounded-lg px-4 py-3 text-[14px] font-mono leading-relaxed resize-none outline-none focus:border-accent"
            onSave={handleSave}
            onCancel={closeEdit}
          />
        </div>
      ) : open ? (
        <>
          <pre data-test-id="thinking-content" className="p-5 bg-base-overlay font-mono whitespace-pre-wrap overflow-x-auto text-text-muted border-t border-surface leading-relaxed" style={{ fontSize: "var(--chat-font-size)" }}>
            {thinking}
          </pre>
          {/* Sticky bottom collapse strip: stays visible as the user scrolls
              past the reasoning so closing it is one click away — no need to
              scroll back up to the header toggle. */}
          <button
            type="button"
            onClick={toggleOpen}
            data-test-id="thinking-collapse-bottom"
            className="thinking-collapse-bottom"
            title="Collapse thinking"
          >
            <ChevronUp size={14} />
            <span>Collapse thinking</span>
          </button>
        </>
      ) : null}
    </div>
  );
});
