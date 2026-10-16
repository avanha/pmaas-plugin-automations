package automations

import (
	"fmt"

	"github.com/avanha/pmaas-spi/automation"
)

// DefaultAlertAfterFailures is how many times in a row an automation has to fail before the plugin raises an alert about it,
// if the configuration doesn't say.
const DefaultAlertAfterFailures = 10

// PluginConfig configures the plugin.
type PluginConfig struct {
	// AlertAfterFailures is how many failed runs in a row it takes for the plugin to raise an alert that an automation is
	// failing, without waiting for it to give up: for something that runs every minute, like a fan being kept in step with
	// a temperature, giving up never comes, since there is always a new time it is due. Zero means
	// DefaultAlertAfterFailures, and a negative number means no alert until it gives up, which for those that never
	// do is never. The alert is cleared when the automation next runs.
	AlertAfterFailures int

	automations []automation.Automation
	ids         map[string]bool
}

func NewPluginConfig() PluginConfig {
	return PluginConfig{AlertAfterFailures: DefaultAlertAfterFailures, ids: make(map[string]bool)}
}

// alertAfterFailures is the number of failures in a row that raises the alert, or zero for none.
func (c PluginConfig) alertAfterFailures() int {
	switch {
	case c.AlertAfterFailures == 0:
		return DefaultAlertAfterFailures
	case c.AlertAfterFailures < 0:
		return 0
	default:
		return c.AlertAfterFailures
	}
}

// Add adds an automation for the plugin to run.
//
// It panics if the automation isn't valid, or has the same ID as one already added, like regexp.MustCompile does for a
// pattern that can't be: automations are written in the program, and a mistake in one should stop the program
// starting, not leave it quietly not doing what it was written to.
func (c *PluginConfig) Add(a automation.Automation) {
	if err := automation.Validate(a); err != nil {
		panic(fmt.Sprintf("automations: %v", err))
	}

	if c.ids == nil {
		c.ids = make(map[string]bool)
	}

	if c.ids[a.ID()] {
		panic(fmt.Sprintf("automations: automation ID %q is used by more than one automation", a.ID()))
	}

	c.ids[a.ID()] = true
	c.automations = append(c.automations, a)
}
