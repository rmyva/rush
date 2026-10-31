// Regression tests for nonterminal assistant snapshot rebuilding in store.ts.
// Run with `node --test` (Node >= 23 strips erasable TypeScript natively).
// Revert-checks:
//   - "nonterminal rebuild preserves incoming part order..." kills rebuilding
//     by concatenating thinking/text before advancing parts.
//   - "nonterminal rebuild drops abandoned advancing parts..." kills copying
//     stale tool/finish parts from the existing message into the rebuilt one.

import assert from "node:assert/strict";
import { test } from "node:test";
import type { Message } from "./types.ts";
import { mergePreserveContent } from "./mergeMessage.ts";

function makeMessage(parts: Message["Parts"]): Message {
  return {
    ID: "m1",
    SessionID: "s1",
    Role: "assistant",
    Parts: parts,
    Model: "",
    Provider: "",
    ReasoningEffort: "",
    CreatedAt: 1,
    UpdatedAt: 2,
    IsSummaryMessage: false,
    Pinned: false,
    Hidden: false,
    AutoResumed: false,
    BackgroundJobNotice: false,
  };
}

test("nonterminal merge preserves incoming part order and retained content positions", () => {
  const existing = makeMessage([
    { type: "thinking", Thinking: "reasoning that remains" },
    { type: "tool_call", ID: "old-call", Name: "old", Input: "{}", Finished: true },
    { type: "text", Text: "answer that remains" },
    { type: "tool_result", ToolCallID: "old-call", Name: "old", Content: "old result", IsError: false },
  ]);
  const incoming = makeMessage([
    { type: "tool_call", ID: "new-call", Name: "new", Input: "{}", Finished: false },
    { type: "thinking", Thinking: "short" },
    { type: "tool_result", ToolCallID: "new-call", Name: "new", Content: "new result", IsError: false },
    { type: "text", Text: "short" },
  ]);

  const merged = mergePreserveContent(existing, incoming);
  assert.deepEqual(merged.Parts.map((part) => part.type), ["tool_call", "thinking", "tool_result", "text"]);
  assert.deepEqual(merged.Parts, [
    incoming.Parts[0],
    existing.Parts[0],
    incoming.Parts[2],
    existing.Parts[2],
  ]);
});

test("nonterminal rebuild preserves interleaving of repeated regressed parts", async () => {
  const existing = makeMessage([
    { type: "thinking", Thinking: "first reasoning that remains" },
    { type: "tool_call", ID: "call-1", Name: "one", Input: "{}", Finished: true },
    { type: "thinking", Thinking: "second reasoning that remains" },
    { type: "tool_call", ID: "call-2", Name: "two", Input: "{}", Finished: true },
    { type: "text", Text: "answer that remains" },
  ]);
  const incoming = makeMessage([
    { type: "thinking", Thinking: "short" },
    { type: "tool_call", ID: "call-1", Name: "one", Input: "{}", Finished: true },
    { type: "thinking", Thinking: "short" },
    { type: "tool_call", ID: "call-2", Name: "two", Input: "{}", Finished: true },
    { type: "text", Text: "short" },
  ]);

  const merged = mergePreserveContent(existing, incoming);
  assert.deepEqual(merged.Parts, [
    existing.Parts[0],
    incoming.Parts[1],
    existing.Parts[2],
    incoming.Parts[3],
    existing.Parts[4],
  ]);
});

test("nonterminal rebuild drops abandoned advancing parts and preserves an absent regressed kind", async () => {
  const existing = makeMessage([
    { type: "tool_call", ID: "abandoned", Name: "old", Input: "{}", Finished: true },
    { type: "thinking", Thinking: "keep this reasoning" },
    { type: "text", Text: "old answer" },
  ]);
  const incoming = makeMessage([
    { type: "tool_call", ID: "current", Name: "new", Input: "{}", Finished: false },
    { type: "text", Text: "" },
  ]);

  const merged = mergePreserveContent(existing, incoming);
  assert.deepEqual(merged.Parts, [incoming.Parts[0], existing.Parts[1], existing.Parts[2]]);
  assert.equal(merged.Parts.some((part) => part.type === "tool_call" && part.ID === "abandoned"), false);
});
