package common

import "github.com/avanha/pmaas-plugin-automations/data"

// RunResult is what came of running an automation by hand.
type RunResult struct {
	// Succeeded is whether it ran, Message the note it gave, and Error why it did not.
	Succeeded bool
	Message   string
	Error     string
}

// RearmResult is what came of asking for an automation to be enabled again.
type RearmResult struct {
	Armed bool

	// Why says why not, when it wasn't, in words fit to show.
	Why string
}

// AutomationStore is how internal/http reaches back into the plugin, which owns the automations. It's implemented
// by the plugin's adapter, which crosses onto the right goroutine.
type AutomationStore interface {
	GetAutomations() (data.Automations, error)

	// RunNow runs the automation with the ID, now, and waits for it.
	RunNow(id string) (RunResult, error)

	// Rearm makes the automation with the ID run again for the time it was last due, if that is still worth doing.
	Rearm(id string) (RearmResult, error)

	// SetPaused pauses the automation with the ID, or resumes it. The pause is kept across restarts.
	SetPaused(id string, paused bool) error
}
