// Package schedule decides when automations run, and what became of them. It knows nothing of the server: it is
// given the time, and runs the automations it is given, so all of it can be tested with fakes.
//
// Everything here belongs to one goroutine, the one the automations are to be called on.
package schedule

import (
	"fmt"
	"time"

	"github.com/avanha/pmaas-spi/automation"
)

// Outcome is what became of an automation the last time it was due, or was run by hand.
type Outcome int

const (
	// OutcomeNone is that it has not been due since it was first seen.
	OutcomeNone Outcome = iota

	// OutcomeSucceeded is that it ran.
	OutcomeSucceeded

	// OutcomeFailed is that it did not run, and is being tried again every minute until its catch-up has passed.
	OutcomeFailed

	// OutcomeGaveUp is that it did not run, and its catch-up passed while it was still failing.
	OutcomeGaveUp

	// OutcomeMissed is that its catch-up had passed before it could be tried, because the server was not running.
	OutcomeMissed

	// OutcomeWaiting is that it is due, but has said it is not ready, and is asked again every minute. The
	// reason it gave is the State's Message.
	OutcomeWaiting

	// OutcomeExpired is that it was waiting when its catch-up passed: it never had anything to do.
	OutcomeExpired
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSucceeded:
		return "succeeded"
	case OutcomeFailed:
		return "failed"
	case OutcomeGaveUp:
		return "gave up"
	case OutcomeMissed:
		return "missed"
	case OutcomeWaiting:
		return "waiting"
	case OutcomeExpired:
		return "condition not met"
	default:
		return "not yet"
	}
}

// ParseOutcome is the outcome that String gives the word for. Any word it doesn't know is OutcomeNone.
func ParseOutcome(word string) Outcome {
	for _, outcome := range []Outcome{
		OutcomeSucceeded, OutcomeFailed, OutcomeGaveUp, OutcomeMissed, OutcomeWaiting, OutcomeExpired,
	} {
		if outcome.String() == word {
			return outcome
		}
	}

	return OutcomeNone
}

// State is what is known of one automation's history. It is what is kept across restarts.
type State struct {
	// LastDue is the latest time the automation was due that has been dealt with, by running it, giving up on it
	// or finding it too late. It is what makes the automation not run twice for one time it was due.
	LastDue time.Time

	// LastRun is when the automation was last run, or tried, by the clock on the wall.
	LastRun time.Time

	Outcome Outcome

	// Manual is that LastRun, and Outcome, were of a run by hand.
	Manual bool

	// Message is the note the automation gave the last time it ran, and Error is why it failed.
	Message string
	Error   string

	// Attempts is how many times in a row it has failed for the time it is now due.
	Attempts int

	// Paused is that a person has stopped it running. A paused automation is not run, and the times it is due while it is
	// paused are dealt with as they come, so that resuming it does not run it for a time long gone. It is kept across restarts.
	Paused bool
}

// Event is something that happened to an automation, which the server may want to tell someone about.
type Event struct {
	ID      string
	Name    string
	Outcome Outcome
	Manual  bool
	Message string
	Error   string
	Due     time.Time

	// Attempts is how many times in a row the automation has now failed.
	Attempts int
}

// Status is an automation as it stands, for the page.
type Status struct {
	ID          string
	Name        string
	Description string
	Trigger     string
	CatchUp     time.Duration

	// Next is when it is next due, if it ever is.
	Next    time.Time
	HasNext bool

	// CanRearm is that the time it was last due is dealt with but its catch-up has not passed, so Rearm can make it
	// run for that time again.
	CanRearm bool

	State
}

type entry struct {
	automation.Automation
	State

	// seen is that the entry has been looked at once, which sets LastDue for an automation that has no history.
	seen bool

	// waitingFor is the latest time the automation was due that it waited to act on. It isn't kept across a restart: a
	// time it was waiting for before then is, afterwards, one that was missed. Only a time that is waited for and then
	// runs out matches it, since a time that is acted on or given up on is not looked at again.
	waitingFor time.Time
}

// Schedule runs a set of automations.
type Schedule struct {
	entries []*entry
	byID    map[string]*entry
}

// New makes a schedule of the automations, in the order given. It is an error for one to be invalid, or for two to
// have the same ID.
func New(automations []automation.Automation) (*Schedule, error) {
	s := &Schedule{byID: make(map[string]*entry, len(automations))}

	for _, a := range automations {
		if err := automation.Validate(a); err != nil {
			return nil, err
		}

		if _, duplicate := s.byID[a.ID()]; duplicate {
			return nil, fmt.Errorf("automation ID %q is used by more than one automation", a.ID())
		}

		e := &entry{Automation: a}
		s.entries = append(s.entries, e)
		s.byID[a.ID()] = e
	}

	return s, nil
}

// Restore gives automations the history they had before a restart. IDs it has no automation for are ignored.
func (s *Schedule) Restore(states map[string]State) {
	for id, state := range states {
		if e, ok := s.byID[id]; ok {
			e.State = state
			e.seen = !state.LastDue.IsZero()
		}
	}
}

// States is every automation's history, by ID, for keeping.
func (s *Schedule) States() map[string]State {
	states := make(map[string]State, len(s.entries))

	for _, e := range s.entries {
		states[e.ID()] = e.State
	}

	return states
}

// Evaluate runs the automations that are due. now is the minute it is, and wall the time on the clock on the wall,
// which is only noted down. It returns what happened to each automation that anything happened to, and whether that
// changed what Restore would need to be given.
//
// An automation is due if the latest time its trigger was due, at or before now, is after the latest it has been dealt
// with. That is one question that answers whether it is time, whether a minute was slept through, and whether the
// server was off when it was due. If it is within the automation's catch-up of that time it is run; if it fails it
// stays due, and is run again at the next minute, until it succeeds or the catch-up has passed.
func (s *Schedule) Evaluate(now time.Time, wall time.Time) (events []Event, changed bool) {
	now = now.Truncate(time.Minute)

	for _, e := range s.entries {
		due, ok := e.Trigger().Previous(now)
		if !ok {
			continue
		}

		if !e.seen {
			// An automation seen for the first time was not there when it was last due, so it does not run for that
			// time: a new "turn on at sunset", added at 9pm, doesn't turn the lights on at once.
			e.seen = true
			e.LastDue = due
			changed = true

			continue
		}

		if e.Paused {
			// Dealt with, not run, so that it isn't run for this time when it is resumed.
			if due.After(e.LastDue) {
				e.LastDue = due
				e.settleForPause()
				changed = true
			}

			continue
		}

		if !due.After(e.LastDue) {
			continue
		}

		if now.Sub(due) > e.CatchUp() {
			events = append(events, e.tooLate(due))
			changed = true

			continue
		}

		event, news := e.run(due, wall, false)
		if news {
			events = append(events, event)
			changed = true
		}
	}

	return events, changed
}

// settleForPause drops what an automation that is failing or waiting was doing, since it isn't going to be tried again
// while it is paused: leaving the error up would say it is still failing.
func (e *entry) settleForPause() {
	e.Attempts, e.waitingFor = 0, time.Time{}

	if e.Outcome == OutcomeFailed || e.Outcome == OutcomeWaiting {
		e.Outcome, e.Message, e.Error = OutcomeNone, "", ""
	}
}

// SetPaused pauses an automation, or resumes it, and reports whether that changed anything. Pausing is for the times it is due
// from now on: one that is paused is not run, and when it is resumed it carries on from the next time it is due, not from
// the ones it was paused through. A run by hand still works while it is paused.
func (s *Schedule) SetPaused(id string, paused bool) (bool, error) {
	e, ok := s.byID[id]
	if !ok {
		return false, fmt.Errorf("there is no automation %q", id)
	}

	if e.Paused == paused {
		return false, nil
	}

	e.Paused = paused

	if paused {
		e.settleForPause()
	}

	return true, nil
}

// tooLate deals with a time the automation was due that is further back than its catch-up.
func (e *entry) tooLate(due time.Time) Event {
	outcome := OutcomeMissed

	switch {
	case e.Attempts > 0:
		outcome = OutcomeGaveUp
	case e.waitingFor.Equal(due):
		outcome = OutcomeExpired
	}

	// What it last failed with, or last said it was waiting for, stays, to say why it gave up or expired.
	if outcome == OutcomeMissed {
		e.Message, e.Error = "", ""
	}

	attempts := e.Attempts
	e.LastDue, e.Outcome, e.Manual, e.Attempts = due, outcome, false, 0

	return e.event(due, attempts)
}

// RunNow runs an automation, on request. It doesn't change what the automation is due for: a run by hand is no
// reason for the scheduled one to be skipped, and a failure to run by hand is not retried.
func (s *Schedule) RunNow(id string, wall time.Time) (Event, error) {
	e, ok := s.byID[id]
	if !ok {
		return Event{}, fmt.Errorf("there is no automation %q", id)
	}

	event, _ := e.run(time.Time{}, wall, true)

	return event, nil
}

// Rearm makes an automation run again for the time it was last due, when it has already dealt with it and its catch-up
// has not passed: someone took the matter into their own hands, say, and wants it back in the automation's. It reports
// whether it did, and if not, why not.
func (s *Schedule) Rearm(id string, now time.Time) (bool, string, error) {
	e, ok := s.byID[id]
	if !ok {
		return false, "", fmt.Errorf("there is no automation %q", id)
	}

	if e.Paused {
		return false, "it is paused", nil
	}

	now = now.Truncate(time.Minute)

	due, ok := e.Trigger().Previous(now)
	if !ok {
		return false, "it has not been due yet", nil
	}

	if due.After(e.LastDue) {
		return false, "it is already due, and will run at the next minute", nil
	}

	if now.Sub(due) > e.CatchUp() {
		if next, hasNext := e.Trigger().Next(now); hasNext {
			return false, fmt.Sprintf("its time has passed, and it is next due at %s", next.Format("Mon 2006-01-02 3:04 PM")), nil
		}

		return false, "its time has passed", nil
	}

	// Just before the time it was due, so that it is due again. It can't have been failing or have an error to clear:
	// that is only while it is due, which is refused above.
	e.LastDue = due.Add(-time.Nanosecond)
	e.Outcome, e.Manual, e.Message = OutcomeWaiting, false, "re-enabled"

	return true, "", nil
}

// run runs the automation for the time it was due, which is zero for a run by hand. The event is what became of it,
// and news is whether that is worth telling anyone: an automation that is waiting is asked again every minute, and
// that, repeated, is not.
func (e *entry) run(due time.Time, wall time.Time, manual bool) (event Event, news bool) {
	message, err := e.Run()

	e.LastRun = wall
	e.Manual = manual

	if reason, notReady := automation.NotReadyReason(err); notReady {
		e.Outcome, e.Message, e.Error, e.Attempts = OutcomeWaiting, reason, "", 0

		if !manual {
			e.waitingFor = due
		}

		return e.event(due, 0), manual
	}

	e.Message = message

	if err == nil {
		e.Outcome, e.Error = OutcomeSucceeded, ""

		if !manual {
			e.LastDue, e.Attempts = due, 0
		}

		return e.event(due, 0), true
	}

	e.Outcome, e.Error = OutcomeFailed, err.Error()

	if !manual {
		e.Attempts++
	}

	return e.event(due, e.Attempts), true
}

func (e *entry) event(due time.Time, attempts int) Event {
	return Event{
		ID:       e.ID(),
		Name:     e.Name(),
		Outcome:  e.Outcome,
		Manual:   e.Manual,
		Message:  e.Message,
		Error:    e.Error,
		Due:      due,
		Attempts: attempts,
	}
}

// Statuses is every automation as it stands at now, in the order they were given.
func (s *Schedule) Statuses(now time.Time) []Status {
	statuses := make([]Status, len(s.entries))

	for i, e := range s.entries {
		next, hasNext := e.Trigger().Next(now)

		// The time it was last due, if it is dealt with and not too long ago for it to be worth running now.
		due, wasDue := e.Trigger().Previous(now)
		canRearm := !e.Paused && wasDue && !due.After(e.LastDue) && now.Truncate(time.Minute).Sub(due) <= e.CatchUp()

		statuses[i] = Status{
			ID:          e.ID(),
			Name:        e.Name(),
			Description: e.Description(),
			Trigger:     e.Trigger().Description(),
			CatchUp:     e.CatchUp(),
			Next:        next,
			HasNext:     hasNext,
			CanRearm:    canRearm,
			State:       e.State,
		}
	}

	return statuses
}
