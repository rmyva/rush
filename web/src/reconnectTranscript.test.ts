import assert from "node:assert/strict";
import { test } from "node:test";
import { reloadReconnectTranscript } from "./reconnectTranscript.ts";

test("reconnect reload dispatches a load for the active transcript", () => {
  const loaded: string[] = [];
  reloadReconnectTranscript("session-a", (sessionID) => loaded.push(sessionID));
  assert.deepEqual(loaded, ["session-a"]);
});

test("reconnect without an active transcript does not request history", () => {
  const loaded: string[] = [];
  reloadReconnectTranscript(null, (sessionID) => loaded.push(sessionID));
  assert.deepEqual(loaded, []);
});
