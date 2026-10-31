// Reconnect may have missed message_created/updated/deleted pushes. Reload the
// active transcript directly rather than relying on sessions_list routing.
export function reloadReconnectTranscript(activeSessionID: string | null, loadMessages: (sessionID: string) => void): void {
  if (activeSessionID) loadMessages(activeSessionID);
}
