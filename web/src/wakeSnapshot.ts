export interface WakeSnapshot<T> {
  sessionID: string;
  schedules?: T[];
}

export function isStaleWakeSnapshot(latestBySession: Map<string, string>, sessionID: string | undefined, requestID: string | undefined, isTaggedRequest: (id: string) => boolean): boolean {
  if (!sessionID || !requestID || !isTaggedRequest(requestID)) return false;
  const latest = latestBySession.get(sessionID);
  return !!latest && latest !== requestID;
}

export function mergeWakeSnapshot<T>(current: Map<string, T[]>, payload: WakeSnapshot<T>): Map<string, T[]> {
  const next = new Map(current);
  next.set(payload.sessionID, payload.schedules ?? []);
  return next;
}

export function pickPendingWake<T extends { id: string; state: string }>(items: T[], pending: T | null): T | null {
  return pending && items.some((item) => item.id === pending.id && item.state === "active") ? pending : null;
}
