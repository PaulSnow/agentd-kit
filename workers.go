package main

import (
	"sync"
	"time"
)

// WakeCheck is how often a running agentd looks for owner records that
// another process appended (agentd clear, mark-delivered), so one is acted
// on within a minute rather than at the next poll.
const WakeCheck = 15 * time.Second

// StopWait is how long a run that is told to stop waits for its running
// jobs. One still running then is cut short: the next start warns of it.
const StopWait = time.Minute

// workState is what the loop and its workers share. Slow work, a delivery
// (its gate and forge calls) or an escalation command, runs in a worker
// goroutine, so it never holds up the pass over the other sessions. A
// worker works on its own copy of its session's Track: it appends each
// event to the log as it happens, applies it to its copy so the job sees
// its own progress, and queues it for the loop, which applies it to the
// session's real Track at its next drain. The Track stays the loop's alone,
// and an outcome a worker appended survives a crash of agentd before the
// loop applied it, since replay applies it from the log.
type workState struct {
	mu       sync.Mutex
	queue    []workerMsg
	inFlight map[string]bool // job keys running now; the loop's alone
	jobs     sync.WaitGroup
	err      error                // the first event a worker could not append
	wake     chan struct{}        // signalled when the owner wrote to the log
	nextPoll map[string]time.Time // when each forge source is next due
	// pollTally holds each source's quiet polls since its last record.
	pollTally map[string]PollPayload
	// liveCursor is each source's cursor as far as polls have moved it,
	// ahead of its event.
	liveCursor map[string]time.Time
}

// workerMsg is an event a worker appended, or the end of its job.
type workerMsg struct {
	event Event
	done  string // the job's key, when the job ended
}

func newWorkState() *workState {
	return &workState{inFlight: map[string]bool{}, wake: make(chan struct{}, 1), nextPoll: map[string]time.Time{},
		pollTally: map[string]PollPayload{}, liveCursor: map[string]time.Time{}}
}

// signal wakes the loop, if it is not already due to wake.
func (w *workState) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// spawn runs job in a worker on a copy of the session's Track, unless a
// job with the same key is still running, and says whether it started it.
func (s *Supervisor) spawn(name, key string, job func(w *Supervisor, t *Track)) bool {
	st := &LogState{Mode: s.st.Mode, Tracks: map[string]*Track{name: s.track(name).clone()}, Pending: map[string]Event{}, cfg: s.cfg}
	return s.start(key, st, func(w *Supervisor) { job(w, w.st.track(name)) })
}

// spawnShared runs a job about no one session, a poll of the forges, in a
// worker whose state holds copies of the items, cursors and poll records,
// unless the same job is still running.
func (s *Supervisor) spawnShared(key string, job func(w *Supervisor)) bool {
	st := &LogState{Mode: s.st.Mode, Tracks: map[string]*Track{}, Pending: map[string]Event{}, cfg: s.cfg,
		Items: copyMap(s.st.Items), Cursors: copyMap(s.st.Cursors), Polls: copyMap(s.st.Polls)}
	return s.start(key, st, job)
}

// start runs job in a worker over st.
func (s *Supervisor) start(key string, st *LogState, job func(w *Supervisor)) bool {
	if s.work.inFlight[key] {
		return false
	}
	s.work.inFlight[key] = true
	w := *s
	w.worker = true
	w.halt = nil
	w.st = st
	s.work.jobs.Add(1)
	go func() {
		defer s.work.jobs.Done()
		defer func() {
			s.work.mu.Lock()
			s.work.queue = append(s.work.queue, workerMsg{done: key})
			s.work.mu.Unlock()
		}()
		job(&w)
	}()
	return true
}

// waitStop waits for the running jobs, up to StopWait.
func (s *Supervisor) waitStop() {
	done := make(chan struct{})
	go func() { s.work.jobs.Wait(); close(done) }()
	select {
	case <-done:
		s.drain()
	case <-s.clock.After(StopWait):
	}
}

// drain applies what the workers queued, in the order they appended it,
// and forgets the jobs that ended. Only the loop drains.
func (s *Supervisor) drain() {
	s.work.mu.Lock()
	defer s.work.mu.Unlock()
	s.drainLocked()
}

// drainLocked is drain with the workers' lock held.
func (s *Supervisor) drainLocked() {
	q := s.work.queue
	s.work.queue = nil
	for _, m := range q {
		if m.done != "" {
			delete(s.work.inFlight, m.done)
			continue
		}
		s.st.apply(m.event)
	}
}

// Wait waits for every running job, applies what they queued, and returns
// the first event a worker could not append, if any. Tests use it to make
// a pass whole; Run uses it before a --once run stops.
func (s *Supervisor) Wait() error {
	s.work.jobs.Wait()
	s.drain()
	s.work.mu.Lock()
	defer s.work.mu.Unlock()
	err := s.work.err
	s.work.err = nil
	return err
}

// clone copies a Track deeply enough that a worker's changes to its copy
// never reach the loop's.
func (t *Track) clone() *Track {
	c := *t
	c.Restarts = append([]time.Time(nil), t.Restarts...)
	c.Crashes = append([]Crash(nil), t.Crashes...)
	c.Posted = copyMap(t.Posted)
	c.Delivered = copyMap(t.Delivered)
	c.Skipped = copyMap(t.Skipped)
	c.Blocked = copyMap(t.Blocked)
	c.Filtered = copyMap(t.Filtered)
	c.Usage = copyMap(t.Usage)
	c.Answers = copyMap(t.Answers)
	c.Decisions = append([]Decision(nil), t.Decisions...)
	c.Failures = make(map[string][]time.Time, len(t.Failures))
	for k, v := range t.Failures {
		c.Failures[k] = append([]time.Time(nil), v...)
	}
	c.DeliveredRefs = append([]string(nil), t.DeliveredRefs...)
	c.GaveUp = append([]string(nil), t.GaveUp...)
	c.CutShort = append([]string(nil), t.CutShort...)
	c.Deliveries = append([]DeliveryRecord(nil), t.Deliveries...)
	c.Gated = append([]string(nil), t.Gated...)
	c.Escalated = append([]EscalationRecord(nil), t.Escalated...)
	c.Turns = append([]TurnRecord(nil), t.Turns...)
	c.TurnLines = copyMap(t.TurnLines)
	c.Wakes = append([]WakeRecord(nil), t.Wakes...) // a wake's spend is never changed in place
	if t.WakeBase != nil {
		b := *t.WakeBase
		c.WakeBase = &b
	}
	if t.LastObs != nil {
		o := *t.LastObs
		c.LastObs = &o
	}
	return &c
}

func copyMap[V any](m map[string]V) map[string]V {
	c := make(map[string]V, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
