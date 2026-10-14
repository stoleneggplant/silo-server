import { formatTime } from "../utils/formatTime";

/** Where this viewer is relative to a Watch Party room's live position. */
export interface RoomLiveState {
  liveEdgeSeconds: number;
  /** Seconds this viewer is behind the room; negative when ahead. */
  behindSeconds: number;
  onGoLive: () => void;
}

/**
 * Behind by no more than this, the viewer counts as live. Short local stalls
 * and the scheduling lead of a command stay inside it, so the indicator does
 * not flicker.
 */
export const roomLiveToleranceSeconds = 3;

/**
 * A Watch Party viewer is never moved to the room's position on their own.
 * When they fall behind, this offers the jump; when they are at the room's
 * position it reads LIVE.
 */
export function RoomLiveIndicator({ state }: { state: RoomLiveState }) {
  const offSeconds = Math.abs(state.behindSeconds);
  if (offSeconds <= roomLiveToleranceSeconds) {
    return (
      <span className="inline-flex shrink-0 items-center gap-1 font-mono text-[11px] tracking-[0.12em] text-white/85 normal-case">
        <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-red-500" />
        LIVE
      </span>
    );
  }
  const offset = formatTime(offSeconds);
  const ahead = state.behindSeconds < 0;
  return (
    <button
      type="button"
      onClick={state.onGoLive}
      aria-label={`Go live with the room, ${offset} ${ahead ? "ahead" : "behind"}`}
      className="inline-flex shrink-0 items-center gap-1 rounded-full bg-white/10 px-2 py-0.5 font-mono text-[11px] tracking-[0.12em] text-white/85 normal-case transition-colors hover:bg-white/20 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none"
    >
      <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-white/50" />
      GO LIVE
      <span className="text-white/55 tabular-nums">
        {ahead ? "+" : "−"}
        {offset}
      </span>
    </button>
  );
}
