package watchtogether

import (
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// bufferingRoom is a playing room with a host and guests, each attached to a
// playback session, driven by a clock the test advances.
type bufferingRoom struct {
	t     *testing.T
	s     *Service
	repo  *stubRepo
	now   time.Time
	conns map[string]*recordingConn
}

func newBufferingRoom(t *testing.T, guests ...string) *bufferingRoom {
	t.Helper()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	room := baseRoom(now)
	room.AnchorUpdatedAt = now
	room.AnchorPositionSeconds = 100
	f := &bufferingRoom{t: t, repo: &stubRepo{room: room}, now: now, conns: make(map[string]*recordingConn)}
	f.s = newServiceForTest(now, f.repo, &stubSessions{}, &stubFiles{}, nil)
	f.s.now = func() time.Time { return f.now }
	t.Cleanup(f.s.Close)
	live := f.s.rooms[room.ID]
	add := func(userID int, profileID string) {
		conn := new(recordingConn)
		f.conns[profileID] = conn
		live.members[buildMemberKey(userID, profileID)] = &memberState{
			userID: userID, profileID: profileID, sessionID: profileID + "-session", connection: conn, isReady: true,
		}
	}
	add(7, "host")
	for i, guest := range guests {
		add(8+i, guest)
	}
	return f
}

func (f *bufferingRoom) userID(profileID string) int {
	return f.member(profileID).userID
}

func (f *bufferingRoom) member(profileID string) *memberState {
	for _, member := range f.s.rooms[f.repo.room.ID].members {
		if member.profileID == profileID {
			return member
		}
	}
	f.t.Fatalf("no member %q", profileID)
	return nil
}

func (f *bufferingRoom) reg(profileID string) *Registration {
	return registrationFor(f.repo.room.ID, f.userID(profileID), profileID, f.conns[profileID])
}

func (f *bufferingRoom) expected() float64 {
	return expectedPosition(f.repo.room, f.now)
}

func (f *bufferingRoom) buffer(profileID string) Snapshot {
	f.t.Helper()
	snapshot, err := f.s.HandleBufferingForConnection(f.t.Context(), f.reg(profileID), f.userID(profileID), profileID, StateReport{
		SessionID: profileID + "-session", PositionSeconds: f.expected(),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return snapshot
}

func (f *bufferingRoom) ready(profileID string, position float64) Snapshot {
	f.t.Helper()
	snapshot, err := f.s.HandleReadyForConnection(f.t.Context(), f.reg(profileID), f.userID(profileID), profileID, StateReport{
		SessionID: profileID + "-session", PositionSeconds: position,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return snapshot
}

func (f *bufferingRoom) report(profileID string, position float64) Snapshot {
	f.t.Helper()
	snapshot, err := f.s.HandleStateReportForConnection(f.t.Context(), f.reg(profileID), f.userID(profileID), profileID, StateReport{
		SessionID: profileID + "-session", PositionSeconds: position,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return snapshot
}

// resumeAll acknowledges the current barrier for every member.
func (f *bufferingRoom) resumeAll() {
	f.t.Helper()
	position := f.repo.room.AnchorPositionSeconds
	for _, member := range f.s.rooms[f.repo.room.ID].members {
		f.ready(member.profileID, position)
	}
	if f.repo.room.PlaybackState != RoomPlaybackStatePlaying {
		f.t.Fatalf("room did not resume: %s", f.repo.room.PlaybackState)
	}
}

// seek has the host seek the room, which waits for every viewer.
func (f *bufferingRoom) seek(position float64) {
	f.t.Helper()
	if _, err := f.s.HandleTransportRequestForConnection(f.t.Context(), f.reg("host"), 7, "host", TransportRequest{Action: TransportActionSeek, PositionSeconds: &position}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *bufferingRoom) missDeadline() {
	f.t.Helper()
	f.now = f.now.Add(waitingResumeDeadline)
	f.s.handleWaitingDeadline(f.repo.room.ID, f.s.rooms[f.repo.room.ID].waitingEpoch)
	if f.repo.room.PlaybackState != RoomPlaybackStatePlaying {
		f.t.Fatalf("deadline did not resume the room: %s", f.repo.room.PlaybackState)
	}
}

func (f *bufferingRoom) transports(profileID string) int {
	return transportCount(f.conns[profileID])
}

func memberSummary(t *testing.T, snapshot Snapshot, profileID string) MemberSummary {
	t.Helper()
	for _, member := range snapshot.Members {
		if member.ProfileID == profileID {
			return member
		}
	}
	t.Fatalf("snapshot has no member %q", profileID)
	return MemberSummary{}
}

func TestStallNeverPausesARoomOthersAreWatching(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	for range 3 {
		generation := f.repo.room.Generation
		snapshot := f.buffer("guest")
		if snapshot.PlaybackState != RoomPlaybackStatePlaying || f.repo.room.Generation != generation {
			t.Fatalf("a guest's stall paused the room: %+v", snapshot)
		}
		if guest := memberSummary(t, snapshot, "guest"); !guest.IsBuffering {
			t.Fatalf("stalled guest status = %+v", guest)
		}
		if !f.member("guest").ignoreWait {
			t.Fatal("stalled guest is not excluded from barriers")
		}
		if got := f.transports("host"); got != 0 {
			t.Fatalf("a guest's stall sent the host %d commands", got)
		}
		f.ready("guest", f.expected()-5)
		f.now = f.now.Add(10 * time.Second)
	}
}

// A viewer who recovers from a stall keeps playing from where they are. The
// room does not jump them to its position; they choose that with
// sync_to_room.
func TestRecoveredViewerKeepsTheirPosition(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.buffer("guest")
	f.now = f.now.Add(20 * time.Second)
	snapshot := f.ready("guest", f.expected()-8)
	if guest := memberSummary(t, snapshot, "guest"); guest.IsBuffering || !guest.IsReady || snapshot.SelfIgnoreWait {
		t.Fatalf("recovered guest = %+v ignore=%v", guest, snapshot.SelfIgnoreWait)
	}
	if got := f.transports("guest"); got != 0 {
		t.Fatalf("recovery sent the guest %d commands, want none", got)
	}
}

// The room's clock is the server's. However far a member drifts, the server
// neither corrects it nor follows it.
func TestFallingBehindIsNeverCorrected(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	anchor, anchorAt := f.repo.room.AnchorPositionSeconds, f.repo.room.AnchorUpdatedAt
	for _, behind := range []float64{1.5, 4, 30, 30, 120} {
		f.now = f.now.Add(time.Minute)
		f.report("guest", f.expected()-behind)
		f.report("host", f.expected()-behind)
	}
	f.report("host", f.expected()+45)
	if got := f.transports("guest") + f.transports("host"); got != 0 {
		t.Fatalf("drift sent %d corrections, want none", got)
	}
	if f.repo.room.AnchorPositionSeconds != anchor || !f.repo.room.AnchorUpdatedAt.Equal(anchorAt) {
		t.Fatalf("a member's position moved the room clock to %v at %v",
			f.repo.room.AnchorPositionSeconds, f.repo.room.AnchorUpdatedAt)
	}
}

// A host pausing from the system controls pauses the room where the room
// is, not where the host's player happens to be.
func TestHostSystemPauseUsesTheRoomClock(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.now = f.now.Add(30 * time.Second)
	live := f.expected()
	if _, err := f.s.HandleStateReportForConnection(t.Context(), f.reg("host"), 7, "host", StateReport{
		SessionID: "host-session", PositionSeconds: live - 6, IsPaused: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !f.repo.room.IsPaused || f.repo.room.AnchorPositionSeconds != live {
		t.Fatalf("host pause left the room paused=%v at %v, want paused at %v",
			f.repo.room.IsPaused, f.repo.room.AnchorPositionSeconds, live)
	}
	for _, profile := range []string{"host", "guest"} {
		if command := lastTransport(t, f.conns[profile]); command.Action != TransportActionPause || command.PositionSeconds != live {
			t.Fatalf("%s received %+v, want pause at %v", profile, command, live)
		}
	}

	// A guest's paused player is not a decision.
	f.now = f.now.Add(time.Second)
	if _, err := f.s.HandleTransportRequestForConnection(t.Context(), f.reg("host"), 7, "host", TransportRequest{Action: TransportActionPlay}); err != nil {
		t.Fatal(err)
	}
	before := f.transports("host")
	if _, err := f.s.HandleStateReportForConnection(t.Context(), f.reg("guest"), 8, "guest", StateReport{
		SessionID: "guest-session", PositionSeconds: live, IsPaused: true,
	}); err != nil {
		t.Fatal(err)
	}
	if f.repo.room.IsPaused || f.transports("host") != before {
		t.Fatal("a guest's paused player paused the room")
	}
}

// Play commands start everyone at execute_at, so the room's clock starts
// there too and members are not behind it from the first frame.
func TestPlayStartsTheRoomClockAtExecution(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	position := 300.0
	if _, err := f.s.HandleTransportRequestForConnection(t.Context(), f.reg("host"), 7, "host", TransportRequest{Action: TransportActionPause, PositionSeconds: &position}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.HandleTransportRequestForConnection(t.Context(), f.reg("host"), 7, "host", TransportRequest{Action: TransportActionPlay}); err != nil {
		t.Fatal(err)
	}
	command := lastTransport(t, f.conns["guest"])
	executeAt, err := time.Parse(time.RFC3339Nano, command.ExecuteAt)
	if err != nil {
		t.Fatal(err)
	}
	if !executeAt.After(f.now) || !f.repo.room.AnchorUpdatedAt.Equal(executeAt) || command.PositionSeconds != position {
		t.Fatalf("play at %v from %v: anchor %v at %v", executeAt, f.now, f.repo.room.AnchorPositionSeconds, f.repo.room.AnchorUpdatedAt)
	}
	if got := expectedPosition(f.repo.room, executeAt.Add(2*time.Second)); got != position+2 {
		t.Fatalf("room position 2s after execution = %v, want %v", got, position+2)
	}
}

func TestSyncToRoomSendsOnlyThatMemberTheLivePosition(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.now = f.now.Add(time.Minute)
	f.report("guest", f.expected()-20)
	anchor, anchorAt := f.repo.room.AnchorPositionSeconds, f.repo.room.AnchorUpdatedAt

	if _, err := f.s.HandleSyncToRoomForConnection(t.Context(), f.reg("guest"), 8, "guest", "guest-session"); err != nil {
		t.Fatal(err)
	}
	command := lastTransport(t, f.conns["guest"])
	executeAt, err := time.Parse(time.RFC3339Nano, command.ExecuteAt)
	if err != nil {
		t.Fatal(err)
	}
	if command.Action != TransportActionPlay || command.SessionID != "guest-session" ||
		command.PositionSeconds != expectedPosition(f.repo.room, executeAt) {
		t.Fatalf("sync = %+v, want play at the room's position at %v", command, executeAt)
	}
	if got := f.transports("host"); got != 0 {
		t.Fatalf("a guest's sync sent the host %d commands", got)
	}
	if f.repo.room.AnchorPositionSeconds != anchor || !f.repo.room.AnchorUpdatedAt.Equal(anchorAt) {
		t.Fatal("a member's sync moved the room clock")
	}

	if _, err := f.s.HandleSyncToRoomForConnection(t.Context(), f.reg("guest"), 8, "guest", "other-session"); err == nil {
		t.Fatal("sync for a session the member has not attached was accepted")
	}
}

// Until a syncing host reaches the room, its reports describe the stream it
// is replacing; a paused frame from it must not pause the room.
func TestSyncingHostDoesNotPauseTheRoom(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.now = f.now.Add(time.Minute)
	if _, err := f.s.HandleSyncToRoomForConnection(t.Context(), f.reg("host"), 7, "host", "host-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.HandleStateReportForConnection(t.Context(), f.reg("host"), 7, "host", StateReport{
		SessionID: "host-session", PositionSeconds: f.expected() - 30, IsPaused: true,
	}); err != nil {
		t.Fatal(err)
	}
	if f.repo.room.IsPaused {
		t.Fatal("a syncing host's stale report paused the room")
	}
	f.report("host", f.expected())
	if f.member("host").syncingToRoom {
		t.Fatal("reaching the room did not end the host's sync")
	}
}

func TestSyncToRoomWhileWaitingSendsTheBarrier(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	position := 500.0
	if _, err := f.s.HandleTransportRequestForConnection(t.Context(), f.reg("host"), 7, "host", TransportRequest{Action: TransportActionSeek, PositionSeconds: &position}); err != nil {
		t.Fatal(err)
	}
	barrier := lastTransport(t, f.conns["guest"])
	if _, err := f.s.HandleSyncToRoomForConnection(t.Context(), f.reg("guest"), 8, "guest", "guest-session"); err != nil {
		t.Fatal(err)
	}
	if command := lastTransport(t, f.conns["guest"]); command.CommandID != barrier.CommandID {
		t.Fatalf("sync while waiting = %+v, want the barrier %s", command, barrier.CommandID)
	}
}

func TestViewerSkippedAtDeadlineNeverPausesTheRoomAgain(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	position := 200.0
	if _, err := f.s.HandleTransportRequestForConnection(t.Context(), f.reg("host"), 7, "host", TransportRequest{Action: TransportActionSeek, PositionSeconds: &position}); err != nil {
		t.Fatal(err)
	}
	f.ready("host", position)
	f.missDeadline()
	if guest := f.member("guest"); !guest.ignoreWait {
		t.Fatalf("skipped guest = %+v", guest)
	}

	f.now = f.now.Add(time.Minute)
	generation := f.repo.room.Generation
	if snapshot := f.buffer("guest"); snapshot.PlaybackState != RoomPlaybackStatePlaying || f.repo.room.Generation != generation {
		t.Fatalf("skipped guest paused the room: %+v", snapshot)
	}
}

func TestLoneViewerStallAlwaysPausesTheRoom(t *testing.T) {
	f := newBufferingRoom(t)
	f.member("host").ignoreWait = true
	snapshot := f.buffer("host")
	if snapshot.PlaybackState != RoomPlaybackStateWaiting {
		t.Fatalf("lone viewer's stall let the room run on: %+v", snapshot)
	}
	if f.member("host").ignoreWait {
		t.Fatal("lone viewer's stall excluded them from the barrier")
	}
	if command := lastTransport(t, f.conns["host"]); command.Action != TransportActionPause {
		t.Fatalf("lone viewer was not paused: %+v", command)
	}
	if snapshot := f.ready("host", f.repo.room.AnchorPositionSeconds); snapshot.PlaybackState != RoomPlaybackStatePlaying {
		t.Fatalf("lone viewer's recovery did not resume the room: %+v", snapshot)
	}
}

// Clients that never acknowledge recovery still report positions; one that
// matches the room clears the member's buffering and behind flags.
func TestInSyncReportClearsAStaleBufferingStatus(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.buffer("guest")

	f.now = f.now.Add(5 * time.Second)
	f.report("guest", f.expected()-3)
	if guest := f.member("guest"); !guest.isBuffering || !guest.ignoreWait {
		t.Fatalf("out-of-sync report cleared the stall: %+v", guest)
	}

	hostFrames := len(f.conns["host"].payloads)
	snapshot := f.report("guest", f.expected()-0.5)
	guest := memberSummary(t, snapshot, "guest")
	if guest.IsBuffering || !guest.IsReady || snapshot.SelfIgnoreWait {
		t.Fatalf("in-sync report left the guest flagged: %+v ignore=%v", guest, snapshot.SelfIgnoreWait)
	}
	if len(f.conns["host"].payloads) == hostFrames {
		t.Fatal("other viewers did not receive the recovered status")
	}
}

// With nobody ready, running the room would only skip content. A lone viewer
// is waited for past the deadline, and reconnecting finds the room where they
// stalled.
func TestLoneViewerIsWaitedForPastTheDeadline(t *testing.T) {
	f := newBufferingRoom(t)
	f.s.sessions = &stubSessions{session: &playback.Session{UserID: 7, ProfileID: "host", MediaFileID: 1}}
	f.s.files = &stubFiles{file: &models.MediaFile{ContentID: "movie-1"}}
	f.buffer("host")
	f.now = f.now.Add(waitingResumeDeadline + time.Second)
	f.s.handleWaitingDeadline(f.repo.room.ID, f.s.rooms[f.repo.room.ID].waitingEpoch)
	if f.repo.room.PlaybackState != RoomPlaybackStateWaiting || f.member("host").ignoreWait {
		t.Fatalf("deadline ran the room without its only viewer: state=%s", f.repo.room.PlaybackState)
	}

	f.s.Disconnect(f.reg("host"), false)
	conn := new(recordingConn)
	reg, _, err := f.s.Connect(t.Context(), f.repo.room.ID, 7, "host", conn)
	if err != nil {
		t.Fatal(err)
	}
	f.conns["host"] = conn
	if _, err = f.s.AttachSessionForConnection(t.Context(), reg, 7, "host", "host-session"); err != nil {
		t.Fatal(err)
	}
	if command := lastTransport(t, conn); command.PlaybackState != RoomPlaybackStateWaiting || command.PositionSeconds != 100 {
		t.Fatalf("reconnected lone viewer target = %+v, want the waiting anchor 100", command)
	}
	f.now = f.now.Add(20 * time.Second)
	if snapshot := f.ready("host", 100); snapshot.PlaybackState != RoomPlaybackStatePlaying || f.repo.room.AnchorPositionSeconds != 100 {
		t.Fatalf("lone viewer's recovery moved the room: %+v", snapshot)
	}
}

// A viewer who fell behind while someone else watched can end up alone.
// Recovering then moves the room to where they are instead of leaving them
// behind an empty room.
func TestViewerLeftAloneRecoversFromTheirPosition(t *testing.T) {
	f := newBufferingRoom(t, "guest")
	f.buffer("host")
	if !f.member("host").ignoreWait {
		t.Fatal("the stalled viewer is not behind the room")
	}
	f.s.Disconnect(f.reg("guest"), true)
	f.now = f.now.Add(20 * time.Second)
	f.ready("host", 100)
	if f.repo.room.AnchorPositionSeconds != 100 || f.repo.room.PlaybackState != RoomPlaybackStatePlaying {
		t.Fatalf("recovery jumped the room: anchor=%v state=%s", f.repo.room.AnchorPositionSeconds, f.repo.room.PlaybackState)
	}
	if got := f.transports("host"); got != 0 {
		t.Fatalf("recovery sent %d commands to a viewer already at the room's position", got)
	}
}

// Past the deadline, the first viewer to become ready resumes the room.
func TestFirstReadyViewerResumesARoomPastItsDeadline(t *testing.T) {
	f := newBufferingRoom(t, "ann", "bob")
	f.seek(100)
	f.now = f.now.Add(waitingResumeDeadline + time.Second)
	f.s.handleWaitingDeadline(f.repo.room.ID, f.s.rooms[f.repo.room.ID].waitingEpoch)
	if f.repo.room.PlaybackState != RoomPlaybackStateWaiting {
		t.Fatal("room resumed with nobody ready")
	}
	if snapshot := f.ready("bob", f.repo.room.AnchorPositionSeconds); snapshot.PlaybackState != RoomPlaybackStatePlaying {
		t.Fatalf("first ready viewer did not resume the room: %+v", snapshot)
	}
	if !f.member("ann").ignoreWait || !f.member("host").ignoreWait {
		t.Fatal("unready viewers were not skipped")
	}
}
