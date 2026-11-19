package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"marionette/client"
	"marionette/internal/adbrelay"
	"marionette/internal/direct"

	"github.com/pion/webrtc/v4"
)

// adbPath is either a direct WebRTC peer or the backend relay. Both expose the
// same lease watch stream and per-service opener.
type adbPath struct {
	label    string
	features string
	watch    adbrelay.Stream
	open     adbrelay.Opener
	close    func()
}

func openPath(ctx context.Context, api *client.Client, id, token, mode string, ice []webrtc.ICEServer) (adbPath, error) {
	if mode != "relay" {
		path, err := openDirect(ctx, api, id, token, ice)
		if err == nil {
			return path, nil
		}
		if mode == "direct" {
			return adbPath{}, fmt.Errorf("direct connection unavailable: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Direct connection unavailable (%v); using the backend relay.\n", err)
	}
	ws, hello, err := api.OpenADB(ctx, id, token, "watch")
	if err != nil {
		return adbPath{}, err
	}
	watch := adbrelay.WebSocket(ws)
	return adbPath{
		label:    "backend relay",
		features: hello.Features,
		watch:    watch,
		open: func(ctx context.Context, service string) (adbrelay.Stream, error) {
			ws, _, err := api.OpenADB(ctx, id, token, service)
			if err != nil {
				return nil, err
			}
			return adbrelay.WebSocket(ws), nil
		},
		close: func() { watch.Close() },
	}, nil
}

func openDirect(ctx context.Context, api *client.Client, id, token string, ice []webrtc.ICEServer) (adbPath, error) {
	peer, err := direct.Connect(ctx, ice, func(ctx context.Context, offer string) (string, error) {
		return api.DirectOffer(ctx, id, token, offer)
	})
	if err != nil {
		return adbPath{}, err
	}
	watch, hello, err := peer.Open(ctx, "watch")
	if err != nil {
		peer.Close()
		return adbPath{}, err
	}
	return adbPath{
		label:    "direct (WebRTC, backend not in the data path)",
		features: hello.Features,
		watch:    channelStream{watch},
		open: func(ctx context.Context, service string) (adbrelay.Stream, error) {
			ch, _, err := peer.Open(ctx, service)
			if err != nil {
				return nil, err
			}
			return channelStream{ch}, nil
		},
		close: func() { watch.Close(); peer.Close() },
	}, nil
}

type channelStream struct{ *direct.Channel }

func (c channelStream) Read() ([]byte, error) { return c.Channel.Read(time.Time{}) }

func adbCommand(parent context.Context, api *client.Client, current session, statePath string, args []string) error {
	f := flag.NewFlagSet("adb", flag.ContinueOnError)
	port := f.Int("port", 0, "local loopback port; zero chooses an available port")
	adb := f.String("adb", "", "local platform-tools/adb path, or discover from PATH / Android SDK")
	mode := f.String("mode", "auto", "auto tries a direct WebRTC path first; direct requires it; relay always uses the backend")
	ice := f.String("ice", os.Getenv("MARIONETTE_ICE_SERVERS"), "comma-separated STUN/TURN URLs, or none (default: public STUN)")
	if err := f.Parse(args); err != nil {
		return helpError(err)
	}
	if *mode != "auto" && *mode != "direct" && *mode != "relay" {
		return errors.New("--mode must be auto, direct or relay")
	}
	if *port < 0 || *port > 65535 {
		return errors.New("invalid local port")
	}
	if *adb == "" {
		*adb, _ = exec.LookPath("adb")
		if *adb == "" {
			home, _ := os.UserHomeDir()
			for _, root := range []string{os.Getenv("ANDROID_HOME"), os.Getenv("ANDROID_SDK_ROOT"), filepath.Join(home, "Library", "Android", "sdk"), filepath.Join(home, "Android", "Sdk")} {
				if root == "" {
					continue
				}
				candidate := filepath.Join(root, "platform-tools", "adb")
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
					*adb = candidate
					break
				}
			}
		}
	}
	if *adb == "" {
		return errors.New("local adb not found; pass --adb PATH (Android Studio SDK platform-tools/adb)")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	id, token := current.Assignment.Assigned, current.Assignment.Lease.Token
	path, err := openPath(ctx, api, id, token, *mode, direct.ICEServers(*ice))
	if err != nil {
		return err
	}
	defer path.close()
	relay, err := adbrelay.New(ctx, path.open, current.Assignment.Device.Model, path.features, *port)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer func() { cancel(); path.close(); relay.Close(); wg.Wait() }()
	wg.Add(3)
	go func() { defer wg.Done(); _ = relay.Run(); cancel() }()
	go func() {
		defer wg.Done()
		// The watch stream ends when the agent revokes the lease.
		for {
			if _, err := path.watch.Read(); err != nil {
				cancel()
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		// Only this running relay renews the assignment; stopping it ends renewal.
		for {
			remaining := time.Until(current.Assignment.Lease.ExpiresAt)
			delay := min(60*time.Second, remaining/3)
			if delay < time.Second {
				delay = time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			latest, err := readSession(statePath, current.URL)
			if err != nil || !sameSession(latest, current) {
				cancel()
				return
			}
			lease, err := api.Renew(ctx, id, token, 300)
			if err != nil {
				fmt.Fprintln(os.Stderr, "ADB lease renewal failed; connection closed")
				cancel()
				return
			}
			current = latest
			setAssignmentLease(&current.Assignment, lease)
			if saveCurrentSession(statePath, current) != nil {
				fmt.Fprintln(os.Stderr, "cannot save renewed lease; connection closed")
				cancel()
				return
			}
		}
	}()
	connectCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	result, err := exec.CommandContext(connectCtx, *adb, "connect", relay.Address()).CombinedOutput()
	stop()
	if err != nil {
		return errors.New("local adb connect failed")
	}
	fmt.Fprint(os.Stdout, string(result))
	fmt.Fprintf(os.Stdout, "Connection path: %s\n", path.label)
	fmt.Fprintf(os.Stdout, "Assigned device %s is available as %s. Select it in Android Studio, or run:\n  adb -s %s shell\nKeep this command running. Ctrl+C disconnects ADB and stops lease renewal.\n", id, relay.Address(), relay.Address())
	defer func() {
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = exec.CommandContext(ctx, *adb, "disconnect", relay.Address()).Run()
	}()
	<-ctx.Done()
	if parent.Err() == nil {
		return errors.New("device access ended; ADB disconnected")
	}
	return nil
}
