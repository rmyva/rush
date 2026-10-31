export const MAX_ATTACHMENT_PAYLOAD_BYTES = 19 * 1024 * 1024;
export const ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES = 64 * 1024;

export interface AttachmentSizeInput {
  size?: number;
  data?: string;
}

/**
 * Estimate a complete JSON WebSocket frame. The server's read limit is
 * 20 MiB (internal/server/hub.go:maxMessageSize); keep a 1 MiB margin and
 * reserve 64 KiB of that budget for JSON metadata and framing.
 */
export function estimateAttachmentPayloadBytes(
  text: string,
  attachments: readonly AttachmentSizeInput[],
): number {
  // WebSocket sends JSON, so account for JSON escapes (notably control
  // characters) rather than only the UTF-8 bytes of the original text.
  const textBytes = new TextEncoder().encode(JSON.stringify(text)).byteLength - 2;
  const encodedAttachments = attachments.reduce(
    (total, attachment) => {
      const encodedSize = attachment.data !== undefined
        ? new TextEncoder().encode(JSON.stringify(attachment.data)).byteLength - 2
        : 4 * Math.ceil((attachment.size ?? 0) / 3);
      return total + encodedSize;
    },
    0,
  );
  return textBytes + encodedAttachments + ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES;
}

export function isAttachmentPayloadWithinLimit(
  text: string,
  attachments: readonly AttachmentSizeInput[],
): boolean {
  return estimateAttachmentPayloadBytes(text, attachments) <= MAX_ATTACHMENT_PAYLOAD_BYTES;
}
