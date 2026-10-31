// keepAlive — silent broadband-ish noise generator that keeps the browser
// tab "audibly alive". Purpose: prevent Bluetooth headphones (and some
// laptop audio chipsets) from suspending the output stream during long
// agent-runs, which causes the first second of any real notification /
// system beep to be eaten.
//
// Persistence note: this module owns ONLY the runtime state (is the
// AudioContext currently producing noise?). The user's preference lives
// in the global rush.json on the backend (Options.KeepAliveEnabled) and
// reaches the web UI via the `config` WebSocket event. Sync between
// preference and runtime happens in useWS.ts.
//
// Design:
//   - Singleton AudioContext + buffer-source loop, lazy-built on start().
//   - White-noise sample feeds a lowpass biquad (6 kHz default, "soft
//     hiss") feeds a gain node held at a very small constant (≈0.001 ≈
//     −60 dB relative to ceiling), then to destination.
//   - visibilitychange handler resumes the AudioContext if the OS /
//     browser suspended it while the tab was hidden.

const NOISE_BUFFER_SECONDS = 2;
// Lowpass cutoff. Lower = less high-frequency content, which the ear
// perceives as quieter at equal amplitude (equal-loudness contours).
// 1500 Hz keeps enough body for the audio device to register the stream
// while pushing the spectral peak well below the 2–5 kHz band where the
// ear is most sensitive. Was 6000 Hz — operators reported it as audible
// hiss on quiet headphones.
const LOWPASS_HZ = 1500;
// Effective output level. Trickier than it looks: the BROWSER's tab-is-
// playing-audio detector watches signal RMS, but the human ear weights
// the perceived loudness via equal-loudness contours. Narrowing the
// lowpass 6000→1500 Hz already drops broadband RMS by ~12 dB, so we
// compensate by RAISING the gain above the original 0.001 — the tab
// indicator (and Bluetooth-keep-alive) tracks RMS and needs it back;
// the ear barely notices because almost no energy sits in the
// sensitive 2–5 kHz band any more. 0.0012 (≈ −58 dBFS) lands at a
// stable RMS the browser reliably detects while staying inaudible on
// normal listening volumes. The earlier 0.0002 + 1500 Hz combo went
// silent enough for the tab to drop its audio-playing icon entirely.
const GAIN = 0.0012;

let ctx: AudioContext | null = null;
let source: AudioBufferSourceNode | null = null;
let filter: BiquadFilterNode | null = null;
let gainNode: GainNode | null = null;
let running = false;
let autoResumeListener: (() => void) | null = null;

function buildNoiseBuffer(audio: AudioContext): AudioBuffer {
  const len = audio.sampleRate * NOISE_BUFFER_SECONDS;
  const buf = audio.createBuffer(1, len, audio.sampleRate);
  const d = buf.getChannelData(0);
  for (let i = 0; i < len; i++) d[i] = Math.random() * 2 - 1;
  return buf;
}

export function isKeepAliveRunning(): boolean {
  return running;
}

export function startKeepAlive(): boolean {
  if (running) return true;
  // AudioContext construction must happen inside a user-gesture handler
  // (modern autoplay policy). Without a recent gesture the AudioContext
  // is constructed in "suspended" state — visibilitychange/click handlers
  // upstream of us will call .resume() when one becomes available.
  const AC = window.AudioContext || (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
  if (!AC) return false;
  try {
    ctx = new AC();
    source = ctx.createBufferSource();
    source.buffer = buildNoiseBuffer(ctx);
    source.loop = true;
    filter = ctx.createBiquadFilter();
    filter.type = "lowpass";
    filter.frequency.value = LOWPASS_HZ;
    gainNode = ctx.createGain();
    gainNode.gain.value = GAIN;
    source.connect(filter).connect(gainNode).connect(ctx.destination);
    source.start();
    running = true;
    return true;
  } catch {
    stopKeepAlive();
    return false;
  }
}

export function stopKeepAlive(): void {
  if (!running && !ctx) return;
  try { source?.stop(); } catch { /* noop */ }
  source?.disconnect();
  filter?.disconnect();
  gainNode?.disconnect();
  ctx?.close().catch(() => { /* noop */ });
  source = null;
  filter = null;
  gainNode = null;
  ctx = null;
  running = false;
}

// installKeepAliveAutoResume wires a visibilitychange listener that nudges
// the AudioContext back to "running" if it was suspended in the
// background. Repeated calls are idempotent; cleanup removes the listener
// and allows a later installation (e.g. after StrictMode effect replay).
export function installKeepAliveAutoResume(): () => void {
  if (autoResumeListener) return () => {};
  const listener = () => {
    if (!running || !ctx) return;
    if (ctx.state === "suspended") {
      ctx.resume().catch(() => { /* noop */ });
    }
  };
  autoResumeListener = listener;
  document.addEventListener("visibilitychange", listener);
  return () => {
    // A stale cleanup (or a cleanup for a duplicate installation) must not
    // remove a listener installed by a later owner.
    if (autoResumeListener !== listener) return;
    document.removeEventListener("visibilitychange", listener);
    autoResumeListener = null;
  };
}

export function uninstallKeepAliveAutoResume(): void {
  if (!autoResumeListener) return;
  document.removeEventListener("visibilitychange", autoResumeListener);
  autoResumeListener = null;
}
