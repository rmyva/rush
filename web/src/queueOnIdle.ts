export interface IdleQueuePayload { content: string; attachments: unknown[] }

export function drainSendableQueue<T extends { content: string; attachments?: unknown[]; failureState?: string }>(items: T[]): { sendable: T[]; retained: T[] } {
  return { sendable: items.filter((item) => !item.failureState), retained: items.filter((item) => !!item.failureState) };
}

export interface QueuedItemDependencies<T, E = unknown> {
  take(sessionID: string, id: string): { item: T; index: number } | undefined;
  request(sessionID: string, item: T): Promise<unknown>;
  restore(sessionID: string, item: T, index: number, error: E): void;
  describeFailure(error: unknown): E;
}

export async function sendQueuedItem<T, E>(sessionID: string, id: string, deps: QueuedItemDependencies<T, E>): Promise<void> {
  const taken = deps.take(sessionID, id);
  if (!taken) return;
  try {
    await deps.request(sessionID, taken.item);
  } catch (error) {
    deps.restore(sessionID, taken.item, taken.index, deps.describeFailure(error));
  }
}

export interface IdleQueueDependencies<T extends IdleQueuePayload> {
  dequeue(sessionID: string): T | undefined;
  send(payload: { sessionID: string; content: string; attachments?: unknown[] }): boolean;
  restore(sessionID: string, content: string, attachments: unknown[]): void;
}

/** Production busy=false queue drain, send, and failed-send restoration seam. */
export function flushQueueOnIdle<T extends IdleQueuePayload>(sessionID: string, deps: IdleQueueDependencies<T>): void {
  const flushed = deps.dequeue(sessionID);
  if (!flushed) return;
  const payload: { sessionID: string; content: string; attachments?: unknown[] } = { sessionID, content: flushed.content };
  if (flushed.attachments.length > 0) payload.attachments = flushed.attachments;
  if (!deps.send(payload)) deps.restore(sessionID, flushed.content, flushed.attachments);
}
