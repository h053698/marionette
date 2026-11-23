// Marionette is a remote device CLI. Only the PC agent needs platform tooling.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"marionette/client"
	"marionette/internal/service"
)

type session struct {
	URL        string            `json:"url"`
	Assignment client.Assignment `json:"assignment"`
}

var version = "dev"
var commit = "unknown"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("marionette %s (%s)\n", version, commit)
		return nil
	}
	global := flag.NewFlagSet("marionette", flag.ContinueOnError)
	origin := os.Getenv("MARIONETTE_URL")
	if origin == "" {
		origin = "http://127.0.0.1:8080"
	}
	url := global.String("url", origin, "backend origin")
	statePath := global.String("session", ".marionette/session.json", "private lease state file")
	global.Usage = func() {
		fmt.Fprintln(global.Output(), "Usage: marionette [--url ORIGIN] [--session PATH] devices|occupy|attach|renew|release|info|apps|processes|launch|tree|find|click|type|tap|swipe|key|action|screenshot|adb|job [options]\nSet MARIONETTE_TOKEN to a personal API token. Run a command with --help for its options.")
	}
	if err := global.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if global.NArg() == 0 {
		global.Usage()
		return nil
	}
	api, err := client.New(*url, os.Getenv("MARIONETTE_TOKEN"))
	if err != nil {
		return err
	}
	ctx, cancel := service.Context()
	defer cancel()
	command, args := global.Arg(0), global.Args()[1:]
	var wipe bool
	if command == "release" {
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		f.BoolVar(&wipe, "wipe", false, "uninstall third-party apps and clear shared storage on return")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		if f.NArg() != 0 {
			return errors.New("usage: release [--wipe]")
		}
	}
	if command == "job" {
		f := flag.NewFlagSet("job", flag.ContinueOnError)
		file := f.String("file", "", "JSON job request path for submit")
		id := f.String("id", "", "job ID for status/cancel")
		index := f.Int("index", 0, "screenshot step index for artifact")
		target := f.String("o", "", "PNG output path for artifact")
		if len(args) == 0 {
			return errors.New("usage: job submit --file PATH | job status --id ID | job wait --id ID | job cancel --id ID")
		}
		if err := f.Parse(args[1:]); err != nil {
			return helpError(err)
		}
		switch args[0] {
		case "submit":
			if *file == "" {
				return errors.New("job submit requires --file PATH")
			}
			input, err := os.Open(*file)
			if err != nil {
				return err
			}
			defer input.Close()
			var request client.JobRequest
			decoder := json.NewDecoder(io.LimitReader(input, (1<<20)+1))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return err
			}
			result, err := api.SubmitJob(ctx, request)
			if err != nil {
				return err
			}
			return output(result)
		case "status":
			result, err := api.Job(ctx, *id)
			if err != nil {
				return err
			}
			return output(result)
		case "wait":
			for {
				result, err := api.Job(ctx, *id)
				if err != nil {
					return err
				}
				if result.Status != "queued" && result.Status != "running" {
					if err := output(result); err != nil {
						return err
					}
					if result.Status != "succeeded" {
						return errors.New("job " + result.Status)
					}
					return nil
				}
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		case "cancel":
			if err := api.CancelJob(ctx, *id); err != nil {
				return err
			}
			return output(map[string]bool{"cancelled": true})
		case "artifact":
			if *target == "" {
				return errors.New("job artifact requires -o PATH")
			}
			data, err := api.JobArtifact(ctx, *id, *index)
			if err != nil {
				return err
			}
			return outputFile(*target, data)
		default:
			return errors.New("unknown job command")
		}
	}
	if command == "devices" {
		devices, err := api.Devices(ctx)
		if err != nil {
			return err
		}
		return output(devices)
	}
	if command == "attach" {
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		device := f.String("device", "", "device ID from the dashboard")
		code := f.String("code", os.Getenv("MARIONETTE_CONNECT_CODE"), "single-use dashboard code (90 seconds)")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		if *device == "" || *code == "" {
			return errors.New("attach requires --device and --code")
		}
		old, live, err := existingSession(ctx, api, *statePath, *url)
		if err != nil {
			return err
		}
		if live && old.Assignment.Assigned != *device {
			return errors.New("release the current session before attaching another device")
		}
		devices, err := api.Devices(ctx)
		if err != nil {
			return err
		}
		var selected client.Device
		for _, d := range devices {
			if d.Serial == *device {
				selected = d
				break
			}
		}
		if selected.Serial == "" {
			return errors.New("device not found")
		}
		lease, err := api.Attach(ctx, *device, *code)
		if err != nil {
			return err
		}
		if err := api.Start(ctx, *device, lease.Token); err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = api.Release(cleanup, *device, lease.Token)
			return err
		}
		assignment := client.Assignment{Assigned: *device, Device: selected, Lease: lease}
		setAssignmentLease(&assignment, lease)
		if err := saveSession(*statePath, session{*url, assignment}); err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = api.Release(cleanup, *device, lease.Token)
			return err
		}
		return output(map[string]any{"assigned": *device, "expiresAt": lease.ExpiresAt, "next": "marionette adb"})
	}
	if command == "occupy" {
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		platform := f.String("platform", "android", "device platform (android)")
		tags := f.String("tags", "", "comma-separated tags (all must match)")
		model := f.String("model", "", "exact device model")
		device := f.String("device", "", "specific device ID, or omit for automatic assignment")
		ttl := f.Int("ttl", 300, "lease seconds, 30–3600")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		if *platform != "" && *platform != "android" {
			return errors.New("only the android platform is supported")
		}
		if *ttl != 0 && (*ttl < 30 || *ttl > 3600) {
			return errors.New("ttlSeconds must be between 30 and 3600")
		}
		selection := client.Selection{Platform: *platform, Model: *model, DeviceID: *device}
		if *tags != "" {
			for _, tag := range strings.Split(*tags, ",") {
				selection.Tags = append(selection.Tags, strings.TrimSpace(tag))
			}
		}
		old, live, err := existingSession(ctx, api, *statePath, *url)
		if err != nil {
			return err
		}
		if live {
			if !matchesAssignment(old.Assignment, selection) {
				return errors.New("session already has a live lease; release it before occupying another device")
			}
			if err := api.Start(ctx, old.Assignment.Assigned, old.Assignment.Lease.Token); err != nil {
				return err
			}
			return output(map[string]any{"assigned": old.Assignment.Assigned, "device": old.Assignment.Device, "expiresAt": old.Assignment.Lease.ExpiresAt, "resumed": true})
		}
		assignment, err := api.Occupy(ctx, selection, *ttl)
		if err != nil {
			return err
		}
		if err := saveSession(*statePath, session{*url, assignment}); err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = api.Release(cleanup, assignment.Assigned, assignment.Lease.Token)
			return fmt.Errorf("save private lease state: %w", err)
		}
		return output(map[string]any{"assigned": assignment.Assigned, "device": assignment.Device, "expiresAt": assignment.Lease.ExpiresAt})
	}
	current, err := readSession(*statePath, *url)
	if err != nil {
		return fmt.Errorf("run occupy first: %w", err)
	}
	if command == "release" {
		if err := api.ReleaseWith(ctx, current.Assignment.Assigned, current.Assignment.Lease.Token, wipe); err != nil {
			return err
		}
		if err := removeCurrentSession(*statePath, current); err != nil {
			return err
		}
		return output(map[string]bool{"released": true})
	}
	current.Assignment, err = api.RefreshAssignment(ctx, current.Assignment)
	if err != nil {
		return err
	}
	if err := saveCurrentSession(*statePath, current); err != nil {
		return err
	}
	id, token := current.Assignment.Assigned, current.Assignment.Lease.Token
	var body map[string]any
	switch command {
	case "adb":
		return adbCommand(ctx, api, current, *statePath, args)
	case "renew":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		ttl := f.Int("ttl", 300, "lease seconds")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		lease, err := api.Renew(ctx, id, token, *ttl)
		if err != nil {
			return err
		}
		setAssignmentLease(&current.Assignment, lease)
		if err := saveCurrentSession(*statePath, current); err != nil {
			return err
		}
		return output(map[string]any{"assigned": id, "expiresAt": lease.ExpiresAt})
	case "screenshot":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		target := f.String("o", "", "PNG output path (required)")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		if *target == "" {
			return errors.New("screenshot requires -o PATH")
		}
		png, err := api.Screenshot(ctx, id, token)
		if err != nil {
			return err
		}
		return outputFile(*target, png)
	case "info":
		body = map[string]any{"action": "info"}
	case "apps":
		body = map[string]any{"action": "apps"}
	case "processes":
		body = map[string]any{"action": "processes"}
	case "launch":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		packageName := f.String("package", "", "Android package name (required)")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		if *packageName == "" {
			return errors.New("launch requires --package NAME")
		}
		body = map[string]any{"action": "launchApp", "packageName": *packageName}
	case "tree":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		format := f.String("format", "json", "json or xml")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		result, err := api.UITree(ctx, id, token, *format)
		if err != nil {
			return err
		}
		if *format == "xml" {
			var tree struct {
				XML string `json:"xml"`
			}
			if err := json.Unmarshal(result, &tree); err != nil {
				return err
			}
			_, err = fmt.Fprintln(os.Stdout, tree.XML)
			return err
		}
		return output(result)
	case "find", "click", "type":
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		resource := f.String("id", "", "exact resource ID, e.g. package:id/input")
		text := f.String("text", "", "exact element text")
		description := f.String("description", "", "exact accessibility description")
		class := f.String("class", "", "exact class name")
		index := f.Int("index", 0, "zero-based matching element index")
		wait := f.Int("wait", 5000, "wait for an element, 0–8000 milliseconds")
		value := f.String("value", "", "replacement text for type (empty clears field)")
		if err := f.Parse(args); err != nil {
			return helpError(err)
		}
		selector := client.Selector{ResourceID: *resource, Text: *text, Description: *description, ClassName: *class, Index: *index}
		hasSelector := *resource != "" || *text != "" || *description != "" || *class != ""
		if !hasSelector && command != "type" {
			return errors.New("provide --id, --text, --description or --class")
		}
		action := map[string]string{"find": "findElement", "click": "click", "type": "text"}[command]
		body = map[string]any{"action": action, "waitMs": *wait}
		if hasSelector {
			body["selector"] = selector
		}
		if command == "type" {
			body["text"] = *value
		}
	case "tap", "swipe", "key", "action":
		if len(args) != 1 {
			return errors.New("supply one JSON object, e.g. tap '{\"x\":100,\"y\":200}'")
		}
		if err := json.Unmarshal([]byte(args[0]), &body); err != nil || body == nil {
			return errors.New("expected a JSON object")
		}
		if command != "action" {
			body["action"] = command
		}
		if command == "swipe" {
			if _, ok := body["durationMs"]; !ok {
				body["durationMs"] = 300
			}
		}
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	result, err := api.Action(ctx, id, token, body)
	if err != nil {
		return err
	}
	return output(result)
}

func helpError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// A saved expiry is only a snapshot: the browser or another CLI may renew the
// same lease. Only server inventory decides whether that lease still exists.
func existingSession(ctx context.Context, api *client.Client, path, origin string) (session, bool, error) {
	current, err := readSession(path, origin)
	if errors.Is(err, os.ErrNotExist) {
		return session{}, false, nil
	}
	if err != nil {
		return session{}, false, err
	}
	current.Assignment, err = api.RefreshAssignment(ctx, current.Assignment)
	if err != nil {
		if errors.Is(err, client.ErrLeaseNotCurrent) {
			return session{}, false, nil
		}
		return session{}, false, err
	}
	if err := saveCurrentSession(path, current); err != nil {
		return session{}, false, err
	}
	return current, true, nil
}

func matchesAssignment(assignment client.Assignment, selection client.Selection) bool {
	d := assignment.Device
	if selection.DeviceID != "" && selection.DeviceID != assignment.Assigned || selection.Platform != "" && selection.Platform != d.Platform || selection.Model != "" && !strings.EqualFold(selection.Model, d.Model) {
		return false
	}
	for _, tag := range selection.Tags {
		found := false
		for _, available := range d.Tags {
			if available == tag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func setAssignmentLease(assignment *client.Assignment, lease client.Lease) {
	assignment.Lease = lease
	assignment.Device.Occupied = true
	assignment.Device.Mine = true
	assignment.Device.Owner = lease.Owner
	assignment.Device.LeaseID = lease.ID
	assignment.Device.ExpiresAt = &assignment.Lease.ExpiresAt
}

func sameSession(a, b session) bool {
	return a.URL == b.URL && a.Assignment.Assigned == b.Assignment.Assigned && a.Assignment.Lease.ID == b.Assignment.Lease.ID && a.Assignment.Lease.Token == b.Assignment.Lease.Token
}

func saveCurrentSession(path string, state session) error {
	latest, err := readSession(path, state.URL)
	if err != nil {
		return err
	}
	if !sameSession(latest, state) {
		return errors.New("saved session changed; retry with the current session")
	}
	return saveSession(path, state)
}

func removeCurrentSession(path string, state session) error {
	latest, err := readSession(path, state.URL)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !sameSession(latest, state) {
		return errors.New("lease released; saved session changed and was preserved")
	}
	return os.Remove(path)
}

func output(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func outputFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return output(map[string]string{"path": path})
}
func readSession(path, origin string) (session, error) {
	var state session
	file, err := os.Open(path)
	if err != nil {
		return state, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() {
		return state, errors.New("session file must be a private regular file (chmod 600)")
	}
	if err := secureSessionFile(file); err != nil {
		return state, err
	}
	if info.Size() > 1<<20 {
		return state, errors.New("session file too large")
	}
	err = json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&state)
	if err == nil && (state.URL != origin || state.Assignment.Assigned == "" || state.Assignment.Lease.ID == "" || state.Assignment.Lease.Token == "") {
		err = errors.New("invalid session or session belongs to another API origin")
	}
	return state, err
}
func saveSession(path string, state session) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".lease-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := secureSessionFile(file); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(state); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
