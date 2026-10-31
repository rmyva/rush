// Regression tests for keepAlive's visibility listener ownership.
// Run with `node --test` (Node >= 23 strips erasable TypeScript natively).
import assert from "node:assert/strict";
import { test } from "node:test";
import { installKeepAliveAutoResume } from "./keepAlive.ts";

test("duplicate installation cleanup cannot uninstall the active listener", () => {
  const listeners = new Set<EventListenerOrEventListenerObject>();
  const previousDocument = globalThis.document;
  globalThis.document = {
    addEventListener(type: string, listener: EventListenerOrEventListenerObject) {
      assert.equal(type, "visibilitychange");
      listeners.add(listener);
    },
    removeEventListener(type: string, listener: EventListenerOrEventListenerObject) {
      assert.equal(type, "visibilitychange");
      listeners.delete(listener);
    },
  } as unknown as Document;

  try {
    const cleanupOwner = installKeepAliveAutoResume();
    const cleanupDuplicate = installKeepAliveAutoResume();
    assert.equal(listeners.size, 1);

    cleanupDuplicate();
    assert.equal(listeners.size, 1, "duplicate cleanup leaves the owner's listener installed");

    cleanupOwner();
    assert.equal(listeners.size, 0, "owner cleanup removes its listener");
    cleanupOwner();
    assert.equal(listeners.size, 0, "cleanup is idempotent");
  } finally {
    for (const listener of listeners) document.removeEventListener("visibilitychange", listener);
    globalThis.document = previousDocument;
  }
});

test("stale cleanup cannot remove a subsequent installation", () => {
  const listeners = new Set<EventListenerOrEventListenerObject>();
  const previousDocument = globalThis.document;
  globalThis.document = {
    addEventListener(_type: string, listener: EventListenerOrEventListenerObject) {
      listeners.add(listener);
    },
    removeEventListener(_type: string, listener: EventListenerOrEventListenerObject) {
      listeners.delete(listener);
    },
  } as unknown as Document;

  try {
    const staleCleanup = installKeepAliveAutoResume();
    staleCleanup();
    const cleanupCurrent = installKeepAliveAutoResume();
    assert.equal(listeners.size, 1);

    staleCleanup();
    assert.equal(listeners.size, 1, "stale cleanup preserves the current listener");

    cleanupCurrent();
    assert.equal(listeners.size, 0);
  } finally {
    for (const listener of listeners) document.removeEventListener("visibilitychange", listener);
    globalThis.document = previousDocument;
  }
});
