package signalling

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/coder/websocket"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/wsconn"
)

const defaultStreamerID = "UnknownStreamer"

// dispatch handles one message from p. Hooks run first, without the lock; routing runs under it.
//
// What each role may send is spelled out in fromStreamer, fromPlayer and fromSFU. Anything not
// listed there is dropped. Wherever a message names a player, the server checks that the player is
// subscribed to the sender, and wherever a message comes from a player, the server stamps it with
// that player's id, so no peer can speak for or to anyone it is not connected with.
func (s *Server) dispatch(p *peer, m protocol.Message) {
	info := s.info(p)
	if s.hooks.OnMessage != nil && !s.hooks.OnMessage(info, m) {
		return
	}

	switch v := m.(type) {
	case *protocol.Subscribe:
		if p.isPlayer() && !s.hookAllowsSubscribe(info, v.StreamerID) {
			p.conn.Send(&protocol.SubscribeFailed{Message: notFound(v.StreamerID)})
			return
		}
	case *protocol.EndpointID:
		if p.isStreamer() && s.hooks.AuthorizeStreamerID != nil {
			id, err := s.hooks.AuthorizeStreamerID(info, v.ID)
			if err != nil || (id != "" && !protocol.ValidStreamerID(id)) {
				s.log.Info("streamer id refused by hook", "requested", v.ID, "err", err)
				p.conn.Close(wsconn.StatusUnauthorized, "streamer id not permitted")
				return
			}
			v.ID = id
		}
	default:
		if p.role == auth.RolePlayer && s.cfg.LegacyAutoSubscribe && isNegotiation(m) {
			s.legacyAutoSubscribe(p, info)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch p.role {
	case auth.RoleStreamer:
		s.fromStreamer(p, m)
	case auth.RolePlayer:
		s.fromPlayer(p, m)
	case auth.RoleSFU:
		s.fromSFU(p, m)
	}
}

func (s *Server) fromStreamer(p *peer, m protocol.Message) {
	switch v := m.(type) {
	case *protocol.EndpointID:
		s.setStreamerIDLocked(p, v.ID)
	case *protocol.Ping:
		p.conn.Send(&protocol.Pong{Time: v.Time})
	case *protocol.Offer, *protocol.Answer, *protocol.IceCandidate:
		s.toSubscriberLocked(p, m)
	case *protocol.DisconnectPlayer:
		s.disconnectSubscriberLocked(p, v)
	case *protocol.LayerPreference:
		// A streamer may steer the layers an SFU forwards to the SFU's players.
		for _, sub := range p.subscribers {
			if sub.role == auth.RoleSFU {
				sub.conn.Send(v)
			}
		}
	default:
		s.unexpected(p, m)
	}
}

func (s *Server) fromPlayer(p *peer, m protocol.Message) {
	switch v := m.(type) {
	case *protocol.Subscribe:
		s.subscribeLocked(p, v.StreamerID)
	case *protocol.Unsubscribe:
		s.unsubscribeLocked(p)
	case *protocol.ListStreamers:
		p.conn.Send(&protocol.StreamerList{IDs: s.visibleStreamersLocked(p, true)})
	case *protocol.Ping:
		p.conn.Send(&protocol.Pong{Time: v.Time})
	case *protocol.Offer:
		// A player may offer to a streamer (the frontend's OfferToReceive mode), but never to an
		// SFU, which takes every offer as its upstream streamer's.
		if p.upstream != nil && p.upstream.role == auth.RoleStreamer {
			s.toUpstreamLocked(p, m, false)
		} else {
			s.unexpected(p, m)
		}
	case *protocol.Answer, *protocol.IceCandidate, *protocol.DataChannelRequest,
		*protocol.PeerDataChannelsReady, *protocol.LayerPreference:
		s.toUpstreamLocked(p, m, false)
	default:
		s.unexpected(p, m)
	}
}

func (s *Server) fromSFU(p *peer, m protocol.Message) {
	switch v := m.(type) {
	// As a player of its upstream streamer.
	case *protocol.Subscribe:
		s.subscribeLocked(p, v.StreamerID)
	case *protocol.Unsubscribe:
		s.unsubscribeLocked(p)
	case *protocol.ListStreamers:
		p.conn.Send(&protocol.StreamerList{IDs: s.visibleStreamersLocked(p, false)})
	case *protocol.Answer:
		s.toUpstreamLocked(p, m, false)
	case *protocol.StreamerDataChannels:
		// playerId names the SFU's own player, which the streamer only knows through the SFU.
		v.SFUID = protocol.PlayerID(p.playerID)
		s.toUpstreamLocked(p, m, true)

	// As a streamer to its own players.
	case *protocol.EndpointID:
		s.setStreamerIDLocked(p, v.ID)
	case *protocol.StartStreaming:
		p.streaming = true
	case *protocol.StopStreaming:
		p.streaming = false
		s.dropSubscribersLocked(p, "SFU stopped streaming")
	case *protocol.Offer, *protocol.PeerDataChannels:
		s.toSubscriberLocked(p, m)
	case *protocol.DisconnectPlayer:
		s.disconnectSubscriberLocked(p, v)

	// Either way, depending on whom it names.
	case *protocol.IceCandidate:
		if _, ok := p.subscribers[string(v.PlayerID)]; ok {
			s.toSubscriberLocked(p, m)
		} else {
			s.toUpstreamLocked(p, m, false)
		}
	case *protocol.Ping:
		p.conn.Send(&protocol.Pong{Time: v.Time})
	default:
		s.unexpected(p, m)
	}
}

func (s *Server) unexpected(p *peer, m protocol.Message) {
	s.log.Debug("dropped message", "role", p.role, "player", p.playerID, "streamer", p.streamerID, "type", protocol.TypeOf(m))
}

// toUpstreamLocked forwards a message from a player (or SFU) to the streamer it is subscribed to,
// stamped with the sender's player id unless keepPlayerID.
func (s *Server) toUpstreamLocked(p *peer, m protocol.Message, keepPlayerID bool) {
	if p.upstream == nil {
		s.log.Debug("message from unsubscribed peer dropped", "player", p.playerID, "type", protocol.TypeOf(m))
		return
	}
	if !keepPlayerID {
		protocol.SetPlayerID(m, protocol.PlayerID(p.playerID))
	}
	p.upstream.conn.Send(m)
}

// toSubscriberLocked forwards a message from a streamer (or SFU) to the subscriber it names.
func (s *Server) toSubscriberLocked(p *peer, m protocol.Message) {
	pid, _ := protocol.PlayerIDOf(m)
	sub, ok := p.subscribers[string(pid)]
	if !ok {
		s.log.Debug("message to non-subscriber dropped", "streamer", p.streamerID, "player", string(pid), "type", protocol.TypeOf(m))
		return
	}
	// The recipient knows who it is; the id is only routing information.
	protocol.SetPlayerID(m, "")
	sub.conn.Send(m)
}

func (s *Server) disconnectSubscriberLocked(p *peer, m *protocol.DisconnectPlayer) {
	if sub, ok := p.subscribers[string(m.PlayerID)]; ok {
		s.log.Info("streamer disconnected player", "streamer", p.streamerID, "player", sub.playerID)
		sub.conn.Close(websocket.StatusInternalError, m.Reason)
	}
}

func (s *Server) hookAllowsSubscribe(info PeerInfo, streamerID string) bool {
	if s.hooks.AuthorizeSubscribe == nil {
		return true
	}
	if err := s.hooks.AuthorizeSubscribe(info, streamerID); err != nil {
		s.log.Info("subscribe refused by hook", "player", info.PlayerID, "streamer", streamerID, "err", err)
		return false
	}
	return true
}

func (s *Server) subscribeLocked(p *peer, streamerID string) {
	target := s.streamers[streamerID]
	// Not existing and not being allowed to see it get the same answer, so that the reply does not
	// reveal which streamers exist.
	if target == nil || target == p || !p.identity.MaySubscribe(streamerID) {
		p.conn.Send(&protocol.SubscribeFailed{Message: notFound(streamerID)})
		return
	}
	if p.upstream != nil {
		s.unsubscribeLocked(p)
	}
	// SFUs are trusted infrastructure and do not take a player slot, as in Wilbur.
	if p.role == auth.RolePlayer && s.cfg.MaxSubscribers > 0 && s.countRole(target.subscribers, auth.RolePlayer) >= s.cfg.MaxSubscribers {
		p.conn.Send(&protocol.SubscribeFailed{Message: fmt.Sprintf("Streamer %s is full. Max players = %d.", streamerID, s.cfg.MaxSubscribers)})
		return
	}
	p.upstream = target
	target.subscribers[p.playerID] = p
	target.conn.Send(&protocol.PlayerConnected{
		PlayerID:    protocol.PlayerID(p.playerID),
		DataChannel: true,
		SFU:         p.role == auth.RoleSFU,
	})
	s.log.Info("subscribed", "player", p.playerID, "streamer", streamerID)
}

func (s *Server) unsubscribeLocked(p *peer) {
	up := p.upstream
	if up == nil {
		return
	}
	delete(up.subscribers, p.playerID)
	p.upstream = nil
	up.conn.Send(&protocol.PlayerDisconnected{PlayerID: protocol.PlayerID(p.playerID)})
	s.log.Info("unsubscribed", "player", p.playerID, "streamer", up.streamerID)
}

// dropSubscribersLocked detaches everyone subscribed to p. Players are disconnected, which is how
// the frontend learns to reconnect; SFUs are told, and go looking for another streamer.
func (s *Server) dropSubscribersLocked(p *peer, reason string) {
	for id, sub := range p.subscribers {
		delete(p.subscribers, id)
		sub.upstream = nil
		if sub.role == auth.RoleSFU {
			sub.conn.Send(&protocol.StreamerDisconnected{})
		} else {
			sub.conn.Close(websocket.StatusNormalClosure, reason)
		}
	}
}

// visibleStreamersLocked lists the streamers p may subscribe to. Players only see streamers that
// are ready; SFUs see all of them, as in Wilbur.
func (s *Server) visibleStreamersLocked(p *peer, onlyStreaming bool) []string {
	ids := []string{}
	for id, st := range s.streamers {
		if st != p && (st.streaming || !onlyStreaming) && p.identity.MaySubscribe(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// setStreamerIDLocked commits the id a streamer asked for in endpointId.
//
// An identity with reserved ids (auth.Identity.PublishAs) may only use those, always gets exactly
// the id it asked for, and takes it over from whichever connection holds it. Anyone else gets the
// legacy behaviour: the id it asked for, with a number appended if it is taken.
func (s *Server) setStreamerIDLocked(p *peer, requested string) {
	id := p.identity
	if requested == "" && id.Pinned() && len(id.PublishAs) == 1 {
		requested = id.PublishAs[0]
	}
	if requested == "" {
		requested = defaultStreamerID
	}

	final := requested
	if _, reserved := s.reserved[requested]; reserved && !id.Pinned() {
		s.log.Warn("reserved streamer id refused", "subject", id.Subject, "requested", requested)
		p.conn.Close(wsconn.StatusUnauthorized, "streamer id not permitted")
		return
	}
	if id.Pinned() {
		if !id.MayPublishAs(requested) {
			s.log.Info("streamer id not permitted", "subject", id.Subject, "requested", requested)
			p.conn.Close(wsconn.StatusUnauthorized, "streamer id not permitted")
			return
		}
		if holder := s.streamers[requested]; holder != nil && holder != p {
			s.log.Warn("streamer id taken over", "streamer", requested, "subject", id.Subject, "previous", holder.identity.Subject)
			delete(s.streamers, requested)
			s.dropSubscribersLocked(holder, "streamer replaced")
			holder.conn.Close(wsconn.StatusReplaced, "replaced by a new connection")
		}
		s.reserved[requested] = struct{}{}
	} else if requested != p.streamerID {
		final = s.uniqueStreamerID(requested)
	}

	if final != p.streamerID {
		if s.streamers[p.streamerID] == p {
			delete(s.streamers, p.streamerID)
		}
		s.streamers[final] = p
		s.log.Info("streamer identified", "streamer", final, "was", p.streamerID)
		p.streamerID = final
		for pid, sub := range p.subscribers {
			// A rename must not carry subscribers somewhere their scope does not reach.
			if !sub.identity.MaySubscribe(final) {
				delete(p.subscribers, pid)
				sub.upstream = nil
				sub.conn.Close(websocket.StatusNormalClosure, "streamer renamed")
				continue
			}
			sub.conn.Send(&protocol.StreamerIDChanged{NewID: final})
		}
	}
	p.streaming = true
	p.conn.Send(&protocol.EndpointIDConfirm{CommittedID: final})
}

// uniqueStreamerID returns base, or base with the lowest number appended that makes it unique.
func (s *Server) uniqueStreamerID(base string) string {
	if len(base) > protocol.MaxIDBytes-8 {
		base = base[:protocol.MaxIDBytes-8]
	}
	if _, taken := s.streamers[base]; !taken {
		return base
	}
	for n := 1; ; n++ {
		if id := base + strconv.Itoa(n); s.streamers[id] == nil {
			return id
		}
	}
}

// legacyAutoSubscribe subscribes a player that skipped subscribe to the first streamer it may see.
func (s *Server) legacyAutoSubscribe(p *peer, info PeerInfo) {
	s.mu.Lock()
	var target string
	if p.upstream == nil {
		if ids := s.visibleStreamersLocked(p, true); len(ids) > 0 {
			target = ids[0]
		}
	}
	subscribed := p.upstream != nil
	s.mu.Unlock()
	if subscribed || target == "" || !s.hookAllowsSubscribe(info, target) {
		return
	}
	s.mu.Lock()
	if p.upstream == nil {
		s.subscribeLocked(p, target)
	}
	s.mu.Unlock()
}

func isNegotiation(m protocol.Message) bool {
	switch m.(type) {
	case *protocol.Offer, *protocol.Answer, *protocol.IceCandidate, *protocol.DataChannelRequest,
		*protocol.PeerDataChannelsReady, *protocol.LayerPreference:
		return true
	}
	return false
}

func notFound(streamerID string) string {
	if len(streamerID) > protocol.MaxIDBytes {
		streamerID = streamerID[:protocol.MaxIDBytes]
	}
	return fmt.Sprintf("Streamer %s does not exist.", streamerID)
}
