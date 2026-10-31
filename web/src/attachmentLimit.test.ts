// Unit tests for attachment payload budgeting, run with
// `node --test` (Node >= 23 strips erasable TypeScript natively).
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES,
  estimateAttachmentPayloadBytes,
  isAttachmentPayloadWithinLimit,
  MAX_ATTACHMENT_PAYLOAD_BYTES,
} from "./attachmentLimit.ts";

// Revert-checks:
// - Separate files each round up to their own base64 quantum.
// - Removing the 64 KiB reserve or reverting to a 20 MiB budget breaks the
//   explicit constants and empty-frame check.
// - Ignoring prompt text (or counting UTF-16 characters instead of UTF-8
//   bytes) breaks the text-size cases.
// - Replacing the shared estimator or changing its boundary breaks the
//   exact-limit and over-limit cases.

test("rounds up base64 expansion separately for each file", () => {
  assert.equal(
    estimateAttachmentPayloadBytes("", [{ size: 1 }, { size: 1 }]),
    ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES + 8,
  );
  assert.equal(
    estimateAttachmentPayloadBytes("", [{ size: 3 }]),
    ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES + 4,
  );
});

test("reserves 64 KiB of the 19 MiB frame budget", () => {
  assert.equal(ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES, 64 * 1024);
  assert.equal(MAX_ATTACHMENT_PAYLOAD_BYTES, 19 * 1024 * 1024);
  assert.ok(MAX_ATTACHMENT_PAYLOAD_BYTES < 20 * 1024 * 1024);
  assert.equal(estimateAttachmentPayloadBytes("", []), 64 * 1024);
});

test("counts JSON-escaped text bytes", () => {
  const escapedText = "\\u0000".repeat(10);
  assert.equal(
    estimateAttachmentPayloadBytes(escapedText, []),
    ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES + new TextEncoder().encode(JSON.stringify(escapedText)).byteLength - 2,
  );
  assert.ok(
    estimateAttachmentPayloadBytes(escapedText, []) >
      ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES + new TextEncoder().encode(escapedText).byteLength,
  );
});

test("counts wire attachment data bytes", () => {
  const data = "AQIDBA==";
  assert.equal(
    estimateAttachmentPayloadBytes("", [{ data }]),
    ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES + new TextEncoder().encode(JSON.stringify(data)).byteLength - 2,
  );
});

test("accepts an explicit 10 MiB file and rejects an explicit 16 MiB file", () => {
  assert.equal(isAttachmentPayloadWithinLimit("", [{ size: 10 * 1024 * 1024 }]), true);
  assert.equal(isAttachmentPayloadWithinLimit("", [{ size: 16 * 1024 * 1024 }]), false);
});

test("counts prompt text in UTF-8 bytes", () => {
  const limitText = "a".repeat(MAX_ATTACHMENT_PAYLOAD_BYTES - ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES);
  assert.equal(estimateAttachmentPayloadBytes(limitText, []), MAX_ATTACHMENT_PAYLOAD_BYTES);
  assert.equal(isAttachmentPayloadWithinLimit(limitText, []), true);
  assert.equal(isAttachmentPayloadWithinLimit(`${limitText}a`, []), false);
  assert.equal(estimateAttachmentPayloadBytes("🙂", []), ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES + 4);
});

test("accepts an estimated payload at the limit", () => {
  const text = "x".repeat(MAX_ATTACHMENT_PAYLOAD_BYTES - ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES);
  assert.equal(isAttachmentPayloadWithinLimit(text, []), true);
});

test("rejects a combined payload above the 19 MiB budget", () => {
  const text = "x".repeat(MAX_ATTACHMENT_PAYLOAD_BYTES - ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES);
  assert.equal(isAttachmentPayloadWithinLimit(text, [{ size: 1 }]), false);
});
