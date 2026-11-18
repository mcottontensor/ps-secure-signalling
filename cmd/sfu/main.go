// Command sfu runs the Pixel Streaming Selective Forwarding Unit.
//
//	sfu -config sfu.yaml
//	sfu -signalling-url wss://signalling.example:8889 -public-ip 203.0.113.7
//
// The old SFU's flags (--signallingURL, --PublicIP) are accepted too.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/epicgames-ps/ps-secure-signalling/pkg/sfu"
)

func main() {
	var configPath, url, publicIP string
	flag.StringVar(&configPath, "config", "", "YAML config file")
	flag.StringVar(&url, "signalling-url", "", "signalling server SFU URL (overrides the config file)")
	flag.StringVar(&url, "signallingURL", "", "alias of -signalling-url, as the old SFU took it")
	flag.StringVar(&publicIP, "public-ip", "", "public IP to advertise for media (overrides the config file)")
	flag.StringVar(&publicIP, "PublicIP", "", "alias of -public-ip, as the old SFU took it")
	flag.Parse()

	cfg, err := sfu.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if url != "" {
		cfg.SignallingURL = url
	}
	if publicIP != "" {
		cfg.ICE.PublicIPs = []string{publicIP}
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(cfg.LogLevel))); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	s, err := sfu.New(cfg, log)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting SFU", "id", cfg.SFUID, "signalling", cfg.SignallingURL)
	_ = s.Run(ctx)
	log.Info("stopped")
}
