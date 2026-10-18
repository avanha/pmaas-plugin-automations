package automations

import (
	"errors"
	"fmt"
	"sync"

	"github.com/avanha/pmaas-spi/alert"
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
	reactions   []automation.EventAutomation
	ids         map[string]bool
	alerts      *alertBroadcaster
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

// AddEvent adds an event-driven automation for the plugin to run: one that is due when an event matches, such as a button
// being pressed (see automations.NewOnEvent in pmaas-automations). It runs the moment the event arrives, and is not tried again if
// it fails.
//
// It panics, as Add does, if the automation isn't valid, or has the same ID as one already added, of either kind.
func (c *PluginConfig) AddEvent(a automation.EventAutomation) {
	if err := automation.ValidateEvent(a); err != nil {
		panic(fmt.Sprintf("automations: %v", err))
	}

	if c.ids == nil {
		c.ids = make(map[string]bool)
	}

	if c.ids[a.ID()] {
		panic(fmt.Sprintf("automations: automation ID %q is used by more than one automation", a.ID()))
	}

	c.ids[a.ID()] = true
	c.reactions = append(c.reactions, a)
}

var errAlertsNotAvailable = errors.New("the automations plugin is not running, so it can't send alerts")

// alertBroadcaster is how an automation raises an alert: it sends what it is given to the server's event manager, through
// the plugin's container, once the plugin has started, and says it can't before then and after it has stopped.
type alertBroadcaster struct {
	mu          sync.Mutex
	broadcaster alert.Broadcaster
}

func (b *alertBroadcaster) BroadcastEvent(entityEventId string, event any) error {
	b.mu.Lock()
	broadcaster := b.broadcaster
	b.mu.Unlock()

	if broadcaster == nil {
		return errAlertsNotAvailable
	}

	return broadcaster.BroadcastEvent(entityEventId, event)
}

func (b *alertBroadcaster) set(broadcaster alert.Broadcaster) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.broadcaster = broadcaster
}

// Alerts is where an automation sends the alerts it raises (see automations.WatchAlert in pmaas-automations). It is
// usable from the moment it is made, so it can be given to an automation while the assembly is configured: until the plugin
// has started, and after it has stopped, sending to it fails. Every call returns the same one.
func (c *PluginConfig) Alerts() alert.Broadcaster {
	if c.alerts == nil {
		c.alerts = &alertBroadcaster{}
	}

	return c.alerts
}
