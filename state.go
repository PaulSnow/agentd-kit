package main

import (
	"fmt"
	"time"
)

// State is a session's state. Every transition is explained by evidence.
type State string

const (
	Starting       State = "starting"
	Working        State = "working"
	Idle           State = "idle"
	BlockedOnOwner State = "blocked-on-owner"
	UsageLimited   State = "usage-limited"
	Done           State = "done"
	Failed         State = "failed"
	CrashLooping   State = "crash-looping"
)

// Track is what the supervisor remembers about one session between passes.
// It is rebuilt from the event log at start, so the log is the only state.
type Track struct {
	State State
	Since time.Time

	Hash      string    // last pane hash seen
	HashSince time.Time // when the pane last changed, nudge echoes excluded
	Observed  bool      // at least one observation

	Restarts  []time.Time // restart times by agentd
	Breaker   bool        // the crash-loop breaker has tripped; cleared when the session is seen alive
	Nudges    int         // nudges in the current idle episode
	LastNudge time.Time
	Limited   bool

	Posted    map[string]bool   // escalation keys already posted
	Delivered map[string]string // delivery keys: MR reference or the reason none was made
	Skipped   map[string]bool   // skip reasons already logged
	LastObs   map[string]any    // last observe details, to log only changes
}

func newTrack() *Track {
	return &Track{Posted: map[string]bool{}, Delivered: map[string]string{}, Skipped: map[string]bool{}}
}

// restartsInWindow counts restarts inside the breaker window.
func (t *Track) restartsInWindow(now time.Time, window time.Duration) int {
	n := 0
	for _, at := range t.Restarts {
		if now.Sub(at) < window {
			n++
		}
	}
	return n
}

// lastRestart is the time of the latest restart, or zero.
func (t *Track) lastRestart() time.Time {
	if len(t.Restarts) == 0 {
		return time.Time{}
	}
	return t.Restarts[len(t.Restarts)-1]
}

// backoff is the wait before restart number n (0-based) within the window:
// none for the first, then initial doubled each time, capped.
func backoff(n int, p RestartPolicy) time.Duration {
	if n <= 0 {
		return 0
	}
	d := p.BackoffInitial.Duration
	for i := 1; i < n; i++ {
		d *= 2
		if d >= p.BackoffMax.Duration {
			return p.BackoffMax.Duration
		}
	}
	if d > p.BackoffMax.Duration {
		return p.BackoffMax.Duration
	}
	return d
}

// noteHash records the pane hash and decides whether a change is progress.
// After a nudge, a change is the nudge's echo rather than progress when it
// comes within the settle time, or when the transcript shows no turn since
// the nudge; an echo keeps the idle clock running. Any other change restarts
// the clock and the nudge count.
func (t *Track) noteHash(hash string, now time.Time, p NudgePolicy, tr TranscriptEvidence) {
	if !t.Observed {
		t.Hash, t.HashSince, t.Observed = hash, now, true
		return
	}
	if hash == t.Hash {
		return
	}
	t.Hash = hash
	if t.Nudges > 0 && !t.LastNudge.IsZero() {
		if now.Sub(t.LastNudge) <= p.Settle.Duration {
			return
		}
		if tr.Found && !tr.At.After(t.LastNudge) {
			return
		}
	}
	t.HashSince = now
	t.Nudges = 0
}

// Classify derives the state from evidence and memory. It changes nothing
// but the pane memory. The reason names the evidence that decided.
func Classify(t *Track, ev Evidence, pol Policies, now time.Time) (State, string) {
	switch {
	case ev.Status.Done:
		return Done, "STATUS.md ends with DONE"
	case !ev.Alive:
		if t.Breaker {
			return CrashLooping, "crash-loop breaker tripped; waiting for the owner"
		}
		if pol.Restart.Enabled && t.restartsInWindow(now, pol.Restart.Window.Duration) >= pol.Restart.MaxRestarts {
			t.Breaker = true
			return CrashLooping, fmt.Sprintf("tmux session gone after %d restarts in %s", pol.Restart.MaxRestarts, pol.Restart.Window)
		}
		return Failed, "tmux session gone and STATUS.md not DONE"
	}
	t.Breaker = false
	first := !t.Observed
	t.noteHash(ev.PaneHash, now, pol.Nudge, ev.Transcript)
	switch {
	case ev.Limited:
		return UsageLimited, "screen shows the usage-limit message"
	case ev.TrustPrompt:
		return Starting, "screen shows the folder-trust prompt"
	case ev.Status.Questions != "":
		return BlockedOnOwner, "STATUS.md has open Questions"
	}
	if first {
		return Starting, "first observation"
	}
	unchanged := now.Sub(t.HashSince)
	if pol.Nudge.Enabled && unchanged >= pol.Nudge.Stall.Duration {
		if ev.Transcript.Found && now.Sub(ev.Transcript.At) < pol.Nudge.Stall.Duration {
			return Working, fmt.Sprintf("screen unchanged for %s but the transcript moved %s ago", unchanged.Round(time.Minute), now.Sub(ev.Transcript.At).Round(time.Minute))
		}
		return Idle, fmt.Sprintf("screen unchanged for %s", unchanged.Round(time.Minute))
	}
	return Working, "screen changed recently"
}
