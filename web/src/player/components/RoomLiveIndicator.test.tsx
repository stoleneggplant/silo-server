// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { roomLivePositionSeconds, type WatchTogetherRoomSnapshot } from "@/lib/watchTogether";
import { RoomLiveIndicator } from "./RoomLiveIndicator";

function playingRoom(
  overrides: Partial<WatchTogetherRoomSnapshot> = {},
): WatchTogetherRoomSnapshot {
  return {
    room_id: "room-1",
    phase: "playing",
    playback_state: "playing",
    selection_mode: "host_pick",
    selection_revision: 1,
    code: "ROOM",
    guest_control_policy: "host_only",
    is_paused: false,
    anchor_position_seconds: 600,
    anchor_updated_at: "2026-10-14T12:00:00Z",
    anchor_position_at: "2026-10-14T12:00:10Z",
    generation: 1,
    member_count: 2,
    host_connected: true,
    self_role: "guest",
    self_can_control_transport: false,
    self_can_manage_room: false,
    self_ignore_wait: false,
    ...overrides,
  };
}

describe("roomLivePositionSeconds", () => {
  const at = Date.parse("2026-10-14T12:00:10Z");

  it("advances from the instant the server described, on the server clock", () => {
    // The local clock runs 2s behind the server's.
    expect(roomLivePositionSeconds(playingRoom(), at + 3_000, 2_000)).toBe(605);
  });

  it("does not run before a play command's start", () => {
    expect(roomLivePositionSeconds(playingRoom(), at - 1_000, 0)).toBe(600);
  });

  it("has no live position unless the room plays and the server publishes its clock", () => {
    expect(roomLivePositionSeconds(playingRoom({ playback_state: "paused" }), at, 0)).toBeNull();
    expect(roomLivePositionSeconds(playingRoom({ playback_state: "waiting" }), at, 0)).toBeNull();
    expect(
      roomLivePositionSeconds(playingRoom({ anchor_position_at: undefined }), at, 0),
    ).toBeNull();
    expect(roomLivePositionSeconds(null, at, 0)).toBeNull();
  });
});

describe("RoomLiveIndicator", () => {
  afterEach(cleanup);

  it("reads LIVE at the room's position and offers nothing to press", () => {
    render(
      <RoomLiveIndicator state={{ liveEdgeSeconds: 600, behindSeconds: 2, onGoLive: vi.fn() }} />,
    );
    expect(screen.getByText("LIVE")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("offers to go live when the viewer has fallen behind", () => {
    const onGoLive = vi.fn();
    render(<RoomLiveIndicator state={{ liveEdgeSeconds: 600, behindSeconds: 75, onGoLive }} />);
    const button = screen.getByRole("button", { name: "Go live with the room, 1:15 behind" });
    fireEvent.click(button);
    expect(onGoLive).toHaveBeenCalledOnce();
  });
});
