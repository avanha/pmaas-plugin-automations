package config

import "time"

// PersistentConfigV1 is the plugin's state persisted across restarts via
// IPMAASContainer.SaveConfig/LoadConfig.
//
// It is what lets an automation that was due while the server was down be run when it comes back, and not run
// twice for one time it was due: each automation's history, by its ID.
type PersistentConfigV1 struct {
	Automations map[string]AutomationStateV1
}

// AutomationStateV1 is one automation's history.
type AutomationStateV1 struct {
	// LastDue is the latest time the automation was due that has been dealt with.
	LastDue time.Time
	LastRun time.Time

	// Outcome is one of "succeeded", "failed", "gave up", "missed", or empty. It is kept as words so that what is
	// saved does not change meaning if the plugin's own numbering does.
	Outcome  string
	Manual   bool
	Message  string
	Error    string
	Attempts int
}
