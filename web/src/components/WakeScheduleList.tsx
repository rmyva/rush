// Schedules tab body for LiveWorkPanel (stage 5b): the active session's
// wake schedules from the server's durable wake_schedules snapshot
// (session_wake_schedules events). Each active row carries an explicit
// Cancel button gated by the shared ConfirmDialog; the server's fresh
// snapshot reply is what actually removes the row.

import { useState } from "react";
import { useStore } from "@nanostores/react";
import type { WakeScheduleItem } from "../types";
import { sendCancelWakeSchedule, $wakeScheduleCancelError, pickPendingWakeSchedule } from "../store_livework";
import { ConfirmDialog } from "./ConfirmDialog";

function formatLocalTime(ms: number): string {
  return new Date(ms).toLocaleString();
}

function recurrenceLabel(item: WakeScheduleItem): string {
  if (item.kind !== "loop") return "one-time";
  const total = item.maxRuns > 0 ? String(item.maxRuns) : "∞";
  const parts = [`every ${Math.round(item.everyMs / 1000)}s`, `${item.occurrence}/${total}`];
  if (item.untilAt > 0) parts.push(`until ${formatLocalTime(item.untilAt)}`);
  return parts.join(" · ");
}

function WakeScheduleRow({ item, onCancelRequest }: {
  item: WakeScheduleItem;
  onCancelRequest: (item: WakeScheduleItem) => void;
}) {
  const active = item.state === "active";
  return (
    <div data-test-id="wake-schedule-row" className="flex items-center gap-2 px-2 py-1.5 rounded hover:bg-base-overlay/50 transition-colors">
      <span data-test-id="wake-schedule-kind" className="text-mauve font-semibold text-sm shrink-0">
        {item.kind === "loop" ? "loop" : "wake"}
      </span>
      <span className="text-text font-mono text-sm truncate flex-1 min-w-0" title={item.message} style={{ fontSize: "var(--chat-font-size)" }}>
        {item.message || "—"}
      </span>
      <span data-test-id="wake-schedule-detail" className="text-text-subtle text-xs font-mono shrink-0">
        {recurrenceLabel(item)}
      </span>
      {item.state === "active" ? (
        <span className="text-text-subtle text-xs font-mono tabular-nums shrink-0">
          {formatLocalTime(item.nextRunAt)}
        </span>
      ) : (
        <span data-test-id="wake-schedule-state" className="text-text-subtle text-xs font-mono shrink-0">
          {item.state}
        </span>
      )}
      {active && (
        <button
          type="button"
          data-test-id="wake-schedule-cancel"
          onClick={() => onCancelRequest(item)}
          className="shrink-0 px-2 py-0.5 text-xs rounded text-red hover:bg-red/10 transition-colors"
        >
          Cancel
        </button>
      )}
    </div>
  );
}

export function WakeScheduleList({ items, sessionID, emptyLabel }: {
  items: WakeScheduleItem[];
  sessionID: string;
  emptyLabel: string;
}) {
  const [pending, setPending] = useState<WakeScheduleItem | null>(null);
  const cancelError = useStore($wakeScheduleCancelError);

  function requestCancel(item: WakeScheduleItem) {
    $wakeScheduleCancelError.set(null);
    setPending(item);
  }

  function closeCancel() {
    $wakeScheduleCancelError.set(null);
    setPending(null);
  }

  // Confirm sends and KEEPS the dialog open: the server's fresh snapshot is
  // what removes the row, while a rejected cancel lands as an inline error
  // via the dialog's error prop. Success detection is the row's
  // disappearance from the snapshot -- the cancel reply carries no request
  // correlation beyond its WS id. Derived during render (the same pattern
  // as LiveWorkPanel's effectiveTab, deliberately NOT a synchronizing
  // effect): the dialog shows only while the pending row still exists, so
  // the snapshot that removes it closes the dialog with no extra state.
  const effectivePending = pickPendingWakeSchedule(items, pending);

  function confirmCancel() {
    if (!effectivePending) return;
    sendCancelWakeSchedule(sessionID, effectivePending.id);
  }

  return (
    <div data-test-id="wake-schedule-list" className="px-2 py-2">
      {items.length === 0 ? (
        <p className="text-text-muted px-2 py-1" style={{ fontSize: "var(--chat-font-size)" }}>{emptyLabel}</p>
      ) : (
        items.map((item) => (
          <WakeScheduleRow key={item.id} item={item} onCancelRequest={requestCancel} />
        ))
      )}
      {effectivePending && (
        <ConfirmDialog
          title="Cancel wake schedule"
          message={`Stop "${effectivePending.message || effectivePending.id}" from waking the agent${effectivePending.kind === "loop" ? " (all future occurrences)" : ""}?`}
          confirmLabel="Cancel schedule"
          error={cancelError}
          onConfirm={confirmCancel}
          onCancel={closeCancel}
        />
      )}
    </div>
  );
}
