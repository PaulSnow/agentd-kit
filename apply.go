package main

import (
	"sort"
	"strings"
	"time"
)

// LogState is what the event log says for one mode: each session's Track,
// the intents still waiting for their outcome, and the run's start and
// stop. apply is the only code that changes what the log decides, so
// replay, the live path (emit, then apply) and every view read the log the
// same way. The one exception is the pane memory a pass keeps between its
// own observations (Classify's noteHash), which the observe event it logs
// then records.
type LogState struct {
	Mode    string
	Tracks  map[string]*Track
	Pending map[string]Event // intents with no outcome yet, by intent id
	Started time.Time
	Stopped time.Time
	Events  int // events of this mode, and the owner's, applied

	cfg *Config // its policies, read as each event is applied
}

// NewLogState is the empty state of mode under the configuration's policies.
func NewLogState(cfg *Config, mode string) *LogState {
	return &LogState{Mode: mode, Tracks: map[string]*Track{}, Pending: map[string]Event{}, cfg: cfg}
}

// track returns the session's Track, creating it.
func (st *LogState) track(name string) *Track {
	t, ok := st.Tracks[name]
	if !ok {
		t = newTrack()
		st.Tracks[name] = t
	}
	return t
}

// Fold applies every event in order.
func (st *LogState) Fold(events []Event) *LogState {
	for _, e := range events {
		st.apply(e)
	}
	return st
}

// CutShort lists the intents with no outcome, oldest first: actions a
// crash of agentd cut short.
func (st *LogState) CutShort() []Event {
	out := make([]Event, 0, len(st.Pending))
	for _, e := range st.Pending {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// apply folds one event into the state. Events of the other mode are
// ignored; the owner's records count for both. In shadow mode a would event
// counts as the action taken, so the shadow keeps its own counters.
func (st *LogState) apply(e Event) {
	if e.Mode != st.Mode && e.Mode != ModeOwner && e.Mode != "" {
		return
	}
	st.Events++
	if e.Session == "" {
		switch e.Kind {
		case KindStart:
			st.Started, st.Stopped = e.Time, time.Time{}
		case KindStop:
			st.Stopped = e.Time
		}
		return
	}
	t := st.track(e.Session)
	switch e.Kind {
	case KindObserve:
		o := e.Observe
		t.LastSeen, t.Head, t.Reason = e.Time, o.Head, o.Reason
		if o.Alive {
			t.Live = true
			t.noteCommand(o.Command, st.cfg.Policies.PaneCommands, e.Time)
		}
		t.Observed = true
		if o.Alive {
			t.Hash = o.PaneHash
		}
		if o.PaneSince != nil && !o.PaneSince.Equal(t.HashSince) {
			t.HashSince = *o.PaneSince
			t.Nudges = 0
		}
		// The limit is remembered from a pass that found the session
		// usage-limited until the resume line is typed, not only while the
		// message shows: that is when it is typed.
		if State(o.State) == UsageLimited {
			t.Limited = true
		}
		if o.Dialog && !t.Dialog.Showing {
			t.Dialog.Episode++
		}
		t.Dialog.Showing = o.Dialog
		t.Questions.observeQuestions(o.Questions, o.Done)
		if !o.Questions || o.Done {
			t.Asked = ""
		}
		if o.State != "" && t.State == "" {
			t.State, t.Since = State(o.State), e.Time
		}
	case KindTransition:
		t.State, t.Since = State(e.Transition.To), e.Time
		t.Reason = e.Transition.Reason
	case KindCrash:
		c := Crash{Exit: -1, Screen: e.Crash.ScreenHash}
		if e.Crash.Exit != nil {
			c.Exit = *e.Crash.Exit
		}
		t.noteCrash(c, st.cfg.Policies.Restart.MaxCrashes)
	case KindClear:
		t.clear()
	case KindSkip:
		k := e.Skip
		if k.Delivered != "" {
			t.Delivered[k.Delivered] = k.Outcome
			t.DeliveredRefs = append(t.DeliveredRefs, k.Outcome)
			// Found already open or merged; failed and nothing deliver nothing.
			if k.Outcome != "failed" && k.Outcome != "nothing" {
				t.Deliveries = append(t.Deliveries, DeliveryRecord{Ref: k.Outcome, Repository: k.Repository, Delivered: k.Delivered, At: e.Time})
			}
		}
		if k.Filtered != "" {
			t.Filtered[k.Filtered] = true
		}
		if k.Escalation != "" {
			t.Posted[k.Escalation] = true // given up on
			t.GaveUp = append(t.GaveUp, k.Escalation)
			t.settleQuestions(k.QuestionsHash, k.Update, k.Episode, e.Time)
			t.settleDialog(k.Dialog, k.Episode, e.Time)
		}
		if k.Worktree != "" && k.Delivered == "" {
			t.Blocked[k.Worktree] = k.Reason
		}
		t.Skipped[k.Key] = true
	case KindError:
		t.Errors++
		if k := e.Error.Key; k != "" {
			t.Failures[k] = append(t.Failures[k], e.Time)
		}
		if a := e.Error.Act; a != nil {
			delete(st.Pending, a.Intent)
			if a.Action == ActRestart {
				t.Restarts = append(t.Restarts, e.Time)
			}
		}
	case KindIntent:
		if e.Act.Intent != "" {
			st.Pending[e.Act.Intent] = e
		}
	case KindAction, KindWould:
		delete(st.Pending, e.Act.Intent)
		if e.Kind == KindWould {
			if st.Mode != ModeShadow {
				return
			}
			t.Would++
		}
		t.applyAction(e.Act, e.Kind == KindAction, true, e.Time)
		if a := e.Act; (a.Action == ActEscalate || a.Action == ActUpdate) && strings.HasPrefix(a.Key, "questions:") && a.Episode == t.Questions.Episode {
			t.Asked, t.AskedAt = questionText(e.Session, a.Message), e.Time
			if t.Filtered[a.Key] {
				t.Asked = "" // the generic message, not the questions
			}
		}
	case KindWarning:
		// An intent with no outcome was cut short by a crash of agentd. It
		// is shown, and what the owner or the session may have seen is
		// counted as done, so it is never repeated: an escalation never
		// posts twice and a line is never typed twice.
		if a := e.Warning.Intent; a != nil {
			delete(st.Pending, a.Intent)
			t.CutShort = append(t.CutShort, a.Action+" "+a.Key)
			at := e.Time
			if e.Warning.IntentAt != nil {
				at = *e.Warning.IntentAt
			}
			switch a.Action {
			case ActEscalate, ActUpdate, ActNudge, ActResume, ActTrust:
				t.applyAction(a, false, false, at)
			}
		}
	}
}

// applyAction folds an action taken (or, in shadow mode, one that would
// have been). taken is false for a would event and for an action cut short,
// which started nothing that can be observed; counted is false for an
// action cut short, which the views list apart rather than count.
func (t *Track) applyAction(a *ActPayload, taken, counted bool, at time.Time) {
	n := 0
	if counted {
		n = 1
	}
	switch a.Action {
	case ActRestart:
		t.Restarts = append(t.Restarts, at)
		t.RestartCount += n
		if taken { // a would-be restart started nothing
			t.Hash, t.HashSince, t.Observed, t.Live = "", at, true, true
			t.Nudges, t.LastNudge, t.Limited = 0, time.Time{}, false
		}
	case ActNudge:
		t.Nudges++
		t.NudgeCount += n
		t.LastNudge = at
		if a.PaneHash != "" { // the screen after the nudge's echo
			t.Hash = a.PaneHash
		}
	case ActResume:
		t.Limited = false
	case ActTrust:
		t.TrustHash = a.PaneHash
	case ActEscalate, ActUpdate:
		t.Posted[a.Key] = true
		if a.Action == ActEscalate {
			t.Escalations += n
		}
		t.settleQuestions(a.QuestionsHash, a.Update, a.Episode, at)
		t.settleDialog(a.Dialog, a.Episode, at)
	case ActOpen:
		ref := a.Ref
		if ref == "" {
			ref = "would-open"
		}
		t.Delivered[a.Delivered] = ref
		t.DeliveredRefs = append(t.DeliveredRefs, ref)
		t.Deliveries = append(t.Deliveries, DeliveryRecord{Ref: ref, Repository: a.Repository, Delivered: a.Delivered, At: at})
	}
}

// settleQuestions settles a Questions escalation or update, posted or given
// up on: it carries the hash of the text it covered, and the episode it was
// posted for. One for an episode that has since closed settles nothing.
func (t *Track) settleQuestions(hash string, update, episode int, at time.Time) {
	if hash == "" || episode != t.Questions.Episode {
		return
	}
	t.Questions.Hash = hash
	if update > 0 {
		t.Questions.Updates, t.Questions.LastUpdate = update, at
	}
}

// settleDialog settles dialog escalation n, posted or given up on, for
// its episode.
func (t *Track) settleDialog(n, episode int, at time.Time) {
	if n <= 0 {
		return
	}
	if n > t.Dialog.N {
		t.Dialog.N, t.Dialog.Last = n, at
	}
	if episode > t.Dialog.Escalated {
		t.Dialog.Escalated = episode
	}
}
