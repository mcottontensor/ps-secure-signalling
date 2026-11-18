package sfu

import "testing"

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := LoadConfig("../../deploy/sfu.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SFUID != "SFU" || cfg.SignallingURL == "" {
		t.Fatalf("unexpected config %+v", cfg)
	}
}
