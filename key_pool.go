package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

// MaskKey hides the sensitive center of an API key for safe UI and log display.
func MaskKey(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 10 {
		return "****"
	}
	return fmt.Sprintf("%s...%s", k[:6], k[len(k)-4:])
}

// KeyEntry represents one Gemini API key in the load-balanced pool.
type KeyEntry struct {
	Key             string    `json:"-"`
	MaskedKey       string    `json:"masked_key"`
	Index           int       `json:"index"`
	CoolUntil       time.Time `json:"cool_until"`
	CoolDurationSec int       `json:"cool_duration_sec"`
	TotalCalls      int       `json:"total_calls"`
	Failures        int       `json:"failures"`
}

// IsHealthy returns true if the key is not currently in a rate-limit cooldown.
func (e *KeyEntry) IsHealthy() bool {
	return time.Now().After(e.CoolUntil)
}

// KeyPool manages multiple API keys with thread-safe round-robin selection and rate-limit cooldown.
type KeyPool struct {
	mu      sync.Mutex
	entries []*KeyEntry
	curr    int
}

// NewKeyPool initializes a pool from a slice of API key strings.
func NewKeyPool(rawKeys []string) *KeyPool {
	pool := &KeyPool{
		entries: make([]*KeyEntry, 0, len(rawKeys)),
	}

	seen := make(map[string]bool)
	idx := 0
	for _, raw := range rawKeys {
		k := strings.TrimSpace(raw)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		pool.entries = append(pool.entries, &KeyEntry{
			Key:       k,
			MaskedKey: MaskKey(k),
			Index:     idx,
		})
		idx++
	}

	log.Printf("[KeyPool] Initialized pool with %d API keys (Combined capacity: %d RPM / %d Daily Calls at $0.00)",
		len(pool.entries), len(pool.entries)*15, len(pool.entries)*1500)

	return pool
}

// HasKeys returns true if the pool has at least one key configured.
func (p *KeyPool) HasKeys() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries) > 0
}

// Size returns the total number of keys in the pool.
func (p *KeyPool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// NextKey selects the next available healthy key via round-robin.
// If all keys are cooling down, it returns the key closest to cooldown expiry.
func (p *KeyPool) NextKey() (string, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.entries)
	if n == 0 {
		return "", -1, errors.New("no Gemini API keys configured in pool")
	}

	now := time.Now()

	// Try round-robin starting at p.curr
	for i := 0; i < n; i++ {
		idx := (p.curr + i) % n
		entry := p.entries[idx]
		if now.After(entry.CoolUntil) {
			p.curr = (idx + 1) % n
			entry.TotalCalls++
			return entry.Key, entry.Index, nil
		}
	}

	// If all are cooling down, pick the one with earliest cooldown expiry
	bestIdx := 0
	earliest := p.entries[0].CoolUntil
	for i := 1; i < n; i++ {
		if p.entries[i].CoolUntil.Before(earliest) {
			earliest = p.entries[i].CoolUntil
			bestIdx = i
		}
	}

	entry := p.entries[bestIdx]
	p.curr = (bestIdx + 1) % n
	entry.TotalCalls++
	log.Printf("[KeyPool] ⚠️ All %d keys currently in cooldown; using key #%d (%s, expires in %v)",
		n, entry.Index+1, entry.MaskedKey, time.Until(earliest).Round(time.Second))
	return entry.Key, entry.Index, nil
}

// ReportRateLimit flags a key for rate limit backoff (45 seconds cooldown).
func (p *KeyPool) ReportRateLimit(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, entry := range p.entries {
		if entry.Key == key {
			entry.CoolUntil = time.Now().Add(45 * time.Second)
			entry.CoolDurationSec = 45
			entry.Failures++
			log.Printf("[KeyPool] ⚠️ Rate limit reported for key #%d (%s). Cooling down for 45s (Failures: %d)",
				entry.Index+1, entry.MaskedKey, entry.Failures)
			return
		}
	}
}

// ReportPermanentFailure disables a key that is suspended or invalid.
func (p *KeyPool) ReportPermanentFailure(key string, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, entry := range p.entries {
		if entry.Key == key {
			entry.CoolUntil = time.Now().Add(365 * 24 * time.Hour)
			entry.Failures += 10
			log.Printf("[KeyPool] 🚫 Key #%d (%s) disabled permanently: %s",
				entry.Index+1, entry.MaskedKey, reason)
			return
		}
	}
}

// ReportSuccess acknowledges successful usage of a key.
func (p *KeyPool) ReportSuccess(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, entry := range p.entries {
		if entry.Key == key {
			entry.Failures = 0
			return
		}
	}
}

// Stats returns a summary map suitable for JSON serialization to UI clients.
func (p *KeyPool) Stats() map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	total := len(p.entries)
	healthy := 0
	cooling := 0
	now := time.Now()

	keyList := make([]map[string]interface{}, 0, total)
	for _, e := range p.entries {
		isH := now.After(e.CoolUntil)
		secondsLeft := 0
		if !isH {
			rem := time.Until(e.CoolUntil).Seconds()
			if rem > 0 {
				secondsLeft = int(math.Ceil(rem))
			} else {
				isH = true
			}
		}
		if isH {
			healthy++
		} else {
			cooling++
		}
		duration := e.CoolDurationSec
		if duration <= 0 {
			duration = 45
		}
		keyList = append(keyList, map[string]interface{}{
			"index":             e.Index + 1,
			"masked_key":        e.MaskedKey,
			"is_healthy":        isH,
			"seconds_left":      secondsLeft,
			"cooldown_duration": duration,
			"total_calls":       e.TotalCalls,
			"failures":          e.Failures,
		})
	}

	// Calculate seconds until next daily quota refresh (Midnight Pacific Time)
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.FixedZone("PST", -8*3600)
	}
	nowInLoc := now.In(loc)
	nextMidnight := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day()+1, 0, 0, 0, 0, loc)
	secondsUntilReset := int(nextMidnight.Sub(nowInLoc).Seconds())
	if secondsUntilReset < 0 {
		secondsUntilReset = 0
	}

	healthPct := 100
	if total > 0 {
		healthPct = int(math.Round(float64(healthy) / float64(total) * 100))
	}

	return map[string]interface{}{
		"total_keys":          total,
		"healthy_keys":        healthy,
		"cooling_keys":        cooling,
		"health_pct":          healthPct,
		"quota_reset_seconds": secondsUntilReset,
		"quota_reset_iso":     nextMidnight.Format(time.RFC3339),
		"rpm_capacity":        total * 15,
		"daily_capacity":      total * 1500,
		"cost_tier":           "$0.00 Free Tier",
		"keys":                keyList,
	}
}
