package automations

import (
	"errors"

	"github.com/avanha/pmaas-plugin-automations/data"
	"github.com/avanha/pmaas-plugin-automations/internal/common"
	"github.com/avanha/pmaas-plugin-automations/internal/schedule"
	spi "github.com/avanha/pmaas-spi"
)

// Force implementation of common.AutomationStore
var _ common.AutomationStore = (*automationStoreAdapter)(nil)

// automationStoreAdapter is how HTTP requests reach the automations. They come in on arbitrary goroutines, and what
// is shown belongs to the plugin's own, while the automations are run on the server's.
type automationStoreAdapter struct {
	parent *plugin
}

func (a *automationStoreAdapter) GetAutomations() (data.Automations, error) {
	return spi.Exec(a.parent.container, a.parent.getAutomations)
}

type runNowResult struct {
	event schedule.Event
	err   error
}

func (a *automationStoreAdapter) RunNow(id string) (common.RunResult, error) {
	resultCh := make(chan runNowResult, 1)

	err := a.parent.container.EnqueueOnServerGoRoutine([]func(){func() {
		event, runErr := a.parent.runNow(id)
		resultCh <- runNowResult{event: event, err: runErr}
	}})
	if err != nil {
		return common.RunResult{}, errors.New("the server is not accepting requests")
	}

	result := <-resultCh
	if result.err != nil {
		return common.RunResult{}, result.err
	}

	return common.RunResult{
		Succeeded: result.event.Outcome == schedule.OutcomeSucceeded,
		Message:   result.event.Message,
		Error:     result.event.Error,
	}, nil
}
