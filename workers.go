package main

import (
	"sync"
	"time"
)

// WakeCheck is how often a running agentd looks for owner records that
// another process appended (agentd clear, mark-delivered, an answer), so
// one is acted on within a minute rather than at the next poll.
const WakeCheck = 15 * time.Second

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
	err      error         // the first event a worker could not append
	wake     chan struct{} // signalled when a job ends or the owner wrote to the log
}

// workerMsg is an event a worker appended, or the end of its job.
type workerMsg struct {
	event Event
	done  string // the job's key, when the job ended
}

func newWorkState() *workState {
	return &workState{inFlight: map[string]bool{}, wake: make(chan struct{}, 1)}
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
	if s.work.inFlight[key] {
		return false
	}
	s.work.inFlight[key] = true
	w := *s
	w.worker = true
	w.halt = nil
	w.st = &LogState{Mode: s.st.Mode, Tracks: map[string]*Track{name: s.track(name).clone()}, Pending: map[string]Event{}, cfg: s.cfg}
	s.work.jobs.Add(1)
	go func() {
		defer s.work.jobs.Done()
		job(&w, w.st.track(name))
		s.work.mu.Lock()
		s.work.queue = append(s.work.queue, workerMsg{done: key})
		s.work.mu.Unlock()
		s.work.signal()
	}()
	return true
}

// drain applies what the workers queued, in the order they appended it,
// and forgets the jobs that ended. Only the loop drains.
func (s *Supervisor) drain() {
	s.work.mu.Lock()
	q := s.work.queue
	s.work.queue = nil
	s.work.mu.Unlock()
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
	c.Failures = make(map[string][]time.Time, len(t.Failures))
	for k, v := range t.Failures {
		c.Failures[k] = append([]time.Time(nil), v...)
	}
	c.DeliveredRefs = append([]string(nil), t.DeliveredRefs...)
	c.GaveUp = append([]string(nil), t.GaveUp...)
	c.CutShort = append([]string(nil), t.CutShort...)
	c.Deliveries = append([]DeliveryRecord(nil), t.Deliveries...)
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
