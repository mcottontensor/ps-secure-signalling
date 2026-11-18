// Package sfu is a Selective Forwarding Unit for Pixel Streaming. It subscribes to one streamer
// through the signalling server like a player, and is itself subscribed to by players like a
// streamer, forwarding the streamer's media and data channel traffic to each of them.
//
// It replaces the mediasoup based SFU/sfu_server.js and speaks the same signalling protocol, so it
// works with UE Pixel Streaming (PS1 and PS2), the stock frontend and either signalling server.
package sfu

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/config"
)

// Config is the SFU configuration. LoadConfig fills defaults for anything left unset.
type Config struct {
	SignallingURL       string        `yaml:"signalling_url"`
	SFUID               string        `yaml:"sfu_id"`
	SubscribeStreamerID string        `yaml:"subscribe_streamer_id"` // empty subscribes to the first streamer listed
	RetrySubscribeDelay time.Duration `yaml:"retry_subscribe_delay"`
	LogLevel            string        `yaml:"log_level"` // debug, info, warn, error

	// TokenFile holds a bearer token sent to the signalling server in an Authorization header.
	TokenFile string `yaml:"token_file"`
	// Token is the token itself, for embedders. Never read from the config file, so it cannot end
	// up committed alongside it.
	Token string `yaml:"-"`

	TLS config.ClientTLS `yaml:"tls"`
	ICE ICEConfig        `yaml:"ice"`
	BWE BWEConfig        `yaml:"bwe"`

	MaxPlayers int `yaml:"max_players"` // 0 means unlimited
	// Codecs offered to the streamer, by name: vp8, vp9, h264, av1, opus.
	Codecs []string `yaml:"codecs"`
	// MaxDataChannelMessage is the largest data channel message relayed, in bytes.
	MaxDataChannelMessage int `yaml:"max_data_channel_message"`
	// GatherTimeout bounds how long ICE gathering may take before an SDP is sent with the
	// candidates found so far. Candidates go in the SDP because older signalling servers do not
	// route ICE candidates from an SFU.
	GatherTimeout time.Duration `yaml:"gather_timeout"`
}

// ICEConfig controls which addresses and ports media uses.
type ICEConfig struct {
	// UDPPort, if set, carries all media on one UDP port, which is far easier to firewall.
	UDPPort int `yaml:"udp_port"`
	// PortMin and PortMax bound the ephemeral ports used when UDPPort is not set.
	PortMin uint16 `yaml:"port_min"`
	PortMax uint16 `yaml:"port_max"`
	// PublicIPs are advertised in place of the host's own addresses (1:1 NAT, cloud VMs).
	PublicIPs []string `yaml:"public_ips"`
	// Interfaces restricts candidates to these network interfaces. Empty allows all.
	Interfaces []string `yaml:"interfaces"`
	// IncludeLoopback gathers 127.0.0.1 candidates, for single-machine setups.
	IncludeLoopback bool `yaml:"include_loopback"`
	// TCP enables ICE over TCP. Off by default.
	TCP bool `yaml:"tcp"`
}

// BWEConfig controls automatic layer selection from each player's bandwidth estimate.
type BWEConfig struct {
	Disabled       bool `yaml:"disabled"`
	InitialBitrate int  `yaml:"initial_bitrate"` // bits per second
	MinBitrate     int  `yaml:"min_bitrate"`
	MaxBitrate     int  `yaml:"max_bitrate"`
}

// DefaultConfig returns the defaults, which match the old SFU's config.js.
func DefaultConfig() Config {
	return Config{
		SignallingURL:         "ws://localhost:8889",
		SFUID:                 "SFU",
		SubscribeStreamerID:   "DefaultStreamer",
		RetrySubscribeDelay:   10 * time.Second,
		LogLevel:              "info",
		ICE:                   ICEConfig{PortMin: 40000, PortMax: 49999},
		BWE:                   BWEConfig{InitialBitrate: 20_000_000, MinBitrate: 100_000, MaxBitrate: 100_000_000},
		Codecs:                []string{"vp8", "vp9", "h264", "opus"},
		MaxDataChannelMessage: 256 << 10,
		GatherTimeout:         5 * time.Second,
	}
}

// LoadConfig reads a YAML config file over the defaults. An empty path returns the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true) // a misspelt security setting must not be silently ignored
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	return cfg, cfg.Validate()
}

// Validate checks the config and loads the token file.
func (c *Config) Validate() error {
	if c.SignallingURL == "" || !(strings.HasPrefix(c.SignallingURL, "ws://") || strings.HasPrefix(c.SignallingURL, "wss://")) {
		return errors.New("sfu: signalling_url must be a ws:// or wss:// URL")
	}
	if c.SFUID == "" {
		return errors.New("sfu: sfu_id must be set")
	}
	if c.RetrySubscribeDelay <= 0 {
		c.RetrySubscribeDelay = 10 * time.Second
	}
	if c.MaxDataChannelMessage <= 0 {
		c.MaxDataChannelMessage = 256 << 10
	}
	if c.GatherTimeout <= 0 {
		c.GatherTimeout = 5 * time.Second
	}
	for _, name := range c.Codecs {
		if _, ok := codecsByName[strings.ToLower(name)]; !ok {
			return fmt.Errorf("sfu: unknown codec %q", name)
		}
	}
	if c.TokenFile != "" && c.Token == "" {
		b, err := config.Secret("", c.TokenFile)
		if err != nil {
			return fmt.Errorf("sfu: token_file: %w", err)
		}
		c.Token = string(b)
	}
	return nil
}
