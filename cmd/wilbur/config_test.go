package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	c, err := loadConfig("../../deploy/wilbur.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.peerOptions(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesExposedAnonymousStreamer(t *testing.T) {
	c := defaultConfig()
	c.Listeners.Streamer.Addr = ":8888"
	if _, err := c.validate(); err == nil || !strings.Contains(err.Error(), "streamer") {
		t.Fatalf("got %v", err)
	}
	c.AllowInsecure = true
	if w, err := c.validate(); err != nil || len(w) == 0 {
		t.Fatalf("allow_insecure: %v %v", w, err)
	}
}

func TestValidateRefusesUnsafeAdmin(t *testing.T) {
	c := defaultConfig()
	c.Auth.Anonymous = append(c.Auth.Anonymous, "admin")
	if _, err := c.validate(); err == nil {
		t.Fatal("anonymous admin accepted")
	}
	c = defaultConfig()
	c.Listeners.Player.API = true
	if _, err := c.validate(); err == nil {
		t.Fatal("API without admin authentication accepted")
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	f := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(f, []byte("auth:\n  anonymus: [player]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(f); err == nil {
		t.Fatal("misspelt key accepted")
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true, ":1": false, "0.0.0.0:1": false, "10.0.0.1:1": false} {
		if isLoopback(addr) != want {
			t.Errorf("%s", addr)
		}
	}
}
