package schedule

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avanha/pmaas-spi/automation"
)

var day = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

// at is a time on the day the tests are about.
func at(hour, minute int) time.Time {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// scripted is a trigger that is due at the times it is given.
type scripted struct{ dues []time.Time }

func (s scripted) Previous(t time.Time) (time.Time, bool) {
	var found time.Time
	ok := false

	for _, due := range s.dues {
		if !due.After(t) && (!ok || due.After(found)) {
			found, ok = due, true
		}
	}

	return found, ok
}

func (s scripted) Next(t time.Time) (time.Time, bool) {
	var found time.Time
	ok := false

	for _, due := range s.dues {
		if due.After(t) && (!ok || due.Before(found)) {
			found, ok = due, true
		}
	}

	return found, ok
}

func (s scripted) Description() string { return "scripted" }

// fake is an automation whose runs are scripted too.
type fake struct {
	id      string
	trigger scripted
	catchUp time.Duration

	// results are what the runs return, in turn; the last is used again when they run out.
	results []result
	runs    int
}

type result struct {
	message string
	err     error
}

func (f *fake) ID() string                  { return f.id }
func (f *fake) Name() string                { return "Name of " + f.id }
func (f *fake) Description() string         { return "about " + f.id }
func (f *fake) Trigger() automation.Trigger { return f.trigger }
func (f *fake) CatchUp() time.Duration      { return f.catchUp }

func (f *fake) Run() (string, error) {
	r := result{message: "ok"}

	if len(f.results) > 0 {
		r = f.results[min(f.runs, len(f.results)-1)]
	}

	f.runs++

	return r.message, r.err
}

func newFake(id string, catchUp time.Duration, dues ...time.Time) *fake {
	return &fake{id: id, trigger: scripted{dues}, catchUp: catchUp}
}

func mustSchedule(t *testing.T, fakes ...*fake) *Schedule {
	t.Helper()

	automations := make([]automation.Automation, len(fakes))
	for i, f := range fakes {
		automations[i] = f
	}

	s, err := New(automations)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func outcomes(events []Event) []string {
	var got []string

	for _, e := range events {
		got = append(got, e.ID+":"+e.Outcome.String())
	}

	return got
}

func expectOutcomes(t *testing.T, events []Event, want ...string) {
	t.Helper()

	if got := outcomes(events); !reflect.DeepEqual(got, want) {
		t.Errorf("outcomes %v, want %v", got, want)
	}
}

func TestEvaluate_RunsWhenDueAndNotAgain(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0))
	s := mustSchedule(t, a)

	// Seen before it was due, so it is not a new automation that was due before it was there.
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	events, changed := s.Evaluate(at(16, 59), at(16, 59))
	if len(events) != 0 || changed || a.runs != 0 {
		t.Fatalf("ran early: %v, runs %d", events, a.runs)
	}

	events, changed = s.Evaluate(at(17, 0), at(17, 0).Add(3*time.Second))
	expectOutcomes(t, events, "a:succeeded")

	if !changed || a.runs != 1 || events[0].Message != "ok" || !events[0].Due.Equal(at(17, 0)) {
		t.Errorf("changed %v, runs %d, event %+v", changed, a.runs, events[0])
	}

	state := s.States()["a"]
	if !state.LastDue.Equal(at(17, 0)) || !state.LastRun.Equal(at(17, 0).Add(3*time.Second)) ||
		state.Outcome != OutcomeSucceeded || state.Message != "ok" || state.Attempts != 0 {
		t.Errorf("state %+v", state)
	}

	for minute := 1; minute < 120; minute++ {
		if events, changed := s.Evaluate(at(17, 0).Add(time.Duration(minute)*time.Minute), at(18, 0)); len(events) != 0 || changed {
			t.Fatalf("minute %d: ran again: %v", minute, events)
		}
	}

	if a.runs != 1 {
		t.Errorf("runs %d", a.runs)
	}
}

func TestEvaluate_ANewAutomationDoesNotRunForATimeItWasDueBeforeItWasThere(t *testing.T) {
	a := newFake("a", 24*time.Hour, at(17, 0))
	s := mustSchedule(t, a)

	events, changed := s.Evaluate(at(21, 0), at(21, 0))

	if len(events) != 0 || a.runs != 0 {
		t.Fatalf("a new automation ran for a time before it was there: %v", events)
	}

	if !changed {
		t.Error("not noted as changed, so it would be forgotten again after a restart")
	}

	if got := s.States()["a"].LastDue; !got.Equal(at(17, 0)) {
		t.Errorf("last due %v", got)
	}

	// The next time it's due, it runs.
	tomorrow := at(17, 0).Add(24 * time.Hour)
	a.trigger.dues = append(a.trigger.dues, tomorrow)

	events, _ = s.Evaluate(tomorrow, tomorrow)
	expectOutcomes(t, events, "a:succeeded")
}

func TestEvaluate_AnAutomationNotYetDueAtAllDoesNothing(t *testing.T) {
	a := newFake("a", time.Hour)
	s := mustSchedule(t, a)

	if events, changed := s.Evaluate(at(12, 0), at(12, 0)); len(events) != 0 || changed || a.runs != 0 {
		t.Errorf("events %v, changed %v, runs %d", events, changed, a.runs)
	}
}

func TestEvaluate_CatchesUpAfterTheServerWasDownWhileItWasDue(t *testing.T) {
	a := newFake("a", 2*time.Hour, at(17, 0))
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}}) // it last ran yesterday

	// The server comes back at 18:30, an hour and a half late, which is within the two hours.
	events, _ := s.Evaluate(at(18, 30), at(18, 30))

	expectOutcomes(t, events, "a:succeeded")

	if !events[0].Due.Equal(at(17, 0)) {
		t.Errorf("due %v", events[0].Due)
	}
}

func TestEvaluate_MissesATimeThatIsTooFarBackAndSaysSo(t *testing.T) {
	a := newFake("a", 2*time.Hour, at(17, 0))
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0), Message: "old", Error: "old error"}})

	events, changed := s.Evaluate(at(20, 0), at(20, 0))

	expectOutcomes(t, events, "a:missed")

	if a.runs != 0 || !changed {
		t.Errorf("runs %d, changed %v", a.runs, changed)
	}

	state := s.States()["a"]
	if !state.LastDue.Equal(at(17, 0)) || state.Outcome != OutcomeMissed || state.Message != "" || state.Error != "" {
		t.Errorf("state %+v", state)
	}

	// And not again.
	if events, _ := s.Evaluate(at(20, 1), at(20, 1)); len(events) != 0 {
		t.Errorf("reported again: %v", events)
	}
}

func TestEvaluate_TheLatestTimeItWasDueIsTheOneThatRunsNotEachMissedOne(t *testing.T) {
	a := newFake("a", 24*time.Hour, at(17, 0), at(17, 0).Add(24*time.Hour), at(17, 0).Add(48*time.Hour))
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(17, 0)}})

	events, _ := s.Evaluate(at(17, 30).Add(48*time.Hour), at(17, 30))

	expectOutcomes(t, events, "a:succeeded")

	if a.runs != 1 || !events[0].Due.Equal(at(17, 0).Add(48*time.Hour)) {
		t.Errorf("runs %d, due %v", a.runs, events[0].Due)
	}
}

func TestEvaluate_AFailureIsTriedAgainEachMinuteUntilItWorks(t *testing.T) {
	boom := errors.New("the plug is offline")
	a := newFake("a", time.Hour, at(17, 0))
	a.results = []result{{"", boom}, {"turned on part", boom}, {"turned on all", nil}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	events, _ := s.Evaluate(at(17, 0), at(17, 0))
	expectOutcomes(t, events, "a:failed")

	if events[0].Attempts != 1 || events[0].Error != "the plug is offline" {
		t.Errorf("event %+v", events[0])
	}

	if s.States()["a"].LastDue.Equal(at(17, 0)) {
		t.Error("a failure was recorded as dealt with")
	}

	events, _ = s.Evaluate(at(17, 1), at(17, 1))
	expectOutcomes(t, events, "a:failed")

	if events[0].Attempts != 2 || events[0].Message != "turned on part" {
		t.Errorf("event %+v", events[0])
	}

	events, _ = s.Evaluate(at(17, 2), at(17, 2))
	expectOutcomes(t, events, "a:succeeded")

	state := s.States()["a"]
	if state.Attempts != 0 || state.Error != "" || state.Message != "turned on all" || !state.LastDue.Equal(at(17, 0)) {
		t.Errorf("state %+v", state)
	}

	if events, _ := s.Evaluate(at(17, 3), at(17, 3)); len(events) != 0 || a.runs != 3 {
		t.Errorf("ran again: %v, runs %d", events, a.runs)
	}
}

func TestEvaluate_GivesUpWhenTheCatchUpPassesWhileFailingAndKeepsWhy(t *testing.T) {
	boom := errors.New("the plug is offline")
	a := newFake("a", 2*time.Minute, at(17, 0))
	a.results = []result{{"half", boom}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	for _, minute := range []int{0, 1, 2} {
		events, _ := s.Evaluate(at(17, minute), at(17, minute))
		expectOutcomes(t, events, "a:failed")
	}

	events, _ := s.Evaluate(at(17, 3), at(17, 3))
	expectOutcomes(t, events, "a:gave up")

	if a.runs != 3 || events[0].Attempts != 3 || events[0].Error != "the plug is offline" || events[0].Message != "half" {
		t.Errorf("runs %d, event %+v", a.runs, events[0])
	}

	state := s.States()["a"]
	if !state.LastDue.Equal(at(17, 0)) || state.Outcome != OutcomeGaveUp || state.Attempts != 0 {
		t.Errorf("state %+v", state)
	}

	if events, _ := s.Evaluate(at(17, 4), at(17, 4)); len(events) != 0 {
		t.Errorf("tried again after giving up: %v", events)
	}
}

func TestEvaluate_ACatchUpOfZeroMeansOnlyTheMinuteItIsDue(t *testing.T) {
	a := newFake("a", 0, at(17, 0))
	a.results = []result{{"", errors.New("no")}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	events, _ := s.Evaluate(at(17, 0), at(17, 0))
	expectOutcomes(t, events, "a:failed")

	events, _ = s.Evaluate(at(17, 1), at(17, 1))
	expectOutcomes(t, events, "a:gave up")
}

func TestEvaluate_EachAutomationIsDealtWithOnItsOwnInTheOrderGiven(t *testing.T) {
	first := newFake("first", time.Hour, at(17, 0))
	broken := newFake("broken", time.Hour, at(17, 0))
	broken.results = []result{{"", errors.New("no")}}
	last := newFake("last", time.Hour, at(17, 0))
	s := mustSchedule(t, first, broken, last)
	s.Restore(map[string]State{
		"first": {LastDue: at(0, 0)}, "broken": {LastDue: at(0, 0)}, "last": {LastDue: at(0, 0)},
	})

	events, _ := s.Evaluate(at(17, 0), at(17, 0))

	expectOutcomes(t, events, "first:succeeded", "broken:failed", "last:succeeded")
}

func TestEvaluate_TheMinuteIsTheMinuteAnyWayItIsGiven(t *testing.T) {
	a := newFake("a", 0, at(17, 0))
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	events, _ := s.Evaluate(at(17, 0).Add(41*time.Second), at(17, 0))

	expectOutcomes(t, events, "a:succeeded")
}

func TestRestore_IgnoresWhatItHasNoAutomationFor(t *testing.T) {
	s := mustSchedule(t, newFake("a", time.Hour))

	s.Restore(map[string]State{"gone": {LastDue: at(1, 0)}, "a": {LastDue: at(2, 0)}})

	states := s.States()
	if len(states) != 1 || !states["a"].LastDue.Equal(at(2, 0)) {
		t.Errorf("states %+v", states)
	}
}

func TestRunNow_RunsItWithoutChangingWhatItIsDueFor(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0))
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	event, err := s.RunNow("a", at(12, 0))
	if err != nil || event.Outcome != OutcomeSucceeded || !event.Manual {
		t.Fatalf("event %+v, err %v", event, err)
	}

	state := s.States()["a"]
	if !state.LastRun.Equal(at(12, 0)) || !state.Manual || !state.LastDue.Equal(at(0, 0)) {
		t.Errorf("state %+v", state)
	}

	// The scheduled run still happens.
	events, _ := s.Evaluate(at(17, 0), at(17, 0))
	expectOutcomes(t, events, "a:succeeded")

	if state := s.States()["a"]; state.Manual {
		t.Errorf("the scheduled run is recorded as manual: %+v", state)
	}
}

func TestRunNow_AFailureIsNotRetriedAndDoesntCountAsAnAttempt(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0))
	a.results = []result{{"", errors.New("no")}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	event, _ := s.RunNow("a", at(12, 0))

	if event.Outcome != OutcomeFailed || event.Attempts != 0 || event.Error != "no" || !event.Manual {
		t.Errorf("event %+v", event)
	}

	if events, _ := s.Evaluate(at(12, 1), at(12, 1)); len(events) != 0 || a.runs != 1 {
		t.Errorf("retried: %v, runs %d", events, a.runs)
	}
}

func TestRunNow_UnknownID(t *testing.T) {
	if _, err := mustSchedule(t, newFake("a", time.Hour)).RunNow("nope", at(1, 0)); err == nil {
		t.Error("no error")
	}
}

func TestNew_RefusesWhatCouldNotBeRun(t *testing.T) {
	_, err := New([]automation.Automation{newFake("a", time.Hour), newFake("a", time.Hour)})
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("duplicates: %v", err)
	}

	if _, err := New([]automation.Automation{newFake("", time.Hour)}); err == nil {
		t.Error("an automation with no ID was accepted")
	}
}

func TestStatuses(t *testing.T) {
	a := newFake("a", 90*time.Minute, at(17, 0), at(17, 0).Add(24*time.Hour))
	never := newFake("never", time.Hour)
	s := mustSchedule(t, a, never)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0), Outcome: OutcomeSucceeded, Message: "hello"}})

	statuses := s.Statuses(at(12, 0))

	if len(statuses) != 2 {
		t.Fatalf("statuses %+v", statuses)
	}

	first := statuses[0]
	if first.ID != "a" || first.Name != "Name of a" || first.Description != "about a" || first.Trigger != "scripted" ||
		first.CatchUp != 90*time.Minute || !first.HasNext || !first.Next.Equal(at(17, 0)) || first.Message != "hello" {
		t.Errorf("first %+v", first)
	}

	if statuses[1].HasNext || statuses[1].ID != "never" {
		t.Errorf("second %+v", statuses[1])
	}
}

func TestOutcome_String(t *testing.T) {
	for outcome, want := range map[Outcome]string{
		OutcomeNone: "not yet", OutcomeSucceeded: "succeeded", OutcomeFailed: "failed",
		OutcomeGaveUp: "gave up", OutcomeMissed: "missed", OutcomeWaiting: "waiting", OutcomeExpired: "condition not met",
	} {
		if got := outcome.String(); got != want {
			t.Errorf("%d: %q, want %q", outcome, got, want)
		}
	}
}

func TestEvaluate_AWaitingAutomationIsAskedAgainEachMinuteQuietlyUntilItActs(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0))
	a.results = []result{
		{"", automation.NotReady("too warm: 78")},
		{"", automation.NotReady("too warm: 77")},
		{"turned it off", nil},
	}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	for _, minute := range []int{0, 1} {
		events, changed := s.Evaluate(at(17, minute), at(17, minute))

		if len(events) != 0 || changed {
			t.Fatalf("minute %d: a wait was news: %v, changed %v", minute, events, changed)
		}
	}

	status := s.Statuses(at(17, 1))[0]
	if status.Outcome != OutcomeWaiting || status.Message != "too warm: 77" || status.Error != "" || status.Attempts != 0 ||
		!status.LastRun.Equal(at(17, 1)) {
		t.Errorf("status %+v", status)
	}

	if !s.States()["a"].LastDue.Equal(at(0, 0)) {
		t.Error("waiting dealt with the time it was due")
	}

	events, changed := s.Evaluate(at(17, 2), at(17, 2))

	expectOutcomes(t, events, "a:succeeded")

	if !changed || a.runs != 3 {
		t.Errorf("changed %v, runs %d", changed, a.runs)
	}

	state := s.States()["a"]
	if !state.LastDue.Equal(at(17, 0)) || state.Message != "turned it off" || state.Outcome != OutcomeSucceeded {
		t.Errorf("state %+v", state)
	}

	// And it's done for this time.
	if events, _ := s.Evaluate(at(17, 3), at(17, 3)); len(events) != 0 || a.runs != 3 {
		t.Errorf("ran again: %v, runs %d", events, a.runs)
	}
}

func TestEvaluate_AWaitingAutomationWhoseTimeRunsOutExpiresQuietlyAndKeepsWhatItWasWaitingFor(t *testing.T) {
	a := newFake("a", 2*time.Minute, at(17, 0))
	a.results = []result{{"", automation.NotReady("too warm: 78")}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	for _, minute := range []int{0, 1, 2} {
		s.Evaluate(at(17, minute), at(17, minute))
	}

	events, changed := s.Evaluate(at(17, 3), at(17, 3))

	expectOutcomes(t, events, "a:condition not met")

	if !changed || a.runs != 3 || events[0].Message != "too warm: 78" || events[0].Error != "" || events[0].Attempts != 0 {
		t.Errorf("changed %v, runs %d, event %+v", changed, a.runs, events[0])
	}

	state := s.States()["a"]
	if !state.LastDue.Equal(at(17, 0)) || state.Outcome != OutcomeExpired {
		t.Errorf("state %+v", state)
	}

	if events, _ := s.Evaluate(at(17, 4), at(17, 4)); len(events) != 0 {
		t.Errorf("reported again: %v", events)
	}
}

func TestEvaluate_WhatWasWaitingBeforeARestartIsMissedAfterIt(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0))
	s := mustSchedule(t, a)

	// The state that was kept said it was waiting, but it's a different time that it is now too late for.
	s.Restore(map[string]State{"a": {LastDue: at(0, 0), Outcome: OutcomeWaiting, Message: "too warm"}})

	events, _ := s.Evaluate(at(20, 0), at(20, 0))

	expectOutcomes(t, events, "a:missed")

	if events[0].Message != "" {
		t.Errorf("event %+v", events[0])
	}
}

func TestEvaluate_AnAutomationThatFailedAndThenWaitsIsNoLongerFailing(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0))
	a.results = []result{{"", errors.New("offline")}, {"", automation.NotReady("too warm")}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	s.Evaluate(at(17, 0), at(17, 0))
	s.Evaluate(at(17, 1), at(17, 1))

	state := s.States()["a"]
	if state.Attempts != 0 || state.Error != "" || state.Outcome != OutcomeWaiting {
		t.Errorf("state %+v", state)
	}
}

func TestRunNow_AnAutomationThatIsNotReadyIsReportedAndLeavesWhatItIsDueFor(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0))
	a.results = []result{{"", automation.NotReady("too warm")}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	event, err := s.RunNow("a", at(12, 0))
	if err != nil || event.Outcome != OutcomeWaiting || !event.Manual || event.Message != "too warm" {
		t.Fatalf("event %+v, err %v", event, err)
	}

	if state := s.States()["a"]; !state.LastDue.Equal(at(0, 0)) || !state.Manual || state.Outcome != OutcomeWaiting {
		t.Errorf("state %+v", state)
	}

	// A wait by hand isn't one the time can run out on: nothing was waiting for 17:00.
	events, _ := s.Evaluate(at(20, 0), at(20, 0))
	expectOutcomes(t, events, "a:missed")
}

func TestRearm_MakesItRunAgainForTheTimeItWasLastDueWhileThatIsStillWorthDoing(t *testing.T) {
	a := newFake("a", 2*time.Hour, at(1, 0), at(1, 0).Add(24*time.Hour))
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	s.Evaluate(at(1, 0), at(1, 0)) // it acts, and is done for the night

	if status := s.Statuses(at(1, 30))[0]; !status.CanRearm {
		t.Fatalf("can't be re-armed after it acted: %+v", status)
	}

	armed, why, err := s.Rearm("a", at(1, 30))
	if err != nil || !armed || why != "" {
		t.Fatalf("armed %v, why %q, err %v", armed, why, err)
	}

	state := s.States()["a"]
	if state.Outcome != OutcomeWaiting || state.Message != "re-enabled" || state.Attempts != 0 || state.Error != "" {
		t.Errorf("state %+v", state)
	}

	if status := s.Statuses(at(1, 30))[0]; status.CanRearm {
		t.Error("can be re-armed again at once")
	}

	events, changed := s.Evaluate(at(1, 31), at(1, 31))

	expectOutcomes(t, events, "a:succeeded")

	if !changed || a.runs != 2 {
		t.Errorf("changed %v, runs %d", changed, a.runs)
	}

	// Once more it's done.
	if events, _ := s.Evaluate(at(1, 32), at(1, 32)); len(events) != 0 {
		t.Errorf("ran again: %v", events)
	}
}

func TestRearm_RefusesAndSaysWhy(t *testing.T) {
	a := newFake("a", time.Hour, at(17, 0), at(17, 0).Add(24*time.Hour))
	never := newFake("never", time.Hour)
	s := mustSchedule(t, a, never)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}, "never": {LastDue: at(0, 0)}})

	// Already due, and not yet run.
	armed, why, err := s.Rearm("a", at(17, 0))
	if err != nil || armed || !strings.Contains(why, "already due") {
		t.Errorf("already due: %v %q %v", armed, why, err)
	}

	s.Evaluate(at(17, 0), at(17, 0))

	// Its time has passed.
	armed, why, err = s.Rearm("a", at(19, 0))
	if err != nil || armed || !strings.Contains(why, "next due at Sat 2026-10-17 5:00 PM") {
		t.Errorf("too late: %v %q %v", armed, why, err)
	}

	// It never has been due.
	armed, why, err = s.Rearm("never", at(19, 0))
	if err != nil || armed || !strings.Contains(why, "not been due") {
		t.Errorf("never due: %v %q %v", armed, why, err)
	}

	if _, _, err := s.Rearm("nope", at(19, 0)); err == nil {
		t.Error("an unknown automation")
	}

	if status := s.Statuses(at(19, 0))[0]; status.CanRearm {
		t.Error("offered after its time passed")
	}
}

func TestRearm_AfterItGaveUpOrExpiredToo(t *testing.T) {
	a := newFake("a", 2*time.Minute, at(17, 0))
	a.results = []result{{"", automation.NotReady("too warm")}}
	s := mustSchedule(t, a)
	s.Restore(map[string]State{"a": {LastDue: at(0, 0)}})

	for minute := 0; minute < 4; minute++ {
		s.Evaluate(at(17, minute), at(17, minute))
	}

	// It expired at 17:03, which is past the catch-up, so it is too late to re-arm.
	if armed, _, _ := s.Rearm("a", at(17, 3)); armed {
		t.Error("re-armed after its time ran out")
	}
}

func TestParseOutcome(t *testing.T) {
	for _, outcome := range []Outcome{
		OutcomeSucceeded, OutcomeFailed, OutcomeGaveUp, OutcomeMissed, OutcomeWaiting, OutcomeExpired,
	} {
		if got := ParseOutcome(outcome.String()); got != outcome {
			t.Errorf("%q came back as %v", outcome.String(), got)
		}
	}

	for _, word := range []string{"", "not yet", "something new"} {
		if got := ParseOutcome(word); got != OutcomeNone {
			t.Errorf("%q came back as %v", word, got)
		}
	}
}
