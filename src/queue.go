package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

type mediaLimits struct {
	concurrency, queuePerUser, perHour, perDay int
}

func mediaLimitsFromEnv() (mediaLimits, error) {
	limits := mediaLimits{3, 20, 100, 600}
	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"MEDIA_CONCURRENCY", &limits.concurrency},
		{"MEDIA_USER_QUEUE_LIMIT", &limits.queuePerUser},
		{"MEDIA_USER_HOURLY_LIMIT", &limits.perHour},
		{"MEDIA_USER_DAILY_LIMIT", &limits.perDay},
	} {
		if raw := os.Getenv(setting.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				return mediaLimits{}, fmt.Errorf("%s must be a positive integer", setting.name)
			}
			*setting.value = value
		}
	}
	return limits, nil
}

var (
	errMediaQueueFull   = errors.New("your queue is full")
	errMediaHourlyLimit = errors.New("you reached your hourly link limit")
	errMediaDailyLimit  = errors.New("you reached your daily link limit")
)

type mediaUsage struct {
	queued  int
	started []time.Time
}

type mediaJob struct {
	userID int64
	work   func() error
	result chan error
}

type mediaQueue struct {
	mu          sync.Mutex
	users       map[int64]*mediaUsage
	limits      mediaLimits
	running     int
	waiting     []mediaJob
	lastCleanup time.Time
}

func newMediaQueue(limits mediaLimits) *mediaQueue {
	return &mediaQueue{limits: limits, users: make(map[int64]*mediaUsage)}
}

func (q *mediaQueue) submit(userID int64, work func() error) (<-chan error, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	q.cleanupUsers(now)
	usage := q.users[userID]
	if usage == nil {
		usage = &mediaUsage{}
		q.users[userID] = usage
	}
	if err := usage.checkLimits(now, q.limits); err != nil {
		return nil, err
	}
	if q.running >= q.limits.concurrency && usage.queued >= q.limits.queuePerUser {
		return nil, errMediaQueueFull
	}

	job := mediaJob{userID: userID, work: work, result: make(chan error, 1)}
	if q.running < q.limits.concurrency {
		q.running++
		usage.started = append(usage.started, now)
		go q.run(job)
	} else {
		usage.queued++
		q.waiting = append(q.waiting, job)
	}
	return job.result, nil
}

func (q *mediaQueue) cleanupUsers(now time.Time) {
	if now.Sub(q.lastCleanup) < time.Hour {
		return
	}
	cutoff := now.Add(-24 * time.Hour)
	for id, usage := range q.users {
		if usage.queued == 0 && (len(usage.started) == 0 || !usage.started[len(usage.started)-1].After(cutoff)) {
			delete(q.users, id)
		}
	}
	q.lastCleanup = now
}

func (usage *mediaUsage) checkLimits(now time.Time, limits mediaLimits) error {
	dayAgo := now.Add(-24 * time.Hour)
	first := 0
	for first < len(usage.started) && !usage.started[first].After(dayAgo) {
		first++
	}
	usage.started = usage.started[first:]
	// Waiting jobs reserve quota until they start, even across window boundaries.
	if len(usage.started)+usage.queued >= limits.perDay {
		return errMediaDailyLimit
	}

	hourAgo := now.Add(-time.Hour)
	hourly := 0
	for _, started := range usage.started {
		if started.After(hourAgo) {
			hourly++
		}
	}
	if hourly+usage.queued >= limits.perHour {
		return errMediaHourlyLimit
	}
	return nil
}

func (q *mediaQueue) run(job mediaJob) {
	for {
		result, err := job.result, job.work()
		q.mu.Lock()
		if len(q.waiting) == 0 {
			q.running--
			q.mu.Unlock()
			result <- err
			return
		}
		job = q.waiting[0]
		q.waiting[0] = mediaJob{}
		q.waiting = q.waiting[1:]
		usage := q.users[job.userID]
		usage.queued--
		usage.started = append(usage.started, time.Now())
		q.mu.Unlock()
		result <- err
	}
}
