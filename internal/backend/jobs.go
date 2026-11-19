package backend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"marionette/client"
	"marionette/internal/identity"
	"marionette/internal/jobqueue"
)

type storedJob struct {
	client.Job
	User      identity.User `json:"user"`
	Published bool          `json:"published"`
	ClaimHash string        `json:"claimHash,omitempty"`
	Heartbeat time.Time     `json:"heartbeat"`
	Executing bool          `json:"executing"`
	Lease     client.Lease  `json:"lease"`
}
type jobs struct {
	mu                       sync.Mutex
	records                  map[string]storedJob
	active                   map[string]context.CancelFunc
	file, runnerToken, topic string
	producer                 *kgo.Client
	ctx                      context.Context
	cancel                   context.CancelFunc
	wg                       sync.WaitGroup
	retention                time.Duration
	artifactLimit            int64
	artifacts                map[string]jobArtifacts
	dirty                    bool
}

const (
	minJobTimeoutSeconds = 10
	jobLeaseGraceSeconds = 30
	maxJobClaimJSON      = 900 << 10
	maxJobResultJSON     = 64 << 10
)

type jobClaim struct {
	Job        client.Job `json:"job"`
	ClaimToken string     `json:"claimToken"`
}

const (
	jobCancelled       = "job_cancelled"
	jobInterrupted     = "job_interrupted"
	jobTimedOut        = "job_timed_out"
	jobAllocation      = "job_allocation_failed"
	jobStep            = "job_step_failed"
	jobArtifactLimit   = "job_artifact_limit"
	jobArtifactFailure = "job_artifact_failed"
	jobPersistence     = "job_persistence_failed"
	jobFailed          = "job_failed"
	jobPayloadTooLarge = "job_payload_too_large"
)

var jobMessages = map[string]string{
	jobCancelled:       "작업이 취소되었습니다.",
	jobInterrupted:     "작업이 중단되었습니다. 자동으로 다시 실행하지 않습니다.",
	jobTimedOut:        "실행기 연결이 끊겼거나 작업 시간이 초과되었습니다.",
	jobAllocation:      "작업에 사용할 기기를 배정하지 못했습니다.",
	jobStep:            "기기에서 작업 단계를 완료하지 못했습니다.",
	jobArtifactLimit:   "계정의 작업 아티팩트 저장 한도에 도달했습니다.",
	jobArtifactFailure: "작업 아티팩트를 저장하지 못했습니다.",
	jobPersistence:     "작업 상태를 저장하지 못했습니다.",
	jobFailed:          "작업이 실패했습니다.",
	jobPayloadTooLarge: "작업 요청이 너무 큽니다. 단계 수 또는 입력 크기를 줄여 주세요.",
}

func respondJobError(w http.ResponseWriter, status int, code string) {
	respond(w, status, map[string]string{"code": code, "error": jobMessages[code]})
}

// EnableJobs adds durable submissions and Kafka delivery. One backend owns this
// store; any number of runner processes may consume its Kafka consumer group.
func (s *Server) EnableJobs(cfg jobqueue.Config, file, runnerToken string) error {
	if s.leases.MaxTTL < (minJobTimeoutSeconds+jobLeaseGraceSeconds)*time.Second {
		return errors.New("MARIONETTE_LEASE_MAX_TTL must be at least 40s when Kafka jobs are enabled")
	}
	if len(runnerToken) < 32 || strings.ContainsAny(runnerToken, " \t\r\n") {
		return errors.New("MARIONETTE_RUNNER_TOKEN must contain at least 32 bytes")
	}
	retention := 168 * time.Hour
	if raw := os.Getenv("MARIONETTE_JOB_RETENTION"); raw != "" {
		var err error
		retention, err = time.ParseDuration(raw)
		if err != nil || retention <= 0 {
			return errors.New("MARIONETTE_JOB_RETENTION must be a positive duration")
		}
	}
	artifactLimit, err := positiveIntEnv("MARIONETTE_JOB_ARTIFACT_MAX_BYTES_PER_ACCOUNT", 256<<20, 1<<40)
	if err != nil {
		return err
	}
	producer, err := jobqueue.New(cfg, "")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &jobs{records: make(map[string]storedJob), active: make(map[string]context.CancelFunc), file: file, runnerToken: runnerToken, topic: cfg.Topic, producer: producer, ctx: ctx, cancel: cancel, retention: retention, artifactLimit: artifactLimit, artifacts: make(map[string]jobArtifacts)}
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		cancel()
		producer.Close()
		return err
	}
	if err == nil {
		if len(data) > maxJobStoreBytes || json.Unmarshal(data, &j.records) != nil || j.records == nil || len(j.records) > maxJobStoreRecords {
			cancel()
			producer.Close()
			return errors.New("invalid job store")
		}
		for id, record := range j.records {
			if id != record.ID || len(id) != 32 || strings.ContainsAny(id, "/\\.\x00\r\n") || record.User.Subject == "" || len(record.Steps) < 1 || len(record.Steps) > 100 || record.Completed < 0 || record.Completed > len(record.Steps) || record.Completed != len(record.Results) {
				cancel()
				producer.Close()
				return errors.New("invalid job record")
			}
			if record.Status == "running" {
				now := time.Now()
				record.Status, record.ErrorCode, record.FinishedAt = "failed", jobInterrupted, &now
			}
			if record.Status == "failed" || record.Status == "cancelled" {
				if _, valid := jobMessages[record.ErrorCode]; !valid {
					record.ErrorCode = jobFailed
					if record.Status == "cancelled" {
						record.ErrorCode = jobCancelled
					}
				}
				record.Error = jobMessages[record.ErrorCode]
			} else {
				record.Error, record.ErrorCode = "", ""
			}
			if record.terminal() {
				record.ClaimHash, record.Lease, record.Executing = "", client.Lease{}, false
			}
			j.records[id] = record
		}
	}
	if err := j.loadArtifacts(); err != nil {
		cancel()
		producer.Close()
		return err
	}
	if err := j.prune(time.Now(), true); err != nil {
		cancel()
		producer.Close()
		return err
	}
	s.jobs = j
	j.wg.Add(2)
	go func() { defer j.wg.Done(); s.dispatchJobs() }()
	go func() { defer j.wg.Done(); j.maintain() }()
	return nil
}

func (j *jobs) writeStore(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(j.file), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(j.file), ".jobs-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
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
	return os.Rename(file.Name(), j.file)
}

// put persists before committing the state transition in memory. Call with mu held.
func (j *jobs) put(record storedJob) error {
	old, exists := j.records[record.ID]
	j.records[record.ID] = record
	if err := j.save(nil, record.ID); err != nil {
		if exists {
			j.records[record.ID] = old
		} else {
			delete(j.records, record.ID)
		}
		slog.Error("job state save failed", "jobID", record.ID, "error", err)
		return err
	}
	return nil
}

func (s *Server) dispatchJobs() {
	j := s.jobs
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-j.ctx.Done():
			return
		case <-tick.C:
		}
		j.mu.Lock()
		var pending []string
		var expired []string
		for id, record := range j.records {
			if record.Status == "queued" && !record.Published {
				pending = append(pending, id)
			}
			if record.Status == "running" && (time.Since(record.Heartbeat) > 45*time.Second || record.StartedAt != nil && time.Since(*record.StartedAt) > time.Duration(record.TimeoutSeconds)*time.Second) {
				expired = append(expired, id)
			}
		}
		j.mu.Unlock()
		for _, id := range expired {
			_ = s.finishJob(id, "failed", jobTimedOut)
		}
		for _, id := range pending {
			ctx, cancel := context.WithTimeout(j.ctx, 5*time.Second)
			err := jobqueue.Publish(ctx, j.producer, j.topic, id)
			cancel()
			if err != nil {
				break
			} // Durable outbox retries after broker recovery.
			j.mu.Lock()
			if record, ok := j.records[id]; ok && record.Status == "queued" {
				record.Published = true
				_ = j.put(record)
			}
			j.mu.Unlock()
		}
	}
}

func jobID() string {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}
func claimHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		respond(w, 503, map[string]string{"error": "job queue is not configured"})
		return
	}
	var in struct {
		Selection      client.Selection `json:"selection"`
		Steps          []map[string]any `json:"steps"`
		TimeoutSeconds int              `json:"timeoutSeconds"`
	}
	if err := decode(w, r, &in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			rejectJobSize(w)
			return
		}
		respond(w, 400, map[string]string{"error": "invalid job JSON"})
		return
	}
	if err := validateSelection(in.Selection); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	maxTimeout := min(3300, int(s.leases.MaxTTL/time.Second)-jobLeaseGraceSeconds)
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = min(300, maxTimeout)
	}
	if len(in.Steps) < 1 || len(in.Steps) > 100 || in.TimeoutSeconds < minJobTimeoutSeconds || in.TimeoutSeconds > maxTimeout {
		respond(w, 400, map[string]string{"error": fmt.Sprintf("jobs need 1–100 steps and timeoutSeconds %d–%d", minJobTimeoutSeconds, maxTimeout)})
		return
	}
	for _, step := range in.Steps {
		switch step["action"] {
		case "info", "tap", "click", "findElement", "swipe", "key", "text", "uiTree", "screenshot":
		default:
			respond(w, 400, map[string]string{"error": "job contains an unsupported action"})
			return
		}
	}
	user, _ := identity.FromContext(r.Context())
	record := storedJob{Job: client.Job{ID: jobID(), Status: "queued", Selection: in.Selection, Steps: in.Steps, TimeoutSeconds: in.TimeoutSeconds, CreatedAt: time.Now().UTC(), Results: make([]json.RawMessage, 0)}, User: user}
	if !fitsJobClaim(record.Job) {
		rejectJobSize(w)
		return
	}
	j := s.jobs
	j.mu.Lock()
	if !j.hasJobCapacity(record) {
		j.mu.Unlock()
		respond(w, 429, map[string]string{"error": "job limit reached"})
		return
	}
	err := j.put(record)
	j.mu.Unlock()
	if err != nil {
		respondJobError(w, 500, jobPersistence)
		return
	}
	respond(w, 202, record.Job)
}

func rejectJobSize(w http.ResponseWriter) {
	respondJobError(w, 413, jobPayloadTooLarge)
}

func fitsJobClaim(job client.Job) bool {
	// Reserve the longest UTC timestamp, the token and the trailing newline.
	started := time.Date(2030, 1, 1, 0, 0, 0, 999999999, time.UTC)
	job.Status, job.StartedAt = "running", &started
	data, err := json.Marshal(jobClaim{Job: job, ClaimToken: strings.Repeat("x", 32)})
	return err == nil && len(data)+1 <= maxJobClaimJSON
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		respond(w, 503, map[string]string{"error": "job queue is not configured"})
		return
	}
	user, _ := identity.FromContext(r.Context())
	j := s.jobs
	j.mu.Lock()
	record, ok := j.records[r.PathValue("id")]
	if !ok || record.User.Subject != user.Subject && !identity.IsAdmin(r.Context()) {
		j.mu.Unlock()
		respond(w, 404, map[string]string{"error": "unknown job"})
		return
	}
	if r.Method == "DELETE" {
		if record.terminal() {
			err := j.save([]string{record.ID}, "")
			j.mu.Unlock()
			if err != nil {
				slog.Error("job deletion save failed", "jobID", record.ID, "error", err)
				respondJobError(w, 500, jobPersistence)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		finished, err := j.finishLocked(record, "cancelled", jobCancelled)
		j.mu.Unlock()
		s.releaseJobLease(record, record.DeviceID, record.Lease)
		if err != nil {
			respondJobError(w, 500, jobPersistence)
			return
		}
		record = finished
	} else {
		j.mu.Unlock()
	}
	respond(w, 200, record.Job)
}

func (s *Server) finishJob(id, status, code string) error {
	j := s.jobs
	j.mu.Lock()
	record, ok := j.records[id]
	if !ok || record.terminal() {
		j.mu.Unlock()
		return nil
	}
	_, err := j.finishLocked(record, status, code)
	j.mu.Unlock()
	s.releaseJobLease(record, record.DeviceID, record.Lease)
	return err
}

func (j *jobs) finishLocked(record storedJob, status, code string) (storedJob, error) {
	if cancel := j.active[record.ID]; cancel != nil {
		cancel()
		delete(j.active, record.ID)
	}
	now := time.Now()
	record.Status, record.ErrorCode, record.Error, record.FinishedAt = status, code, jobMessages[code], &now
	record.ClaimHash, record.Lease, record.Executing = "", client.Lease{}, false
	// Keep the job terminal in memory even if the durable store is unwritable.
	// A subsequent save or maintenance tick retries the transition.
	j.records[record.ID] = record
	err := j.save(nil, record.ID)
	if err != nil {
		j.dirty = true
		slog.Error("job completion save failed", "jobID", record.ID, "error", err)
	}
	return record, err
}

func (s *Server) releaseJobLease(record storedJob, device string, lease client.Lease) {
	if lease.Token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.releaseDevice(ctx, record.User, device, lease); err != nil {
		slog.Warn("job lease release failed", "jobID", record.ID, "error", err)
	}
}

// RunnerHTTP is mounted outside OIDC with a separate machine credential. Job
// ownership and the claim token constrain every request to its original payload.
func (s *Server) RunnerHTTP(w http.ResponseWriter, r *http.Request) {
	j := s.jobs
	if j == nil || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+j.runnerToken)) != 1 || r.Header.Get("Origin") != "" {
		respond(w, 401, map[string]string{"error": "invalid runner credentials"})
		return
	}
	id, operation := r.PathValue("id"), r.PathValue("operation")
	j.mu.Lock()
	record, ok := j.records[id]
	if !ok {
		j.mu.Unlock()
		respond(w, 404, map[string]string{"error": "unknown job"})
		return
	}
	if operation == "claim" {
		if record.Status != "queued" {
			j.mu.Unlock()
			respond(w, 409, map[string]string{"error": "job already claimed or finished"})
			return
		}
		token := jobID()
		now := time.Now().UTC()
		if !fitsJobClaim(record.Job) {
			_, err := j.finishLocked(record, "failed", jobPayloadTooLarge)
			j.mu.Unlock()
			if err != nil {
				respondJobError(w, 500, jobPersistence)
				return
			}
			rejectJobSize(w)
			return
		}
		record.Status, record.ClaimHash, record.Heartbeat, record.StartedAt, record.Published = "running", claimHash(token), now, &now, true
		if err := j.put(record); err != nil {
			j.mu.Unlock()
			respondJobError(w, 500, jobPersistence)
			return
		}
		j.mu.Unlock()
		respond(w, 200, jobClaim{Job: record.Job, ClaimToken: token})
		return
	}
	if record.Status != "running" || subtle.ConstantTimeCompare([]byte(record.ClaimHash), []byte(claimHash(r.Header.Get("X-Job-Token")))) != 1 {
		j.mu.Unlock()
		respond(w, 409, map[string]string{"error": "job is no longer owned by this runner"})
		return
	}
	if operation == "heartbeat" {
		record.Heartbeat = time.Now()
		err := j.put(record)
		j.mu.Unlock()
		if err != nil {
			respondJobError(w, 500, jobPersistence)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	j.mu.Unlock()
	switch operation {
	case "occupy":
		s.occupyJob(w, r, record)
	case "step":
		s.stepJob(w, r, record)
	case "finish":
		status, code := "succeeded", ""
		if record.Completed != len(record.Steps) {
			status, code = "failed", jobInterrupted
		}
		if err := s.finishJob(id, status, code); err != nil {
			respondJobError(w, 500, jobPersistence)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	default:
		respond(w, 404, map[string]string{"error": "unknown runner operation"})
	}
}

func (s *Server) occupyJob(w http.ResponseWriter, r *http.Request, record storedJob) {
	j := s.jobs
	j.mu.Lock()
	record = j.records[record.ID]
	if record.Status != "running" || record.DeviceID != "" || record.Executing {
		j.mu.Unlock()
		respond(w, 409, map[string]string{"error": "job already assigned or assignment is in progress"})
		return
	}
	ctx, cancel := context.WithDeadline(j.ctx, record.StartedAt.Add(time.Duration(record.TimeoutSeconds)*time.Second))
	j.active[record.ID] = cancel
	record.Executing = true
	if err := j.put(record); err != nil {
		delete(j.active, record.ID)
		cancel()
		j.mu.Unlock()
		respondJobError(w, 500, jobPersistence)
		return
	}
	j.mu.Unlock()
	// Request cancellation also prevents a lost assignment response from leaving a live job.
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	var assignment client.Assignment
	var status int
	var err error
	for {
		assignment, status, err = s.allocate(ctx, record.User, record.Selection, record.TimeoutSeconds+jobLeaseGraceSeconds)
		if err == nil || status != 409 {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = errors.New("job timed out while waiting for an available device")
			status = 408
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		code := jobAllocation
		if status == 408 || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, status = jobTimedOut, 408
		}
		slog.Error("job assignment failed", "jobID", record.ID, "error", err)
		_ = s.finishJob(record.ID, "failed", code)
		respondJobError(w, status, code)
		return
	}
	j.mu.Lock()
	latest := j.records[record.ID]
	if latest.Status != "running" {
		j.mu.Unlock()
		s.releaseJobLease(record, assignment.Assigned, assignment.Lease)
		respond(w, 409, map[string]string{"error": "job ended during allocation"})
		return
	}
	latest.DeviceID, latest.Lease, latest.Executing = assignment.Assigned, assignment.Lease, false
	err = j.put(latest)
	j.mu.Unlock()
	if err != nil {
		s.releaseJobLease(record, assignment.Assigned, assignment.Lease)
		_ = s.finishJob(record.ID, "failed", jobPersistence)
		respondJobError(w, 500, jobPersistence)
		return
	}
	respond(w, 200, map[string]any{"deviceId": assignment.Assigned, "device": assignment.Device})
}

func (s *Server) stepJob(w http.ResponseWriter, r *http.Request, record storedJob) {
	var in struct {
		Index int `json:"index"`
	}
	if err := decode(w, r, &in); err != nil {
		respond(w, 400, map[string]string{"error": "invalid step index"})
		return
	}
	j := s.jobs
	j.mu.Lock()
	record = j.records[record.ID]
	if in.Index >= 0 && in.Index < record.Completed {
		result := record.Results[in.Index]
		j.mu.Unlock()
		respond(w, 200, result)
		return
	}
	if record.Status != "running" || record.DeviceID == "" || record.Executing || in.Index != record.Completed || in.Index >= len(record.Steps) {
		j.mu.Unlock()
		respond(w, 409, map[string]string{"error": "step is out of order or already executing"})
		return
	}
	record.Executing = true
	if err := j.put(record); err != nil {
		j.mu.Unlock()
		respondJobError(w, 500, jobPersistence)
		return
	}
	ctx, cancel := context.WithDeadline(j.ctx, record.StartedAt.Add(time.Duration(record.TimeoutSeconds)*time.Second))
	previous := j.active[record.ID]
	j.active[record.ID] = cancel
	if previous != nil {
		previous()
	}
	j.mu.Unlock()
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	step := record.Steps[in.Index]
	var data []byte
	var err error
	status := 200
	screenshot := step["action"] == "screenshot"
	if screenshot {
		data, status, err = s.deviceRequest(ctx, record.User, record.DeviceID, "GET", "screenshot", record.Lease.Token, nil)
	} else {
		data, status, err = s.deviceRequest(ctx, record.User, record.DeviceID, "POST", "actions", record.Lease.Token, step)
		if err == nil {
			var result struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err = json.Unmarshal(data, &result); err == nil && (!result.OK || result.Error != "") {
				err = fmt.Errorf("device action failed: %s", result.Error)
			}
		}
	}
	if err != nil {
		slog.Error("job step failed", "jobID", record.ID, "index", in.Index, "error", err)
		_ = s.finishJob(record.ID, "failed", jobStep)
		if status < 400 {
			status = 502
		}
		respondJobError(w, status, jobStep)
		return
	}
	j.mu.Lock()
	latest := j.records[record.ID]
	if latest.Status != "running" {
		j.mu.Unlock()
		respond(w, 409, map[string]string{"error": "job ended during step"})
		return
	}
	if screenshot {
		if err := j.storeArtifact(latest, in.Index, data); err != nil {
			j.mu.Unlock()
			status, code := 500, jobArtifactFailure
			if errors.Is(err, errJobArtifactLimit) {
				status, code = http.StatusInsufficientStorage, jobArtifactLimit
			}
			slog.Error("job artifact save failed", "jobID", record.ID, "index", in.Index, "error", err)
			_ = s.finishJob(record.ID, "failed", code)
			respondJobError(w, status, code)
			return
		}
		data, _ = json.Marshal(map[string]string{"artifact": fmt.Sprintf("/api/jobs/%s/artifacts/%d", record.ID, in.Index)})
	}
	latest.Completed++
	latest.Executing = false
	// Keep results bounded; large UI responses can still be fetched with the normal device API.
	if len(data) <= maxJobResultJSON {
		data, _ = json.Marshal(json.RawMessage(data))
	}
	if len(data) > maxJobResultJSON {
		data = json.RawMessage(`{"omitted":true,"reason":"result exceeds 64 KiB"}`)
	}
	latest.Results = append(append([]json.RawMessage{}, latest.Results...), json.RawMessage(data))
	err = j.put(latest)
	if err != nil && screenshot {
		j.removeArtifact(record.ID, in.Index)
	}
	j.mu.Unlock()
	if err != nil {
		_ = s.finishJob(record.ID, "failed", jobPersistence)
		respondJobError(w, 500, jobPersistence)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *Server) jobArtifact(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		http.NotFound(w, r)
		return
	}
	user, _ := identity.FromContext(r.Context())
	index, err := strconv.Atoi(r.PathValue("index"))
	j := s.jobs
	j.mu.Lock()
	record, ok := j.records[r.PathValue("id")]
	if !ok || record.User.Subject != user.Subject && !identity.IsAdmin(r.Context()) || err != nil || index < 0 || index >= record.Completed || record.Steps[index]["action"] != "screenshot" {
		j.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if _, tracked := j.artifacts[record.ID].sizes[index]; !tracked {
		j.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(j.artifactPath(record.ID, index))
	j.mu.Unlock()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
