// Package data is what the automations page shows.
package data

import "time"

// Automations is everything the page shows.
type Automations struct {
	Items []Automation

	// Failing is how many are failing now, and being tried again.
	Failing int
}

// Automation is one automation as it stands.
type Automation struct {
	ID          string
	Name        string
	Description string
	Trigger     string

	// CatchUp is how long after it was due that it still runs, in words.
	CatchUp string

	Next    time.Time
	HasNext bool

	LastRun time.Time

	// EventDriven is that the automation is due when an event happens, not at a time: Trigger says which, and it has no Next, no
	// CatchUp and nothing to Re-enable.
	EventDriven bool

	// CanRearm is that Re-enable would do something: the time it was last due is dealt with and still worth running
	// for.
	CanRearm bool

	// Paused is that a person has paused it: it is not run until it is resumed.
	Paused bool

	// Outcome is "succeeded", "failed", "gave up", "missed", "waiting", "condition not met" or "not yet"; OutcomeClass is the same as a word safe
	// to use as a style, and Manual is that the last run was by hand.
	Outcome      string
	OutcomeClass string
	Manual       bool
	Message      string
	Error        string
	Attempts     int
}
