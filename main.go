// main.go — ubersdr_airsplice: multi-channel simultaneous audio recorder
//
// Usage:
//
//	ubersdr_airsplice -url ws://sdr.example.com/ws \
//	                  -channel 7880000:usb \
//	                  -channel 14300000:usb \
//	                  -listen :6095 \
//	                  -output ./recordings
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// channelFlag is a repeatable -channel flag value.
type channelFlag []string

func (c *channelFlag) String() string { return strings.Join(*c, ", ") }
func (c *channelFlag) Set(v string) error {
	*c = append(*c, v)
	return nil
}

func main() {
	var (
		ubersdrURL = flag.String("url", "", "UberSDR WebSocket URL (e.g. ws://host/ws)")
		password   = flag.String("password", "", "UberSDR password (optional)")
		listenAddr = flag.String("listen", ":6095", "HTTP listen address")
		outputDir  = flag.String("output", "./recordings", "Directory to save recordings")
		uiPassword = flag.String("ui-password", envOr("UI_PASSWORD", ""),
			"Password required for write actions in the web UI (env: UI_PASSWORD; empty = write actions disabled)")

		// Segment / rotation settings
		segmentSecs = flag.Int("segment-secs", envIntOr("SEGMENT_SECS", 300),
			"Recording segment length in seconds; 0 = continuous (env: SEGMENT_SECS)")

		// Cleanup settings
		cleanupAllDays = flag.Int("cleanup-all-days", envIntOr("CLEANUP_ALL_DAYS", 30),
			"Delete ALL recordings older than N days; 0 = disabled (env: CLEANUP_ALL_DAYS)")
	)

	var channels channelFlag
	flag.Var(&channels, "channel", "Frequency:mode to record, e.g. 7880000:usb (repeatable)")

	flag.Parse()

	if *ubersdrURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(1)
	}
	if len(channels) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one -channel is required")
		flag.Usage()
		os.Exit(1)
	}

	log.Printf("[main] ubersdr_airsplice starting")
	log.Printf("[main] UberSDR URL:   %s", *ubersdrURL)
	log.Printf("[main] Output dir:    %s", *outputDir)
	log.Printf("[main] Listen addr:   %s", *listenAddr)
	log.Printf("[main] Segment secs:  %d", *segmentSecs)

	hub := newSSEHub()
	store := newRecordingStore(*outputDir, hub)

	// Start background cleanup worker (no-op when threshold is 0).
	startAgeCleanup(store, *outputDir, *cleanupAllDays)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse and start each channel.
	var recChannels []*recChannel
	for _, spec := range channels {
		freqHz, mode, err := parseChannelSpec(spec)
		if err != nil {
			log.Fatalf("[main] invalid -channel %q: %v", spec, err)
		}
		inst := newInstance(freqHz, 0, mode, *ubersdrURL, *password)
		ch := newRecChannel(inst, store, hub, *segmentSecs)
		recChannels = append(recChannels, ch)
		log.Printf("[main] starting channel %s (%d Hz, %s)", inst.label, freqHz, mode)
		go ch.run(ctx)
	}

	// Start HTTP server in background.
	go func() {
		if err := startHTTPServer(*listenAddr, store, hub, recChannels, *uiPassword); err != nil {
			log.Fatalf("[main] HTTP server: %v", err)
		}
	}()

	// Wait for SIGINT / SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("[main] shutting down…")
	cancel()
	log.Printf("[main] done")
}

// parseChannelSpec parses "7880000:usb" → (7880000, "usb", nil).
func parseChannelSpec(spec string) (int, string, error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("expected freq:mode, got %q", spec)
	}
	freq, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, "", fmt.Errorf("invalid frequency %q: %w", parts[0], err)
	}
	mode := strings.TrimSpace(parts[1])
	if mode == "" {
		return 0, "", fmt.Errorf("empty mode in %q", spec)
	}
	return freq, mode, nil
}
