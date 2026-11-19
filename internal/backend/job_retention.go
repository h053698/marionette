package backend

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxJobStoreBytes   = 128 << 20
	maxJobStoreRecords = 4096
	jobBytesHighWater  = 112 << 20
	jobCountHighWater  = 3840
)

var errJobArtifactLimit = errors.New("job artifact quota exceeded")

type jobArtifacts struct {
	subject string
	sizes   map[int]int64
}

func (r storedJob) terminal() bool {
	return r.Status == "succeeded" || r.Status == "failed" || r.Status == "cancelled"
}

func (r storedJob) terminalTime() time.Time {
	if r.FinishedAt == nil {
		return r.CreatedAt
	}
	return *r.FinishedAt
}

// Reserve space for outstanding results and assignment metadata at admission.
// Otherwise active jobs alone could grow beyond the store limit as they finish.
func (j *jobs) hasJobCapacity(newJob storedJob) bool {
	var active, account, bytes int
	reserve := func(r storedJob) bool {
		active++
		if r.User.Subject == newJob.User.Subject {
			account++
		}
		data, err := json.Marshal(r)
		bytes += len(data) + (len(r.Steps)-r.Completed)*(maxJobResultJSON+1) + 4096
		return err == nil && account <= 20 && active <= jobCountHighWater && bytes <= jobBytesHighWater
	}
	if !reserve(newJob) {
		return false
	}
	for _, r := range j.records {
		if !r.terminal() && !reserve(r) {
			return false
		}
	}
	return true
}

func (j *jobs) oldestTerminal() []storedJob {
	var terminal []storedJob
	for _, r := range j.records {
		if r.terminal() {
			terminal = append(terminal, r)
		}
	}
	sort.Slice(terminal, func(i, k int) bool {
		if cmp := terminal[i].terminalTime().Compare(terminal[k].terminalTime()); cmp != 0 {
			return cmp < 0
		}
		return terminal[i].ID < terminal[k].ID
	})
	return terminal
}

// save commits record removals before touching artifacts. A failed write rolls
// back eviction and leaves artifact files and readers intact. Call under mu.
func (j *jobs) save(remove []string, keep string) error {
	removed := make(map[string]storedJob)
	drop := func(id string) {
		if r, ok := j.records[id]; ok && r.terminal() && id != keep {
			removed[id] = r
			delete(j.records, id)
		}
	}
	for _, id := range remove {
		drop(id)
	}
	data, err := json.Marshal(j.records)
	if err == nil && (len(data)+1 > jobBytesHighWater || len(j.records) > jobCountHighWater) {
		size := len(data) + 1
		for _, r := range j.oldestTerminal() {
			if r.ID == keep {
				continue
			}
			entry, _ := json.Marshal(r)        // The full map already marshaled successfully.
			size -= len(entry) + len(r.ID) + 4 // JSON key, colon, and comma.
			drop(r.ID)
			if size <= jobBytesHighWater && len(j.records) <= jobCountHighWater {
				break
			}
		}
		if size != len(data)+1 {
			data, err = json.Marshal(j.records)
		}
	}
	if err == nil {
		if len(data)+1 > maxJobStoreBytes || len(j.records) > maxJobStoreRecords {
			err = errors.New("job store capacity exhausted")
		} else {
			err = j.writeStore(data)
		}
	}
	if err != nil {
		for id, r := range removed {
			j.records[id] = r
		}
		return err
	}
	j.dirty = false
	for id := range removed {
		for index := range j.artifacts[id].sizes {
			j.removeArtifact(id, index)
		}
	}
	return nil
}

func (j *jobs) maintain() {
	tick := time.NewTicker(max(time.Second, min(j.retention, time.Minute)))
	defer tick.Stop()
	for {
		select {
		case <-j.ctx.Done():
			return
		case now := <-tick.C:
			j.mu.Lock()
			err := j.prune(now, false)
			j.mu.Unlock()
			if err != nil {
				slog.Error("job store maintenance failed", "error", err)
			}
		}
	}
}

func (j *jobs) prune(now time.Time, force bool) error {
	// Clear interrupted writes and retry old removals before judging quota pressure.
	for id, artifacts := range j.artifacts {
		r, ok := j.records[id]
		for index := range artifacts.sizes {
			if !ok || index >= r.Completed || r.Steps[index]["action"] != "screenshot" {
				j.removeArtifact(id, index)
			}
		}
	}
	usage := make(map[string]int64)
	for _, artifacts := range j.artifacts {
		usage[artifacts.subject] += artifacts.bytes()
	}
	var remove []string
	for _, r := range j.oldestTerminal() {
		subject := r.User.Subject
		bytes := j.artifacts[r.ID].bytes()
		if !r.terminalTime().Add(j.retention).After(now) || bytes > 0 && usage[subject] > j.artifactLimit {
			remove = append(remove, r.ID)
			usage[subject] -= bytes
		}
	}
	if force || j.dirty || len(remove) != 0 {
		return j.save(remove, "")
	}
	return nil
}

func (j *jobs) artifactDir() string { return filepath.Join(filepath.Dir(j.file), "artifacts") }

func (j *jobs) artifactPath(id string, index int) string {
	return filepath.Join(j.artifactDir(), id+"-"+strconv.Itoa(index)+".png")
}

func (j *jobs) loadArtifacts() error {
	entries, err := os.ReadDir(j.artifactDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || len(name) < 38 || name[32] != '-' || !strings.HasSuffix(name, ".png") {
			continue
		}
		id, suffix := name[:32], name[33:len(name)-4]
		_, decodeErr := base64.RawURLEncoding.Strict().DecodeString(id)
		index, parseErr := strconv.Atoi(suffix)
		if decodeErr != nil || parseErr != nil || index < 0 || index >= 100 || strconv.Itoa(index) != suffix {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		j.trackArtifact(id, j.records[id].User.Subject, index, info.Size())
	}
	return nil
}

func (j *jobs) trackArtifact(id, subject string, index int, size int64) {
	artifacts, ok := j.artifacts[id]
	if !ok {
		artifacts = jobArtifacts{subject: subject, sizes: make(map[int]int64)}
		j.artifacts[id] = artifacts
	}
	artifacts.sizes[index] = size
}

func (j *jobs) removeArtifact(id string, index int) {
	if err := os.Remove(j.artifactPath(id, index)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("job artifact removal failed", "jobID", id, "index", index, "error", err)
		return
	}
	sizes := j.artifacts[id].sizes
	delete(sizes, index)
	if len(sizes) == 0 {
		delete(j.artifacts, id)
	}
}

func (a jobArtifacts) bytes() int64 {
	var total int64
	for _, size := range a.sizes {
		total += size
	}
	return total
}

func (j *jobs) artifactUsage(subject string) int64 {
	var total int64
	for _, artifacts := range j.artifacts {
		if artifacts.subject == subject {
			total += artifacts.bytes()
		}
	}
	return total
}

// storeArtifact runs under the same lock as cancellation and step persistence.
func (j *jobs) storeArtifact(r storedJob, index int, data []byte) error {
	size := int64(len(data))
	if size > j.artifactLimit {
		return errJobArtifactLimit
	}
	subject := r.User.Subject
	usage := size + j.artifactUsage(subject)
	if usage > j.artifactLimit {
		var remove []string
		for _, terminal := range j.oldestTerminal() {
			if bytes := j.artifacts[terminal.ID].bytes(); bytes > 0 && terminal.User.Subject == subject {
				remove = append(remove, terminal.ID)
				usage -= bytes
				if usage <= j.artifactLimit {
					break
				}
			}
		}
		if usage > j.artifactLimit {
			return errJobArtifactLimit
		}
		if err := j.save(remove, ""); err != nil {
			return fmt.Errorf("pruning jobs for artifact quota: %w", err)
		}
		// A failed file removal does not free any quota. Maintenance retries it.
		if size+j.artifactUsage(subject) > j.artifactLimit {
			return errJobArtifactLimit
		}
	}
	if err := os.MkdirAll(j.artifactDir(), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(j.artifactPath(r.ID, index), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	written, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	j.trackArtifact(r.ID, subject, index, int64(written))
	if err != nil {
		j.removeArtifact(r.ID, index)
	}
	return err
}
