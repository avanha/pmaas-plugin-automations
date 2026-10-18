package schedule

import (
	"fmt"
	"time"

	"github.com/avanha/pmaas-spi/automation"
	"github.com/avanha/pmaas-spi/events"
)

// reaction is an event-driven automation, and what is known of how it has got on. An event that makes it due runs it there
// and then. It is not run again if it fails, and it has no catch-up: it was wanted at the moment of the event, and that has
// gone. So its State is only what is shown: when it last ran, how that went, and whether it is paused.
type reaction struct {
	automation.EventAutomation
	State
}

// Reactions runs the event-driven automations, and like a Schedule belongs to one goroutine, the one the automations are to be
// called on, except for Wanted.
type Reactions struct {
	entries []*reaction
	byID    map[string]*reaction
}

// NewReactions makes the runner of the automations, in the order given. It is an error for one to be invalid, or for two to have
// the same ID.
func NewReactions(automations []automation.EventAutomation) (*Reactions, error) {
	r := &Reactions{byID: make(map[string]*reaction, len(automations))}

	for _, a := range automations {
		if err := automation.ValidateEvent(a); err != nil {
			return nil, err
		}

		if _, duplicate := r.byID[a.ID()]; duplicate {
			return nil, fmt.Errorf("automation ID %q is used by more than one automation", a.ID())
		}

		e := &reaction{EventAutomation: a}
		r.entries = append(r.entries, e)
		r.byID[a.ID()] = e
	}

	return r, nil
}

// Wanted is whether any of the automations is due for the event. It is the event receiver's predicate, so it is called on the
// event manager's goroutine for every event, and looks at nothing but the event: it does not look at whether an automation
// is paused, which belongs to the other goroutine. Handle does.
func (r *Reactions) Wanted(info *events.EventInfo) bool {
	for _, e := range r.entries {
		if e.EventTrigger().Matches(info) {
			return true
		}
	}

	return false
}

// Has is whether there is an automation with the ID.
func (r *Reactions) Has(id string) bool {
	_, ok := r.byID[id]

	return ok
}

// Handle runs the automations the event makes due, except those that are paused, and returns what happened to each. wall is
// the time on the clock on the wall, which is only noted down.
func (r *Reactions) Handle(info *events.EventInfo, wall time.Time) []Event {
	var result []Event

	for _, e := range r.entries {
		if e.Paused || !e.EventTrigger().Matches(info) {
			continue
		}

		result = append(result, e.run(wall, false))
	}

	return result
}

// RunNow runs an automation, on request, whether or not it is paused.
func (r *Reactions) RunNow(id string, wall time.Time) (Event, error) {
	e, ok := r.byID[id]
	if !ok {
		return Event{}, fmt.Errorf("there is no automation %q", id)
	}

	return e.run(wall, true), nil
}

func (e *reaction) run(wall time.Time, manual bool) Event {
	message, err := e.Run()

	e.LastRun, e.Manual, e.Message, e.Attempts = wall, manual, message, 0

	event := Event{ID: e.ID(), Name: e.Name(), Manual: manual, EventDriven: true}

	if reason, notReady := automation.NotReadyReason(err); notReady {
		// The event was no reason to act. That is not a failure.
		e.Outcome, e.Message, e.Error = OutcomeExpired, reason, ""
	} else if err != nil {
		e.Outcome, e.Error = OutcomeFailed, err.Error()
	} else {
		e.Outcome, e.Error = OutcomeSucceeded, ""
	}

	event.Outcome, event.Message, event.Error = e.Outcome, e.Message, e.Error

	return event
}

// SetPaused pauses an automation, or resumes it, and reports whether that changed anything. A paused one is not run by an
// event; the events it misses are not made up for when it is resumed.
func (r *Reactions) SetPaused(id string, paused bool) (bool, error) {
	e, ok := r.byID[id]
	if !ok {
		return false, fmt.Errorf("there is no automation %q", id)
	}

	if e.Paused == paused {
		return false, nil
	}

	e.Paused = paused

	return true, nil
}

// Restore gives automations the state they had before a restart. IDs it has no automation for are ignored.
func (r *Reactions) Restore(states map[string]State) {
	for id, state := range states {
		if e, ok := r.byID[id]; ok {
			e.State = state
		}
	}
}

// States is every automation's state, by ID, for keeping.
func (r *Reactions) States() map[string]State {
	states := make(map[string]State, len(r.entries))

	for _, e := range r.entries {
		states[e.ID()] = e.State
	}

	return states
}

// Statuses is every automation as it stands, in the order they were given.
func (r *Reactions) Statuses() []Status {
	statuses := make([]Status, len(r.entries))

	for i, e := range r.entries {
		statuses[i] = Status{
			ID:          e.ID(),
			Name:        e.Name(),
			Description: e.Description(),
			Trigger:     e.EventTrigger().Description(),
			EventDriven: true,
			State:       e.State,
		}
	}

	return statuses
}
