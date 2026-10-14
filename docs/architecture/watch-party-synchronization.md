# Watch Party synchronization

Postgres owns the room anchor and its `runtime` JSONB value: connection identities,
leased presence, attached playback sessions, readiness, and the latest room
transport command. Each room operation locks its room row and commits the anchor
and runtime changes together. Commands and snapshots are delivered after commit.
Other rooms use independent locks. Playback ownership and catalog lookups happen
before acquiring the room transaction so they cannot exhaust the connection pool
while transactions wait for another pooled connection.

A transport command has one identity across the room. Seek readiness is tied to
that identity and destination, including an attachment during the seek. A new
selection clears previous attachments and readiness. Readiness considers all
attached, connected members across API servers. A 10-second waiting deadline
excludes stragglers so one client cannot hold everyone indefinitely. It applies
only once at least one member is ready; until then the room keeps waiting,
because resuming without an audience would only skip content. Past the
deadline, the first member to become ready resumes the room. The
shared coordinator checks the persisted command time during reconciliation; it
does not retain process timers that could outlive a remotely replaced barrier.

A connection owns a unique identity. Replacing it fences reports and disconnects
from the old socket. A transient disconnect retains its session for the host's
reconnect grace period; attaching that same session only synchronizes that viewer.
The durable playback-attempt store validates sessions started on another API
server. Legacy playback sessions still use the local session manager.

A failed disconnect immediately drops the closed local socket and queues its
connection identity for reconciliation. The room stays scheduled until that
disconnect commits. A replacement connection has a new identity, so an old retry
cannot remove it. Database failure cannot keep renewing a dead socket's lease.

Presence leases last 45 seconds and are refreshed at most every 15 seconds by the
server holding the socket. An expired lease stops contributing to membership and
readiness. Host disconnect checks use shared presence and the current disconnect
time, so an old server cannot end a room whose host reconnected elsewhere. The
host retains the existing two-minute grace period. Every API also scans the
database every 15 seconds for expired host deadlines, including rooms absent
from its local cache. The scan selects at most 100 rooms and rechecks each under
its room lock before closing it. Shared rooms use the persisted deadline instead
of local host-close timers, so failed reconnects or close transactions retain the
deadline for the next sweep. Losing the last socket-owning API does not require a
disconnect notification for cleanup.

PubSub notifications queue a refresh of the authoritative row. Each active room
coalesces pending notifications so room database waits never block the subscriber.
Every server with local viewers also reconciles rooms every two seconds, repairing missed
notifications and expiring lost connections. Unchanged reconciliation does not
broadcast another snapshot. SQL work has a five-second timeout; a failed mutation
rolls back and sends no command. This coordinates Watch Party state; it does not
restart a media worker or replace the player's transport recovery behavior.

The room generation orders transport changes, not every membership or readiness
update. The web client preserves a socket snapshot when an overlapping HTTP read
or mutation receipt returns the same generation. A higher-generation response
still advances the room. This prevents delayed responses from clearing a session
attachment that the socket has already confirmed.

Each WebSocket has one writer and a queue of 64 frames. A full queue or failed
write closes that socket, allowing the client's existing reconnect flow to
recover. Other viewers' broadcasts do not wait for its write deadline. Each
broadcast builds and sorts the common roster once, then adds each viewer's own
permissions and identity flags.

## Room clock

The server holds the room's clock, the way a live stream's origin does. The
anchor (`anchor_position_seconds` at `anchor_updated_at`) moves only on an
explicit shared action: play, pause, seek, a new selection, or the end of a
barrier. A play command starts members at its `execute_at`, so the clock
starts there too rather than when the command was sent; anchoring it earlier
would put every viewer behind the room by the scheduling lead.

No member's position moves the clock, the host's included, and the server never
corrects a member's position. A viewer who falls behind, through a stall, a slow
seek, or a seek that lands on a keyframe short of its target, keeps playing from
where they are without a jump. They return to the room when they choose with
`sync_to_room`, which sends that one member a transport command at the room's
position when the command executes, or when the room's next play, pause, or seek
moves everyone. The one decision a state report still carries is the host
pausing or resuming from the system controls; the room pauses or resumes at its
own position, not the host's.

v2 snapshots carry `anchor_position_at`, the server instant that
`anchor_position_seconds` describes. While the room plays, its live position at
time `t` is `anchor_position_seconds + max(0, t − anchor_position_at)`. Clients
use it to mark the live edge and show how far behind the viewer is. The web
player marks it on the seek bar and shows LIVE within 3 seconds of it, or a GO
LIVE button with the offset beyond that.

## Buffering policy

One viewer's playback never stalls, pauses, or moves anyone else's.

- Stalls shorter than 2 seconds stay local. The web player reports buffering
  only after 2 seconds without playable media. It sends no position reports
  while its element is stalled, or while its media is unplayable and a
  readiness acknowledgement is pending, because a stalled position is not a
  decision.
- A longer stall marks the viewer buffering while the room keeps playing. The
  viewer is excluded from barriers (`self_ignore_wait`) until it acknowledges
  recovery with `ready`, which clears its status and leaves its playback where
  it recovered.
- A stall from a viewer with nobody else watching pauses the room, and the room
  waits for them past the deadline. A viewer left alone while behind resumes
  the room from wherever they recover instead of skipping ahead.
- Explicit shared actions stay coordinated: start, seek, resume, and a new
  selection. A seek waits for every attached viewer up to the 10-second
  waiting deadline; past it, the first ready viewer resumes the room and the
  rest are behind. Guests must reach the seek destination within 1 second and
  the host within 15, but the room always resumes from the destination.
- A late joiner, or a viewer whose playback session is replaced, is sent the
  room's position once on attach. The room keeps playing.
- A position report within 1 second of the room, with a matching pause state,
  marks the member ready. This covers late joiners and clients that never send
  `ready`.

Room commands still need loading on the web player. A command whose target is
already buffered seeks at once, and a small drift converges by playback rate. A
command that has to load new media, by a range request or a stream rebuild,
lands late by its load time. Only one such load runs at a time; the next waits
10 seconds after the previous one started playing, doubling to 60 seconds, and
aims ahead of the room by the load time the previous one took, up to 10
seconds. Explicit room seeks and the viewer's own GO LIVE skip that wait. After
two sustained stalls within 5 minutes, the player offers a lower quality for the
same source file.

Pausing cancels pending browser reports, and the server ignores delayed
buffering reports for a paused room. Viewer status identifies who is buffering
or syncing. The timeline remains at the requested seek position while the
player waits for the replacement stream.

## Deployment and clients

Every viewer uses the room's selected source file. Automatic selection uses the
catalogue's quality ordering within the preferred edition and presentation part:
resolution, then HDR, then file size. Watch Party adds no resolution or dynamic
range ceiling. Existing access policies still apply. Explicit file selections
retain their existing meaning.

Each viewer receives an independent playback plan for that source. A capable
client can play 4K/HDR directly while another viewer receives a transcode or
tone-mapped stream from the same file, subject to the server's transcoding and
tone-mapping settings and available executors. Clients must report their actual
decode and output capabilities; a fixed file does not require identical delivery,
resolution, bitrate, or dynamic range across viewers.

Room selection starts with the best source and responds to actual playback
refusals. A connected viewer can request `source-fallback` with the failed file,
selection revision, and refusal reason. The server selects a lower-ranked file
within the same edition and presentation part. A 4K conversion refusal skips
same-resolution files; an HDR conversion refusal prefers an SDR source without
imposing a resolution ceiling. If no candidate remains, the original refusal
stays visible.

Fallback changes the room's shared source under the same Postgres lock as normal
selection. It preserves the current anchor and resume intent, advances the
selection revision, clears every attachment and readiness report, and broadcasts
the new selection. Viewers restart against that file and join the readiness
barrier. Stale or concurrent reports return the current snapshot without another
change. Candidate order strictly decreases, preventing clients with different
capabilities from cycling between previously refused sources. This works in
host-pick and vote rooms without changing the selected content.

Before mounting the room player, the web client requires both
`watch_party_coordinator_v1` and `fixed_media_file_v1`. Missing support shows an
update message; a failed capability request can be retried without opening the
playback socket or starting a playback session. The web player disables version
switching while in a room and starts with `allow_alternate_versions: false`.
Clients discover coordinated fallback through the playback
capability `watch_party_source_fallback_v1`. That file constraint survives every replan, so decoder recovery
cannot silently move one viewer to another timeline. Streaming quality and audio
or subtitle adaptations can still use the same source.

The initial coordinator upgrade requires a stop/start rollout. Old API servers
cannot safely write alongside the new coordinator. Before enabling the new fleet:

1. Block new Watch Party HTTP requests and WebSocket upgrades at ingress, including
   reconnects. Finish existing parties or notify viewers of the interruption.
2. Stop every old API instance and wait for its process to exit. Removing it from
   load-balancer discovery alone is insufficient: existing WebSockets and
   background timers still have database write access. Keep replacement instances
   stopped until all old processes and their database connections are gone.
3. Apply the runtime-column migration and start the updated API fleet. Verify every
   API instance advertises `watch_party_coordinator_v1` through
   `GET /api/v2/playback/capabilities` before reopening ingress.
4. Reconnect viewers through the updated fleet and verify membership, seek, and
   pause across API instances. Returning to the old coordinator also requires a
   complete stop/start; never mix coordinator generations during rollback.

A single API instance follows the same stop-before-start order. A rolling update
from the old coordinator is unsupported. Later deployments between versions of
this shared coordinator can retain the normal rolling strategy.

Existing clients may omit readiness command IDs, but their seek position must
still reach the destination. Updated clients send the command ID so the server
can also reject superseded acknowledgements.

Clients discover shared membership, command-aware readiness, and member status
through `watch_party_coordinator_v1` in playback capabilities. An absent flag
means these guarantees and status fields are unsupported; an omitted false status
on a supported server means false.

Clients discover the server-held room clock through `live_clock` in
`GET /api/v2/watch-together/capabilities`. With it, the server sends no position
corrections, v2 snapshots carry `anchor_position_at`, and the v2 socket accepts
`sync_to_room`. V1 sockets share the clock behavior but keep their frozen
frames and refuse `sync_to_room`. A client that relied on corrections to stay
with the room should offer its own way back to the live position instead.

The additive member status fields are optional in v2 socket frames and HTTP v2
snapshots. Android's surface remains disabled. Jellyfin compatibility does not
use these room sockets.

The Postgres tests run independent service instances against the same room and
exercise readiness, notification loss, connection replacement, expiration, and
rollback. They do not establish deployed playback behavior; that requires testing
with actual viewers and media transports.
