package backend

import (
	"fmt"
	"os"
	"strconv"
	"sync"
)

type videoLimits struct {
	mu                   sync.Mutex
	maxTotal, maxAccount int
	total                int
	subjects, leases     map[string]int
}

func positiveIntEnv(name string, fallback, maxValue int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || value > maxValue {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", name, maxValue)
	}
	return value, nil
}

func (v *videoLimits) configure() error {
	global, err := positiveIntEnv("MARIONETTE_VIDEO_MAX_GLOBAL", 128, 1<<20)
	if err != nil {
		return err
	}
	account, err := positiveIntEnv("MARIONETTE_VIDEO_MAX_PER_ACCOUNT", 4, 1<<20)
	if err != nil {
		return err
	}
	v.maxTotal, v.maxAccount = int(global), int(account)
	v.subjects, v.leases = make(map[string]int), make(map[string]int)
	return nil
}

func (v *videoLimits) reserve(subject, lease string) (func(), bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.total >= v.maxTotal || v.subjects[subject] >= v.maxAccount || lease != "" && v.leases[lease] >= v.maxAccount {
		return nil, false
	}
	v.total++
	v.subjects[subject]++
	if lease != "" {
		v.leases[lease]++
	}
	return func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.total--
		decrement(v.subjects, subject)
		if lease != "" {
			decrement(v.leases, lease)
		}
	}, true
}

func decrement(counts map[string]int, key string) {
	counts[key]--
	if counts[key] == 0 {
		delete(counts, key)
	}
}
