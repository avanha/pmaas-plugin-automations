package common

import "github.com/avanha/pmaas-plugin-automations/data"

// RunResult is what came of running an automation by hand.
type RunResult struct {
	// Succeeded is whether it ran, Message the note it gave, and Error why it did not.
	Succeeded bool
	Message   string
	Error     string
}

// AutomationStore is how internal/http reaches back into the plugin, which owns the automations. It's implemented
// by the plugin's adapter, which crosses onto the right goroutine.
type AutomationStore interface {
	GetAutomations() (data.Automations, error)

	// RunNow runs the automation with the ID, now, and waits for it.
	RunNow(id string) (RunResult, error)
}
