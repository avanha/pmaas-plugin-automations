package automations

import (
	"fmt"

	"github.com/avanha/pmaas-spi/automation"
)

// PluginConfig configures the plugin.
type PluginConfig struct {
	automations []automation.Automation
	ids         map[string]bool
}

func NewPluginConfig() PluginConfig {
	return PluginConfig{ids: make(map[string]bool)}
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
