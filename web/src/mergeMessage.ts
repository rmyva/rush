import type { ContentPart, Message } from "./types";

function partsOfKind<T extends ContentPart["type"]>(parts: ContentPart[], kind: T): Extract<ContentPart, { type: T }>[] {
  return parts.filter((p): p is Extract<ContentPart, { type: T }> => p.type === kind);
}

function total(parts: ContentPart[], kind: "thinking" | "text"): string {
  return parts.filter((p) => p.type === kind).map((p) => {
    if (p.type === "thinking") return p.Thinking;
    if (p.type === "text") return p.Text;
    return "";
  }).join("");
}

function terminal(parts: ContentPart[]): boolean {
  return parts.some((p) => p.type === "finish" && !p.Partial);
}

/** Keeps regressed accumulating content while respecting the incoming structural part order. */
export function mergePreserveContent(existing: Message, incoming: Message): Message {
  if (terminal(incoming.Parts)) return incoming;
  const thinkingRegressed = total(incoming.Parts, "thinking").length < total(existing.Parts, "thinking").length;
  const textRegressed = total(incoming.Parts, "text").length < total(existing.Parts, "text").length;
  if (!thinkingRegressed && !textRegressed) return incoming;

  const regressedKinds = new Set<ContentPart["type"]>();
  if (thinkingRegressed) regressedKinds.add("thinking");
  if (textRegressed) regressedKinds.add("text");
  const rebuilt: ContentPart[] = [];
  const emittedCounts = new Map<ContentPart["type"], number>();
  for (const part of incoming.Parts) {
    if (!regressedKinds.has(part.type)) { rebuilt.push(part); continue; }
    const index = emittedCounts.get(part.type) ?? 0;
    const protectedParts = partsOfKind(existing.Parts, part.type);
    if (index < protectedParts.length) {
      rebuilt.push(protectedParts[index]);
      emittedCounts.set(part.type, index + 1);
    } else rebuilt.push(part);
  }
  for (const kind of regressedKinds) {
    const protectedParts = partsOfKind(existing.Parts, kind);
    const emittedCount = emittedCounts.get(kind) ?? 0;
    if (emittedCount >= protectedParts.length) continue;
    const firstExistingIndex = existing.Parts.findIndex((part) => part.type === kind);
    const insertAtIncomingIndex = incoming.Parts.findIndex((part) => {
      const existingIndex = existing.Parts.findIndex((candidate) => candidate.type === part.type && JSON.stringify(candidate) === JSON.stringify(part));
      return existingIndex !== -1 && existingIndex > firstExistingIndex;
    });
    const insertAt = insertAtIncomingIndex === -1 ? Math.min(Math.max(firstExistingIndex, 0), rebuilt.length) : Math.min(insertAtIncomingIndex, rebuilt.length);
    rebuilt.splice(insertAt, 0, ...protectedParts.slice(emittedCount));
  }
  return { ...incoming, Parts: rebuilt };
}
