package turn

import (
	"encoding/json"
	"testing"
	"time"
)

func TestKnownVector(t *testing.T) {
	// The same coturn use-auth-secret vector the reference Wilbur pins, so both mint identically.
	u, c := Credentials([]byte("north"), time.Hour, "player-1", time.Unix(1_700_000_000, 0))
	if u != "1700003600:player-1" || c != "8UZc+N37gQLT4mIkDM7RaDTM+c4=" {
		t.Fatalf("got %s %s", u, c)
	}
}

func TestApply(t *testing.T) {
	in := json.RawMessage(`{"iceServers":[{"urls":["stun:s:3478"]},{"urls":"turn:t:3478?transport=udp"},{"url":"TURNS:t:5349"}],"other":1}`)
	out, err := Apply(in, []byte("secret"), time.Hour, "Player0")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		IceServers []map[string]any `json:"iceServers"`
		Other      int              `json:"other"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Other != 1 || got.IceServers[0]["username"] != nil {
		t.Fatalf("non-TURN entries changed: %s", out)
	}
	for _, s := range got.IceServers[1:] {
		if s["username"] == nil || s["credential"] == nil {
			t.Fatalf("TURN entry not credentialled: %s", out)
		}
	}
	if same, _ := Apply(json.RawMessage(`{}`), []byte("s"), time.Hour, "p"); string(same) != `{}` {
		t.Fatalf("got %s", same)
	}
}
