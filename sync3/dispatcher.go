package sync3

import (
	"context"
	"encoding/json"
	"os"
	"sync"

	"github.com/matrix-org/sliding-sync/internal"
	"github.com/matrix-org/sliding-sync/sync3/caches"
	"github.com/rs/zerolog"
)

var logger = zerolog.New(os.Stdout).With().Timestamp().Logger().Output(zerolog.ConsoleWriter{
	Out:        os.Stderr,
	TimeFormat: "15:04:05",
})

const DispatcherAllUsers = "-"

// Receiver represents the callbacks that a Dispatcher may fire.
type Receiver interface {
	OnNewEvent(ctx context.Context, event *caches.EventData)
	OnReceipt(ctx context.Context, receipt internal.Receipt)
	OnEphemeralEvent(ctx context.Context, roomID string, ephEvent json.RawMessage)
	// OnRegistered is called after a successful call to Dispatcher.Register
	OnRegistered(ctx context.Context) error
}

// Dispatches live events to caches
type Dispatcher struct {
	jrt              *JoinedRoomsTracker
	userToReceiver   map[string]Receiver
	userToReceiverMu *sync.RWMutex
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		jrt:              NewJoinedRoomsTracker(),
		userToReceiver:   make(map[string]Receiver),
		userToReceiverMu: &sync.RWMutex{},
	}
}

func (d *Dispatcher) IsUserJoined(userID, roomID string) bool {
	return d.jrt.IsUserJoined(userID, roomID)
}

// Load joined members into the dispatcher.
// MUST BE CALLED BEFORE V2 POLL LOOPS START.
func (d *Dispatcher) Startup(roomToJoinedUsers map[string][]string) error {
	// populate joined rooms tracker
	d.jrt.Startup(roomToJoinedUsers)
	return nil
}

func (d *Dispatcher) Unregister(userID string) {
	d.userToReceiverMu.Lock()
	defer d.userToReceiverMu.Unlock()
	delete(d.userToReceiver, userID)
}

func (d *Dispatcher) Register(ctx context.Context, userID string, r Receiver) error {
	d.userToReceiverMu.Lock()
	defer d.userToReceiverMu.Unlock()
	if _, ok := d.userToReceiver[userID]; ok {
		logger.Warn().Str("user", userID).Msg("Dispatcher.Register: receiver already registered")
	}
	d.userToReceiver[userID] = r
	return r.OnRegistered(ctx)
}

func (d *Dispatcher) ReceiverForUser(userID string) Receiver {
	d.userToReceiverMu.RLock()
	defer d.userToReceiverMu.RUnlock()
	return d.userToReceiver[userID]
}

// Called by v2 pollers when we receive an initial state block. Very similar to OnNewEvents but
// done in bulk for speed.
func (d *Dispatcher) OnNewInitialRoomState(ctx context.Context, roomID string, state []json.RawMessage) {
	// sanity check
	if _, jc := d.jrt.JoinedUsersForRoom(roomID, nil); jc > 0 {
		logger.Warn().Int("join_count", jc).Str("room", roomID).Int("num_state", len(state)).Msg(
			"OnNewInitialRoomState but have entries in JoinedRoomsTracker already, this should be impossible. Degrading to live events",
		)
		for _, s := range state {
			d.OnNewEvent(ctx, roomID, s, 0)
		}
		return
	}
	// create event datas for state
	eventDatas := make([]*caches.EventData, len(state))
	var joined, invited []string
	for i, event := range state {
		ed := caches.NewEventData(event, roomID, 0)
		eventDatas[i] = ed
		if ed.EventType == "m.room.member" && ed.StateKey != nil {
			membership := ed.Content.Get("membership").Str
			switch membership {
			case "invite":
				invited = append(invited, *ed.StateKey)
			case "join":
				joined = append(joined, *ed.StateKey)
			}
		}
	}
	// bulk update joined room tracker
	forceInitial := d.jrt.UsersJoinedRoom(joined, roomID)
	d.jrt.UsersInvitedToRoom(invited, roomID)
	inviteCount := d.jrt.NumInvitedUsersForRoom(roomID)

	// work out who to notify
	userIDs, joinCount := d.jrt.JoinedUsersForRoom(roomID, func(userID string) bool {
		if userID == DispatcherAllUsers {
			return false // safety guard to prevent dupe global callbacks
		}
		return d.ReceiverForUser(userID) != nil
	})

	// notify listeners
	for _, ed := range eventDatas {
		ed.InviteCount = inviteCount
		ed.JoinCount = joinCount
		d.notifyListeners(ctx, ed, userIDs, "", forceInitial, "")
	}
}

func (d *Dispatcher) OnNewEvent(
	ctx context.Context, roomID string, event json.RawMessage, nid int64,
) {
	ed := caches.NewEventData(event, roomID, nid)

	// update the tracker
	targetUser := ""
	membership := ""
	shouldForceInitial := false
	if ed.EventType == "m.room.member" && ed.StateKey != nil {
		targetUser = *ed.StateKey
		membership = ed.Content.Get("membership").Str
		switch membership {
		case "invite":
			// we only do this to track invite counts correctly.
			d.jrt.UsersInvitedToRoom([]string{targetUser}, ed.RoomID)
		case "join":
			if d.jrt.UserJoinedRoom(targetUser, ed.RoomID) {
				shouldForceInitial = true
			}
		case "ban":
			fallthrough
		case "leave":
			d.jrt.UserLeftRoom(targetUser, ed.RoomID)
		}
		ed.InviteCount = d.jrt.NumInvitedUsersForRoom(ed.RoomID)
	}

	// notify all people in this room
	userIDs, joinCount := d.jrt.JoinedUsersForRoom(ed.RoomID, func(userID string) bool {
		if userID == DispatcherAllUsers {
			return false // safety guard to prevent dupe global callbacks
		}
		return d.ReceiverForUser(userID) != nil
	})
	ed.JoinCount = joinCount
	d.notifyListeners(ctx, ed, userIDs, targetUser, shouldForceInitial, membership)
}

func (d *Dispatcher) OnEphemeralEvent(ctx context.Context, roomID string, ephEvent json.RawMessage) {
	notifyUserIDs, _ := d.jrt.JoinedUsersForRoom(roomID, func(userID string) bool {
		if userID == DispatcherAllUsers {
			return false // safety guard to prevent dupe global callbacks
		}
		return d.ReceiverForUser(userID) != nil
	})

	d.userToReceiverMu.RLock()
	defer d.userToReceiverMu.RUnlock()

	// global listeners (invoke before per-user listeners so caches can update)
	listener := d.userToReceiver[DispatcherAllUsers]
	if listener != nil {
		listener.OnEphemeralEvent(ctx, roomID, ephEvent)
	}

	// poke user caches OnEphemeralEvent which then pokes ConnState
	for _, userID := range notifyUserIDs {
		l := d.userToReceiver[userID]
		if l == nil {
			continue
		}
		l.OnEphemeralEvent(ctx, roomID, ephEvent)
	}
}

func (d *Dispatcher) OnReceipt(ctx context.Context, receipt internal.Receipt) {
	notifyUserIDs, _ := d.jrt.JoinedUsersForRoom(receipt.RoomID, func(userID string) bool {
		if userID == DispatcherAllUsers {
			return false // safety guard to prevent dupe global callbacks
		}
		return d.ReceiverForUser(userID) != nil
	})

	d.userToReceiverMu.RLock()
	defer d.userToReceiverMu.RUnlock()

	// global listeners (invoke before per-user listeners so caches can update)
	listener := d.userToReceiver[DispatcherAllUsers]
	if listener != nil {
		listener.OnReceipt(ctx, receipt) // FIXME: redundant, it doesn't care about receipts
	}

	// poke user caches OnReceipt which then pokes ConnState
	for _, userID := range notifyUserIDs {
		l := d.userToReceiver[userID]
		if l == nil {
			continue
		}
		l.OnReceipt(ctx, receipt)
	}
}

func (d *Dispatcher) notifyListeners(ctx context.Context, ed *caches.EventData, userIDs []string, targetUser string, shouldForceInitial bool, membership string) {
	internal.Logf(ctx, "dispatcher", "%s: notify %d users (nid=%d,join_count=%d)", ed.RoomID, len(userIDs), ed.NID, ed.JoinCount)
	// invoke listeners
	d.userToReceiverMu.RLock()
	defer d.userToReceiverMu.RUnlock()

	// global listeners (invoke before per-user listeners so caches can update)
	listener := d.userToReceiver[DispatcherAllUsers]
	if listener != nil {
		listener.OnNewEvent(ctx, ed)
	}

	// per-user listeners
	notifiedTarget := false
	for _, userID := range userIDs {
		l := d.userToReceiver[userID]
		if l != nil {
			edd := *ed
			if targetUser == userID {
				notifiedTarget = true
				if shouldForceInitial {
					edd.ForceInitial = true
				}
			}
			l.OnNewEvent(ctx, &edd)
		}
	}
	if targetUser != "" && !notifiedTarget { // e.g invites/leaves where you aren't joined yet but need to know about it
		// We expect invites to come down the invitee's poller, which triggers OnInvite code paths and
		// not normal event codepaths. We need the separate code path to ensure invite stripped state
		// is sent to the conn and not live data. Hence, if we get the invite event early from a different
		// connection, do not send it to the target, as they must wait for the invite on their poller.
		if membership != "invite" {
			if shouldForceInitial {
				ed.ForceInitial = true
			}
			l := d.userToReceiver[targetUser]
			if l != nil {
				l.OnNewEvent(ctx, ed)
			}
		}
	}
}

func (d *Dispatcher) OnInvalidateRoom(
	ctx context.Context,
	roomID string,
	joins map[string]*caches.EventData,
	invites map[string][]json.RawMessage,
	leaves map[string]json.RawMessage,
) {
	// XXX: This is all a messy hack and doesn't feel very satisfying. Other than
	//      fetching the userCaches, the dispatcher isn't doing very much for us here.

	// First dispatch to the global cache.
	receiver := d.userToReceiver[DispatcherAllUsers]
	gc := receiver.(*caches.GlobalCache)
	if gc == nil {
		logger.Error().Msgf("No receiver for global cache")
		return
	}
	gc.OnInvalidateRoom(ctx, roomID)

	// Reset the joined room tracker.
	d.jrt.ReloadMembershipsForRoom(roomID, internal.Keys(joins), internal.Keys(invites))

	// Then dispatch to any users who are joined to that room.
	// Invalidation is currently triggered by a state change or a redaction.
	//
	// Ensure we update the following fields on UserRoomData:
	//
	//  * IsInvite and HasLeave: can be altered by state changes
	//  * Invite: May need to remove this if we're no longer invited after a state change,
	//            but shouldn't alter otherwise
	//  * JoinTiming: can change if our membership has changed.
	//
	// Do this by fetching current membership and calling the appropriate callback.

	// We should, but don't currently update these fields:
	//
	//  * Spaces: can grow or shrink if m.space.parent state events have changed.
	//    Also, if this room is a space and its m.space.child state events have changed,
	//    we would  need to update other rooms' Spaces maps.

	// We ignore the following fields because invalidation cannot change them:
	//
	// * IsDM: can't be affected by either of the invalidation causes.
	// * NotificationCount and HighlightCount: might be altered by redactions... but we
	//       don't have enough info to compute counts, so ignore these.
	// * CanonicalisedName and ResolvedAvatarURL: not tracked by the UserCache, these
	//       are updated after global metadata changes in SetRoom.
	// * Tags: account data can't be changed by the invalidation triggers.

	d.userToReceiverMu.RLock()
	defer d.userToReceiverMu.RUnlock()

	// TODO: if there is a state reset, users can leave without having a leave event.
	// We would still need to mark those users as having left their rooms.
	for userID, leaveEvent := range leaves {
		receiver = d.userToReceiver[userID]
		if receiver == nil {
			continue
		}
		uc := receiver.(*caches.UserCache)
		if uc != nil {
			uc.OnLeftRoom(ctx, roomID, leaveEvent)
		}
	}

	for userID, inviteState := range invites {
		receiver = d.userToReceiver[userID]
		if receiver == nil {
			continue
		}
		uc := receiver.(*caches.UserCache)
		if uc != nil {
			uc.OnInvite(ctx, roomID, inviteState)
		}
	}

	for userID, joinData := range joins {
		receiver = d.userToReceiver[userID]
		if receiver == nil {
			continue
		}
		if receiver != nil {
			receiver.OnNewEvent(ctx, joinData)
		}
	}
}
