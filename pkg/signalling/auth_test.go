package signalling_test

import (
	"net/http"
	"testing"

	"github.com/coder/websocket"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/auth"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/protocol"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/signalling"
	"github.com/epicgames-ps/ps-secure-signalling/pkg/wsconn"
)

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

const (
	ue1Token     = "ue1-0123456789abcdef"
	ue2Token     = "ue2-0123456789abcdef"
	openToken    = "open-0123456789abcdef"
	scopedPlayer = "player-0123456789abcd"
)

func tokenHarness(t *testing.T) *harness {
	st, err := auth.NewStaticTokens(auth.DefaultTokenSource, []auth.StaticToken{
		{Token: ue1Token, Subject: "ue1", Roles: []auth.Role{auth.RoleStreamer}, PublishAs: []string{"Game"}},
		{Token: ue2Token, Subject: "ue2", Roles: []auth.Role{auth.RoleStreamer}, PublishAs: []string{"Other"}},
		{Token: openToken, Subject: "open", Roles: []auth.Role{auth.RoleStreamer}},
		{Token: scopedPlayer, Subject: "alice", Roles: []auth.Role{auth.RolePlayer}, Streamers: []string{"Game"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return newHarness(t, signalling.Config{}, &auth.Chain{Authenticators: []auth.Authenticator{st}, Anonymous: []auth.Role{auth.RolePlayer}}, signalling.Hooks{})
}

func TestUnauthenticatedStreamerRefused(t *testing.T) {
	h := tokenHarness(t)
	for _, hdr := range []http.Header{nil, bearer("wrong-0123456789abcdef"), bearer(scopedPlayer)} {
		_, resp, err := dial(t, h.streamer, hdr)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("header %v: err %v resp %v", hdr, err, resp)
		}
	}
	// The query parameter works for clients that cannot set headers.
	h.streamer.URL += "/?token=" + openToken
	mustDial(t, h.streamer, nil).expect(protocol.TypeIdentify)
}

func TestWrongTokenRefusedEvenWhereAnonymousAllowed(t *testing.T) {
	h := tokenHarness(t)
	if _, resp, err := dial(t, h.player, bearer("wrong-0123456789abcdef")); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err %v", err)
	}
}

func TestPinnedStreamerID(t *testing.T) {
	h := tokenHarness(t)

	// An unpinned streamer squats on the id first.
	squatter, id := h.connectStreamer("Game", bearer(openToken))
	if id != "Game" {
		t.Fatalf("id %q", id)
	}
	// The identity that owns the id takes it over, unsuffixed.
	owner, id := h.connectStreamer("Game", bearer(ue1Token))
	if id != "Game" {
		t.Fatalf("owner got %q", id)
	}
	if code := squatter.expectClosed(); code != wsconn.StatusReplaced {
		t.Fatalf("squatter close %v", code)
	}
	// A pinned identity may not publish as anything else.
	s := mustDial(t, h.streamer, bearer(ue2Token))
	s.expect(protocol.TypeIdentify)
	s.send(&protocol.EndpointID{ID: "Game"})
	if code := s.expectClosed(); code != wsconn.StatusUnauthorized {
		t.Fatalf("close %v", code)
	}
	// An empty request gets the identity's only id.
	_, id = h.connectStreamer("", bearer(ue2Token))
	if id != "Other" {
		t.Fatalf("id %q", id)
	}
	_ = owner
}

func TestScopedPlayer(t *testing.T) {
	h := tokenHarness(t)
	game, _ := h.connectStreamer("Game", bearer(ue1Token))
	h.connectStreamer("Other", bearer(ue2Token))

	p := mustDial(t, h.player, bearer(scopedPlayer))
	p.expect(protocol.TypeConfig)
	p.send(&protocol.ListStreamers{})
	ids := p.expect(protocol.TypeStreamerList)["ids"].([]any)
	if len(ids) != 1 || ids[0] != "Game" {
		t.Fatalf("scoped list %v", ids)
	}
	// Out of scope looks exactly like not existing.
	p.send(&protocol.Subscribe{StreamerID: "Other"})
	if m := p.expect(protocol.TypeSubscribeFailed); m["message"] != "Streamer Other does not exist." {
		t.Fatalf("got %v", m)
	}
	p.send(&protocol.Subscribe{StreamerID: "Game"})
	game.expect(protocol.TypePlayerConnected)

	// Anonymous players see everything.
	anon := mustDial(t, h.player, nil)
	anon.expect(protocol.TypeConfig)
	anon.send(&protocol.ListStreamers{})
	if ids := anon.expect(protocol.TypeStreamerList)["ids"].([]any); len(ids) != 2 {
		t.Fatalf("anonymous list %v", ids)
	}
	anon.ws.Close(websocket.StatusNormalClosure, "")
}
