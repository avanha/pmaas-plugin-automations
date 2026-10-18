package schedule

import (
	"errors"
	"reflect"
	"testing"

	"github.com/avanha/pmaas-spi/automation"
	"github.com/avanha/pmaas-spi/events"
)

// A string event is the one these react to, named by its text.
type stringTrigger struct{ want string }

func (t stringTrigger) Matches(info *events.EventInfo) bool {
	s, ok := info.Event.(string)

	return ok && s == t.want
}

func (t stringTrigger) Description() string { return "when " + t.want + " happens" }

type fakeReaction struct {
	id      string
	trigger stringTrigger
	results []result
	runs    int
}

func (f *fakeReaction) ID() string                            { return f.id }
func (f *fakeReaction) Name() string                          { return "Name of " + f.id }
func (f *fakeReaction) Description() string                   { return "about " + f.id }
func (f *fakeReaction) EventTrigger() automation.EventTrigger { return f.trigger }

func (f *fakeReaction) Run() (string, error) {
	r := result{message: "ok"}

	if len(f.results) > 0 {
		r = f.results[min(f.runs, len(f.results)-1)]
	}

	f.runs++

	return r.message, r.err
}

func newReaction(id string, on string) *fakeReaction {
	return &fakeReaction{id: id, trigger: stringTrigger{on}}
}

func mustReactions(t *testing.T, fakes ...*fakeReaction) *Reactions {
	t.Helper()

	automations := make([]automation.EventAutomation, len(fakes))
	for i, f := range fakes {
		automations[i] = f
	}

	r, err := NewReactions(automations)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func happens(event string) *events.EventInfo { return &events.EventInfo{Event: event} }

func TestReactions_AnEventRunsWhatItMakesDueAndNothingElse(t *testing.T) {
	a, b, c := newReaction("a", "ring"), newReaction("b", "ring"), newReaction("c", "knock")
	r := mustReactions(t, a, b, c)

	got := r.Handle(happens("ring"), at(9, 0))
	if !reflect.DeepEqual(outcomes(got), []string{"a:succeeded", "b:succeeded"}) || a.runs != 1 || b.runs != 1 || c.runs != 0 {
		t.Errorf("outcomes %v, runs %d %d %d", outcomes(got), a.runs, b.runs, c.runs)
	}

	for _, e := range got {
		if !e.EventDriven || e.Manual || e.Message != "ok" || !e.Due.IsZero() || e.Attempts != 0 {
			t.Errorf("event %+v", e)
		}
	}

	if got := r.Handle(happens("nothing"), at(9, 1)); len(got) != 0 {
		t.Errorf("an event nobody wants ran %v", outcomes(got))
	}
}

func TestReactions_Wanted(t *testing.T) {
	r := mustReactions(t, newReaction("a", "ring"), newReaction("c", "knock"))

	for event, want := range map[string]bool{"ring": true, "knock": true, "bell": false} {
		if got := r.Wanted(happens(event)); got != want {
			t.Errorf("%s: %v, want %v", event, got, want)
		}
	}

	if r.Wanted(&events.EventInfo{Event: 42}) {
		t.Error("something that isn't a string was wanted")
	}
}

func TestReactions_ARunIsRecordedAndAFailureIsNotRetried(t *testing.T) {
	a := newReaction("a", "ring")
	a.results = []result{{err: errors.New("the plug is offline")}, {message: "turned on"}}
	r := mustReactions(t, a)

	got := r.Handle(happens("ring"), at(9, 0))
	if !reflect.DeepEqual(outcomes(got), []string{"a:failed"}) || got[0].Error != "the plug is offline" {
		t.Fatalf("%+v", got)
	}

	// Nothing runs it again until another event does.
	if a.runs != 1 {
		t.Errorf("ran %d times", a.runs)
	}

	st := r.Statuses()[0]
	if st.Outcome != OutcomeFailed || st.Error != "the plug is offline" || !st.LastRun.Equal(at(9, 0)) || st.Attempts != 0 {
		t.Errorf("status %+v", st.State)
	}

	// The next works, and the failure is gone from what is shown.
	got = r.Handle(happens("ring"), at(9, 5))
	if !reflect.DeepEqual(outcomes(got), []string{"a:succeeded"}) {
		t.Fatalf("%v", outcomes(got))
	}

	if st := r.Statuses()[0]; st.Outcome != OutcomeSucceeded || st.Error != "" || st.Message != "turned on" {
		t.Errorf("status %+v", st.State)
	}
}

func TestReactions_AnEventThatIsNoReasonToActIsRecordedAsTheConditionNotBeingMet(t *testing.T) {
	a := newReaction("a", "ring")
	a.results = []result{{err: automation.NotReady("it is daylight")}}
	r := mustReactions(t, a)

	got := r.Handle(happens("ring"), at(9, 0))
	if !reflect.DeepEqual(outcomes(got), []string{"a:condition not met"}) || got[0].Message != "it is daylight" || got[0].Error != "" {
		t.Errorf("%+v", got)
	}

	if st := r.Statuses()[0]; st.Outcome != OutcomeExpired || st.Message != "it is daylight" {
		t.Errorf("status %+v", st.State)
	}
}

func TestReactions_APausedOneIsNotRunByAnEventButCanBeRunByHand(t *testing.T) {
	a := newReaction("a", "ring")
	r := mustReactions(t, a)

	changed, err := r.SetPaused("a", true)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}

	if got := r.Handle(happens("ring"), at(9, 0)); len(got) != 0 || a.runs != 0 {
		t.Errorf("a paused automation ran: %v", outcomes(got))
	}

	event, err := r.RunNow("a", at(9, 1))
	if err != nil || event.Outcome != OutcomeSucceeded || !event.Manual || !event.EventDriven || a.runs != 1 {
		t.Errorf("%+v %v", event, err)
	}

	// Resumed, the next event runs it; the one it missed is not made up for.
	if changed, _ := r.SetPaused("a", false); !changed {
		t.Error("resuming changed nothing")
	}

	if got := r.Handle(happens("ring"), at(9, 2)); len(got) != 1 || a.runs != 2 {
		t.Errorf("ran %d times", a.runs)
	}

	if changed, _ := r.SetPaused("a", false); changed {
		t.Error("resuming a running one changed something")
	}
}

func TestReactions_StatusesAndStateAreKeptAndRestored(t *testing.T) {
	a := newReaction("a", "ring")
	r := mustReactions(t, a)
	r.Handle(happens("ring"), at(9, 0))
	_, _ = r.SetPaused("a", true)

	st := r.Statuses()
	if len(st) != 1 || st[0].ID != "a" || st[0].Name != "Name of a" || st[0].Description != "about a" ||
		st[0].Trigger != "when ring happens" || !st[0].EventDriven || st[0].HasNext || st[0].CanRearm || !st[0].Paused {
		t.Errorf("status %+v", st)
	}

	restarted := mustReactions(t, newReaction("a", "ring"), newReaction("b", "knock"))
	restarted.Restore(r.States())
	restarted.Restore(map[string]State{"gone": {Paused: true}})

	if s := restarted.Statuses(); !s[0].Paused || s[0].Outcome != OutcomeSucceeded || !s[0].LastRun.Equal(at(9, 0)) || s[1].Paused {
		t.Errorf("restored %+v", s)
	}

	if got := restarted.Handle(happens("ring"), at(10, 0)); len(got) != 0 {
		t.Error("a paused automation ran after a restart")
	}
}

func TestReactions_UnknownIDsAndBadConfigurationAreErrors(t *testing.T) {
	r := mustReactions(t, newReaction("a", "ring"))

	if _, err := r.RunNow("nope", at(9, 0)); err == nil {
		t.Error("RunNow of nothing")
	}

	if _, err := r.SetPaused("nope", true); err == nil {
		t.Error("SetPaused of nothing")
	}

	if r.Has("nope") || !r.Has("a") {
		t.Error("Has")
	}

	if _, err := NewReactions([]automation.EventAutomation{newReaction("a", "x"), newReaction("a", "y")}); err == nil {
		t.Error("two with the same ID")
	}

	if _, err := NewReactions([]automation.EventAutomation{&fakeReaction{id: "", trigger: stringTrigger{"x"}}}); err == nil {
		t.Error("one with no ID")
	}
}
