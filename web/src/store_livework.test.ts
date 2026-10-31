import assert from "node:assert/strict";
import { test } from "node:test";
import { isStaleWakeSnapshot, mergeWakeSnapshot, pickPendingWake } from "./wakeSnapshot.ts";
import type { SessionWakeSchedulesPayload, WakeScheduleItem } from "./types.ts";


function schedule(id: string): WakeScheduleItem {
  return {
    id,
    kind: "wake",
    state: "active",
    message: `message ${id}`,
    nextRunAt: 100,
    createdAt: 1,
    everyMs: 0,
    occurrence: 0,
    maxRuns: 0,
    untilAt: 0,
  };
}

function snapshot(...schedules: WakeScheduleItem[]): SessionWakeSchedulesPayload {
  return { sessionID: "session-1", schedules };
}

test("pending cancellation stays open until its schedule disappears from snapshot", () => {
  const pending = schedule("cancel-me");
  assert.equal(pickPendingWake([schedule("cancel-me")], pending), pending);
  assert.equal(pickPendingWake([], pending), null);
  assert.equal(pickPendingWake([schedule("other")], pending), null);
  assert.equal(pickPendingWake([{ ...pending, state: "cancelled" }], pending), null);
});

test("wake schedule snapshots discard superseded request replies", () => {
  const oldID = "wakesched-get-old";
  const newID = "wakesched-get-new";
  const latest = new Map<string, string>();
  latest.set("session-1", newID);
  const isTagged = (id: string) => id.startsWith("wakesched-get-") || id.startsWith("wakesched-cancel-");
  assert.equal(isStaleWakeSnapshot(latest, "session-1", oldID, isTagged), true);
  assert.equal(isStaleWakeSnapshot(latest, "session-1", newID, isTagged), false);
  assert.equal(isStaleWakeSnapshot(latest, "session-1", undefined, isTagged), false);

  let data = mergeWakeSnapshot(new Map(), snapshot(schedule("new")));
  if (!isStaleWakeSnapshot(latest, "session-1", newID, isTagged)) data = mergeWakeSnapshot(data, snapshot(schedule("new")));
  if (!isStaleWakeSnapshot(latest, "session-1", oldID, isTagged)) data = mergeWakeSnapshot(data, snapshot(schedule("old")));
  assert.deepEqual(data.get("session-1"), [schedule("new")]);
});

test("wake schedule request freshness is isolated by session", () => {
  const latest = new Map([["session-a", "wakesched-get-a"], ["session-b", "wakesched-get-b"]]);
  const isTagged = (id: string) => id.startsWith("wakesched-get-");
  assert.equal(isStaleWakeSnapshot(latest, "session-a", "wakesched-get-a", isTagged), false);
  assert.equal(isStaleWakeSnapshot(latest, "session-a", "wakesched-get-b", isTagged), true);
});
