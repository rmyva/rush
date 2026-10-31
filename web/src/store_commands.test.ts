// Native-Node regression tests for the production queue seams.
import assert from "node:assert/strict";
import { test } from "node:test";
import { sendQueuedItem, flushQueueOnIdle, drainSendableQueue } from "./queueOnIdle.ts";

test("ambiguous per-item request restores before busy=false and is never sent as send_message", async () => {
  const events: string[] = [];
  const queued = [{ id: "q1", content: "payload", attachments: [] as unknown[], error: undefined as string | undefined, failureState: undefined as string | undefined }];
  let busy = true;
  await sendQueuedItem("s3", "q1", {
    take: (_sessionID, id) => {
      events.push("take");
      const index = queued.findIndex((item) => item.id === id);
      if (index < 0) return undefined;
      const [item] = queued.splice(index, 1);
      return { item, index };
    },
    request: async () => {
      events.push("wsRequest");
      throw { ambiguous: true };
    },
    describeFailure: () => "ambiguous",
    restore: (_sessionID, item, index, error) => {
      events.push("restore");
      queued.splice(index, 0, { ...item, error, failureState: "ambiguous" });
    },
  });

  busy = false;
  events.push("busy=false");
  flushQueueOnIdle("s3", {
    dequeue: () => {
      const { sendable, retained } = drainSendableQueue(queued);
      queued.splice(0, queued.length, ...retained);
      return sendable.length ? { content: sendable[0].content, attachments: [] } : undefined;
    },
    send: () => { events.push("send_message"); return true; },
    restore: () => assert.fail("no failed idle send expected"),
  });

  assert.equal(busy, false);
  assert.deepEqual(events, ["take", "wsRequest", "restore", "busy=false"]);
  assert.equal(queued[0].failureState, "ambiguous");
});

test("busy=false sends only clean queue items and retains rejected items", () => {
  const queue = [
    { id: "before", content: "earlier", attachments: [], failureState: undefined as string | undefined },
    { id: "rejected", content: "must not retry", attachments: [], failureState: "rejected" },
    { id: "after", content: "later", attachments: [], failureState: undefined as string | undefined },
  ];
  const sent: unknown[] = [];
  flushQueueOnIdle("s1", {
    dequeue: () => {
      const { sendable, retained } = drainSendableQueue(queue);
      queue.splice(0, queue.length, ...retained);
      return sendable.length ? { content: sendable.map((item) => item.content).join("\n\n"), attachments: [] } : undefined;
    },
    send: (payload) => { sent.push(payload); return true; },
    restore: () => assert.fail("successful idle send must not restore"),
  });
  assert.deepEqual(sent, [{ sessionID: "s1", content: "earlier\n\nlater" }]);
  assert.deepEqual(queue.map((item) => item.id), ["rejected"]);
});

test("busy=false restores drained queue content when injected send fails", () => {
  let restored: unknown;
  flushQueueOnIdle("s2", {
    dequeue: () => ({ content: "queued", attachments: [{ name: "a" }] }),
    send: () => false,
    restore: (...args) => { restored = args; },
  });
  assert.deepEqual(restored, ["s2", "queued", [{ name: "a" }]]);
});
