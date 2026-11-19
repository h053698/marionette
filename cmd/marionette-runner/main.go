// A runner consumes opaque Kafka job IDs. The backend grants a separate,
// revocable claim for each job; no user credentials travel through Kafka.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"marionette/internal/jobqueue"
	"marionette/internal/service"
)

type runner struct {
	origin, token string
	http          *http.Client
}

var errUnclaimed = errors.New("job claim was not acknowledged")

func main() {
	if err := run(); err != nil {
		slog.Error("runner stopped", "error", err)
		os.Exit(1)
	}
}
func run() (err error) {
	origin := flag.String("backend", os.Getenv("MARIONETTE_BACKEND_URL"), "backend origin")
	group := flag.String("group", service.Env("MARIONETTE_RUNNER_GROUP", "marionette-runners"), "shared Kafka consumer group; concurrency is bounded by topic partitions")
	flag.Parse()
	level, format, err := service.ConfigureLogging()
	if err != nil {
		return err
	}
	token := os.Getenv("MARIONETTE_RUNNER_TOKEN")
	if !service.TokenValid(token) {
		return errors.New("MARIONETTE_RUNNER_TOKEN requires at least 32 bytes and no whitespace")
	}
	backendURL, err := service.Origin(strings.TrimRight(*origin, "/"))
	if err != nil {
		return fmt.Errorf("--backend / MARIONETTE_BACKEND_URL %w", err)
	}
	if strings.TrimSpace(*group) == "" || strings.ContainsAny(*group, "\x00\r\n") {
		return errors.New("--group / MARIONETTE_RUNNER_GROUP requires a non-empty Kafka consumer group name")
	}
	queue := jobqueue.FromEnv()
	if err := service.ValidateKafka(queue.Brokers, queue.Topic, queue.TLS, queue.Username, queue.Password); err != nil {
		return err
	}
	r := &runner{origin: backendURL, token: token, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	ctx, finish := service.ShutdownContext()
	defer func() { finish(err) }()
	consumer, err := jobqueue.New(queue, *group)
	if err != nil {
		return err
	}
	defer consumer.Close()
	service.LogLifecycle("runner configuration", level, format,
		slog.String("backend_url", backendURL), slog.String("group", *group), slog.Any("kafka_brokers", queue.Brokers),
		slog.String("kafka_topic", queue.Topic), slog.Bool("kafka_tls", queue.TLS), slog.Bool("kafka_sasl", queue.Username != ""))
	for ctx.Err() == nil {
		fetches := consumer.PollRecords(ctx, 1)
		if fetches.IsClientClosed() {
			return nil
		}
		for _, failure := range fetches.Errors() {
			if ctx.Err() == nil {
				slog.Warn("Kafka fetch failed", "error", failure.Err)
			}
		}
		for _, record := range fetches.Records() {
			id := string(record.Value)
			if len(id) != 32 || strings.ContainsAny(id, "/\\.\x00\r\n") {
				return errors.New("invalid Kafka job ID; refusing to discard malformed queue data")
			}
			if err := r.execute(ctx, id); err != nil {
				slog.Warn("job stopped", "job", id, "error", err)
				if errors.Is(err, errUnclaimed) {
					consumer.AllowRebalance()
					return err
				}
			}
			if ctx.Err() == nil {
				commit, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := consumer.CommitRecords(commit, record)
				cancel()
				if err != nil {
					consumer.AllowRebalance()
					return fmt.Errorf("commit job offset: %w", err)
				}
			}
		}
		consumer.AllowRebalance()
	}
	return nil
}

func (r *runner) request(ctx context.Context, id, operation, claim string, body any, out any) (int, error) {
	if operation != "occupy" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", r.origin+"/internal/runners/jobs/"+id+"/"+operation, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("Content-Type", "application/json")
	if claim != "" {
		request.Header.Set("X-Job-Token", claim)
	}
	response, err := r.http.Do(request)
	if err != nil {
		return 0, errors.New("backend request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return response.StatusCode, fmt.Errorf("backend %s returned %d", operation, response.StatusCode)
	}
	if out != nil {
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out)
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	}
	return response.StatusCode, err
}

func (r *runner) execute(parent context.Context, id string) error {
	var claim struct {
		Job struct {
			Steps          []json.RawMessage `json:"steps"`
			TimeoutSeconds int               `json:"timeoutSeconds"`
		} `json:"job"`
		Token string `json:"claimToken"`
	}
	status, err := r.request(parent, id, "claim", "", nil, &claim)
	if status == 409 || status == 404 {
		return nil
	} // Duplicate delivery or job cancelled before consumption.
	if err != nil {
		return fmt.Errorf("%w: %v", errUnclaimed, err)
	}
	slog.Info("job claimed", "job", id, "pid", os.Getpid())
	ctx, cancel := context.WithTimeout(parent, time.Duration(claim.Job.TimeoutSeconds)*time.Second)
	defer cancel()
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		tick := time.NewTicker(8 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				ping, done := context.WithTimeout(ctx, 5*time.Second)
				status, _ := r.request(ping, id, "heartbeat", claim.Token, nil, nil)
				done()
				if status == 409 || status == 401 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); heartbeat.Wait() }()
	defer func() {
		finish, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, _ = r.request(finish, id, "finish", claim.Token, nil, nil)
	}()
	if _, err := r.request(ctx, id, "occupy", claim.Token, nil, nil); err != nil {
		return err
	}
	for index := range claim.Job.Steps {
		if _, err := r.request(ctx, id, "step", claim.Token, map[string]int{"index": index}, nil); err != nil {
			return err
		}
	}
	slog.Info("job completed", "job", id, "steps", len(claim.Job.Steps))
	return nil
}
