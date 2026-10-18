package automations

import (
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/avanha/pmaas-plugin-automations/config"
	"github.com/avanha/pmaas-plugin-automations/data"
	httphandler "github.com/avanha/pmaas-plugin-automations/internal/http"
	"github.com/avanha/pmaas-plugin-automations/internal/schedule"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/alert"
	"github.com/avanha/pmaas-spi/clock"
	"github.com/avanha/pmaas-spi/events"
)

type Plugin interface {
	spi.IPMAASPlugin
}

// plugin runs the automations, and shows how they are getting on.
//
// Two goroutines are involved. The automations are called only on the server's goroutine, the one that runs the
// assembly's code, so that they can use what the assembly gives them without anyone having to lock it: the schedule
// belongs to it. The plugin's own goroutine owns what is shown and what is kept: after the server's goroutine has
// evaluated the schedule it hands over a copy of how things stand.
type plugin struct {
	config      PluginConfig
	container   spi.IPMAASContainer
	httpHandler *httphandler.Handler

	// schedule and reactions belong to the server's goroutine once the plugin has started: the first is the automations that are
	// due at a time, and the second those that are due when an event matches.
	schedule  *schedule.Schedule
	reactions *schedule.Reactions

	// stopping is set when the plugin is told to stop, so that what is still queued for the server's goroutine does
	// not turn the lights on while the server is shutting down.
	stopping atomic.Bool

	receiverHandle     int
	receiverRegistered bool

	// reactionReceiver is the registration for the events the event-driven automations are waiting for.
	reactionReceiver           int
	reactionReceiverRegistered bool

	// Owned by the plugin's goroutine.
	statuses  []schedule.Status
	evaluated time.Time

	// alerting is the automations the plugin has an alert up for, by ID, so that it clears only what it raised.
	alerting map[string]bool

	now func() time.Time
}

// Force implementation of spi.IPMAASPlugin
var _ spi.IPMAASPlugin = (*plugin)(nil)

func NewPlugin(config PluginConfig) Plugin {
	return &plugin{config: config, httpHandler: httphandler.NewHandler(), now: time.Now, alerting: make(map[string]bool)}
}

func (p *plugin) ShortName() string {
	return "automations"
}

func (p *plugin) Init(container spi.IPMAASContainer) {
	p.container = container

	s, err := schedule.New(p.config.automations)
	if err != nil {
		// Add has checked all of this, so it is only reached by a configuration that wasn't made with it.
		panic(fmt.Sprintf("%T: %v", p, err))
	}

	p.schedule = s

	r, err := schedule.NewReactions(p.config.reactions)
	if err != nil {
		panic(fmt.Sprintf("%T: %v", p, err))
	}

	p.reactions = r

	if p.config.alerts != nil {
		p.config.alerts.set(container)
	}

	p.httpHandler.Init(container, &automationStoreAdapter{parent: p})
}

func (p *plugin) Start() {
	fmt.Printf("%T Starting with %d automation(s) and %d event-driven one(s)...\n", p, len(p.config.automations), len(p.config.reactions))

	states := p.loadPersistedState()

	handle, err := p.container.RegisterEventReceiver(isMinuteChanged, p.onClockEvent)
	if err != nil {
		panic(fmt.Sprintf("Unable to register for clock events: %v", err))
	}

	p.receiverHandle = handle
	p.receiverRegistered = true

	if len(p.config.reactions) > 0 {
		handle, err := p.container.RegisterEventReceiver(p.reactions.Wanted, p.onReactionEvent)
		if err != nil {
			panic(fmt.Sprintf("Unable to register for the events the event-driven automations wait for: %v", err))
		}

		p.reactionReceiver, p.reactionReceiverRegistered = handle, true
	}

	// From here the schedule belongs to the server's goroutine, so what was kept is handed to it, and it is looked at
	// at once: an automation that was due while the server was down is run now, not at the next minute.
	err = p.container.EnqueueOnServerGoRoutine([]func(){func() {
		p.schedule.Restore(states)
		p.reactions.Restore(states)
		p.evaluate(p.now())
	}})
	if err != nil {
		fmt.Printf("%T unable to start the schedule: %v\n", p, err)
	}
}

func (p *plugin) Stop() chan func() {
	fmt.Printf("%T Stopping...\n", p)

	p.stopping.Store(true)

	if p.config.alerts != nil {
		p.config.alerts.set(nil)
	}

	if p.receiverRegistered {
		p.receiverRegistered = false

		if err := p.container.DeregisterEventReceiver(p.receiverHandle); err != nil {
			fmt.Printf("%T unable to deregister the clock event receiver: %v\n", p, err)
		}
	}

	if p.reactionReceiverRegistered {
		p.reactionReceiverRegistered = false

		if err := p.container.DeregisterEventReceiver(p.reactionReceiver); err != nil {
			fmt.Printf("%T unable to deregister the receiver of the events automations wait for: %v\n", p, err)
		}
	}

	return p.container.ClosedCallbackChannel()
}

func isMinuteChanged(eventInfo *events.EventInfo) bool {
	_, ok := eventInfo.Event.(clock.MinuteChangedEvent)

	return ok
}

// onClockEvent is told each minute, on the plugin's goroutine, and hands the evaluation to the server's.
func (p *plugin) onClockEvent(eventInfo *events.EventInfo) error {
	event, ok := eventInfo.Event.(clock.MinuteChangedEvent)
	if !ok {
		return nil
	}

	return p.container.EnqueueOnServerGoRoutine([]func(){func() { p.evaluate(event.Time) }})
}

// onReactionEvent is told of an event some event-driven automation is waiting for, on the plugin's goroutine, and hands it to the
// server's, in the order they came.
func (p *plugin) onReactionEvent(eventInfo *events.EventInfo) error {
	return p.container.EnqueueOnServerGoRoutine([]func(){func() { p.react(eventInfo) }})
}

// react runs the event-driven automations the event makes due. Runs on the server's goroutine.
func (p *plugin) react(eventInfo *events.EventInfo) {
	if p.stopping.Load() {
		return
	}

	ran := p.reactions.Handle(eventInfo, p.now())
	if len(ran) == 0 {
		return
	}

	p.publish(p.now(), ran, true)
}

// evaluate runs what is due at now. Runs on the server's goroutine.
func (p *plugin) evaluate(now time.Time) {
	if p.stopping.Load() {
		return
	}

	events, changed := p.schedule.Evaluate(now, p.now())
	p.publish(now, events, changed)
}

// runNow runs an automation on request. Runs on the server's goroutine.
func (p *plugin) runNow(id string) (schedule.Event, error) {
	if p.stopping.Load() {
		return schedule.Event{}, errors.New("the server is stopping")
	}

	var event schedule.Event
	var err error

	if p.reactions.Has(id) {
		event, err = p.reactions.RunNow(id, p.now())
	} else {
		event, err = p.schedule.RunNow(id, p.now())
	}

	if err != nil {
		return schedule.Event{}, err
	}

	p.publish(p.now(), []schedule.Event{event}, true)

	return event, nil
}

// setPaused pauses an automation or resumes it. Runs on the server's goroutine.
func (p *plugin) setPaused(id string, paused bool) error {
	if p.stopping.Load() {
		return errors.New("the server is stopping")
	}

	var changed bool
	var err error

	if p.reactions.Has(id) {
		changed, err = p.reactions.SetPaused(id, paused)
	} else {
		changed, err = p.schedule.SetPaused(id, paused)
	}

	if err != nil {
		return err
	}

	now := p.now()
	p.publish(now, nil, changed)

	if changed && paused {
		// A paused automation is not going to run, so an alert that it did not would never clear.
		err := p.container.EnqueueOnPluginGoRoutine(func() { p.clearAlert(id) })
		if err != nil {
			fmt.Printf("%T unable to clear the alert for %q: %v\n", p, id, err)
		}
	}

	return nil
}

// clearAlert clears the alert about an automation not running, if the plugin raised one. Runs on the plugin's goroutine.
func (p *plugin) clearAlert(id string) {
	if p.alerting[id] {
		delete(p.alerting, id)
		p.sendAlert(alert.Clear(p.container, p.ShortName(), alertKey(id)))
	}
}

// rearm makes an automation run again for the time it was last due, if that is still worth doing, and looks at it at
// once. Runs on the server's goroutine.
func (p *plugin) rearm(id string) (bool, string, error) {
	if p.stopping.Load() {
		return false, "", errors.New("the server is stopping")
	}

	if p.reactions.Has(id) {
		return false, "it is event-driven, so there is no time to run it again for", nil
	}

	now := p.now()

	armed, why, err := p.schedule.Rearm(id, now)
	if err != nil || !armed {
		return false, why, err
	}

	// Looked at now rather than at the next minute, so that the page the person is looking at is right when they
	// reload it. What is kept has changed, whether or not it runs.
	events, _ := p.schedule.Evaluate(now, now)
	p.publish(now, events, true)

	return true, "", nil
}

// publish hands the plugin's goroutine a copy of how the schedule stands. Runs on the server's goroutine. The states
// are handed over only when there is something new to keep.
func (p *plugin) publish(now time.Time, events []schedule.Event, changed bool) {
	statuses := append(p.schedule.Statuses(now), p.reactions.Statuses()...)

	var states map[string]schedule.State
	if changed {
		states = p.schedule.States()

		for id, state := range p.reactions.States() {
			states[id] = state
		}
	}

	err := p.container.EnqueueOnPluginGoRoutine(func() { p.apply(now, statuses, states, events) })
	if err != nil {
		fmt.Printf("%T unable to report how the automations are getting on: %v\n", p, err)
	}
}

// apply takes in how the schedule stands: what to show, what to keep, and what to tell the user. Runs on the plugin's
// goroutine.
func (p *plugin) apply(now time.Time, statuses []schedule.Status, states map[string]schedule.State, events []schedule.Event) {
	p.statuses = statuses
	p.evaluated = now

	if states != nil {
		p.persist(states)
	}

	for _, event := range events {
		p.report(event)
	}
}

// report logs what happened to an automation, and raises an alert when one has not run and now never will.
func (p *plugin) report(event schedule.Event) {
	how := ""
	if event.Manual {
		how = " (by hand)"
	}

	if event.EventDriven {
		p.reportReaction(event, how)

		return
	}

	switch event.Outcome {
	case schedule.OutcomeSucceeded:
		fmt.Printf("%T ran %q%s: %s\n", p, event.Name, how, event.Message)

		// Only if it was raised: an automation that never failed has nothing to clear, and that is most of them.
		p.clearAlert(event.ID)
	case schedule.OutcomeFailed:
		// Every minute it is tried again, which would fill the log, so only the first, and then now and then.
		if event.Manual || event.Attempts == 1 || event.Attempts%10 == 0 {
			fmt.Printf("%T unable to run %q%s (attempt %d): %s\n", p, event.Name, how, event.Attempts, event.Error)
		}

		// One that is run every minute never gives up, so an alert has to come before then, or never would: when it has
		// failed enough times in a row. Once, since the alert stays until it works. A run by hand has no attempts, so it never counts.
		if threshold := p.config.alertAfterFailures(); threshold > 0 && event.Attempts == threshold {
			p.alerting[event.ID] = true
			p.sendAlert(alert.Raise(p.container, p.ShortName(), alertKey(event.ID), alert.SeverityWarning,
				fmt.Sprintf("Automation is failing: %s", event.Name),
				fmt.Sprintf("It has failed %d times in a row, and is still being tried. Latest error: %s", event.Attempts, event.Error)))
		}
	case schedule.OutcomeGaveUp:
		fmt.Printf("%T gave up on %q after %d attempts: %s\n", p, event.Name, event.Attempts, event.Error)

		p.alerting[event.ID] = true
		p.sendAlert(alert.Raise(p.container, p.ShortName(), alertKey(event.ID), alert.SeverityWarning,
			fmt.Sprintf("Automation did not run: %s", event.Name),
			fmt.Sprintf("It was due at %s and failed %d times: %s",
				event.Due.Format("2006-01-02 3:04 PM"), event.Attempts, event.Error)))
	case schedule.OutcomeMissed:
		fmt.Printf("%T missed %q, which was due at %s, too long ago to run now\n",
			p, event.Name, event.Due.Format("2006-01-02 3:04 PM"))
	case schedule.OutcomeExpired:
		// It was waiting for something that didn't happen. That's not a failure, so it isn't an alert.
		fmt.Printf("%T %q was still waiting when its time ran out (it was due at %s): %s\n",
			p, event.Name, event.Due.Format("2006-01-02 3:04 PM"), event.Message)
	case schedule.OutcomeWaiting:
		// Only a run by hand is reported: the waiting that is asked about every minute is not news.
		fmt.Printf("%T %q%s is not ready: %s\n", p, event.Name, how, event.Message)
	}
}

// reportReaction logs what happened to an event-driven automation, and raises an alert when it failed, since it will not be tried
// again, and clears it when it next works.
func (p *plugin) reportReaction(event schedule.Event, how string) {
	switch event.Outcome {
	case schedule.OutcomeSucceeded:
		fmt.Printf("%T ran %q%s: %s\n", p, event.Name, how, event.Message)
		p.clearAlert(event.ID)
	case schedule.OutcomeFailed:
		fmt.Printf("%T unable to run %q%s: %s\n", p, event.Name, how, event.Error)

		// A person pressed a button and nothing happened. A run by hand is a person looking at the page, who sees it.
		if !event.Manual {
			p.alerting[event.ID] = true
			p.sendAlert(alert.Raise(p.container, p.ShortName(), alertKey(event.ID), alert.SeverityWarning,
				fmt.Sprintf("Automation failed: %s", event.Name),
				fmt.Sprintf("It was run by an event, and failed: %s", event.Error)))
		}
	case schedule.OutcomeExpired:
		fmt.Printf("%T %q%s was not run: %s\n", p, event.Name, how, event.Message)
	}
}

func (p *plugin) sendAlert(err error) {
	if err != nil {
		fmt.Printf("%T unable to send an alert: %v\n", p, err)
	}
}

func alertKey(id string) string {
	return "did-not-run:" + id
}

// persist keeps how the automations have got on, so a restart doesn't forget it. Runs on the plugin's goroutine.
func (p *plugin) persist(states map[string]schedule.State) {
	persisted := config.PersistentConfigV1{Automations: make(map[string]config.AutomationStateV1, len(states))}

	for id, state := range states {
		persisted.Automations[id] = persistedState(state)
	}

	if err := p.container.SaveConfig(persisted); err != nil {
		fmt.Printf("%T unable to keep how the automations have got on: %v\n", p, err)
	}
}

// loadPersistedState is what was kept. Nothing, if there was nothing, or it can't be read: the automations then start
// as if new, which doesn't run any for a time it was due before.
func (p *plugin) loadPersistedState() map[string]schedule.State {
	loaded, err := p.container.LoadConfig(func(typeName string) any {
		v1Type := reflect.TypeFor[config.PersistentConfigV1]()

		if typeName == v1Type.PkgPath()+"/"+v1Type.Name() {
			return &config.PersistentConfigV1{}
		}

		return nil
	})

	if err != nil || loaded == nil {
		return nil
	}

	persisted, ok := loaded.(*config.PersistentConfigV1)
	if !ok {
		return nil
	}

	states := make(map[string]schedule.State, len(persisted.Automations))
	for id, state := range persisted.Automations {
		states[id] = stateOf(state)
	}

	return states
}

func persistedState(s schedule.State) config.AutomationStateV1 {
	outcome := ""
	if s.Outcome != schedule.OutcomeNone {
		outcome = s.Outcome.String()
	}

	return config.AutomationStateV1{
		LastDue:  s.LastDue,
		LastRun:  s.LastRun,
		Outcome:  outcome,
		Manual:   s.Manual,
		Message:  s.Message,
		Error:    s.Error,
		Attempts: s.Attempts,
		Paused:   s.Paused,
	}
}

func stateOf(s config.AutomationStateV1) schedule.State {
	return schedule.State{
		LastDue:  s.LastDue,
		LastRun:  s.LastRun,
		Outcome:  schedule.ParseOutcome(s.Outcome),
		Manual:   s.Manual,
		Message:  s.Message,
		Error:    s.Error,
		Attempts: s.Attempts,
		Paused:   s.Paused,
	}
}

// getAutomations builds what the page shows. Runs on the plugin's goroutine.
func (p *plugin) getAutomations() data.Automations {
	result := data.Automations{Items: make([]data.Automation, len(p.statuses))}

	for i, status := range p.statuses {
		item := viewOf(status)
		result.Items[i] = item

		// Failing is those being tried again, and an event-driven automation is not: it shows its error on its card and has an alert.
		if status.Outcome == schedule.OutcomeFailed && !status.Manual && !status.EventDriven {
			result.Failing++
		}
	}

	return result
}

func viewOf(s schedule.Status) data.Automation {
	// An event-driven automation has none.
	catchUp := ""
	if !s.EventDriven {
		catchUp = catchUpInWords(s.CatchUp)
	}

	return data.Automation{
		ID:           s.ID,
		Name:         s.Name,
		Description:  s.Description,
		Trigger:      s.Trigger,
		CatchUp:      catchUp,
		Next:         s.Next,
		HasNext:      s.HasNext,
		LastRun:      s.LastRun,
		CanRearm:     s.CanRearm,
		Paused:       s.Paused,
		EventDriven:  s.EventDriven,
		Outcome:      s.Outcome.String(),
		OutcomeClass: outcomeClass(s.Outcome),
		Manual:       s.Manual,
		Message:      s.Message,
		Error:        s.Error,
		Attempts:     s.Attempts,
	}
}

func outcomeClass(o schedule.Outcome) string {
	switch o {
	case schedule.OutcomeSucceeded:
		return "succeeded"
	case schedule.OutcomeFailed:
		return "failed"
	case schedule.OutcomeGaveUp:
		return "gave-up"
	case schedule.OutcomeMissed:
		return "missed"
	case schedule.OutcomeWaiting:
		return "waiting"
	case schedule.OutcomeExpired:
		return "expired"
	default:
		return "none"
	}
}

// catchUpInWords is how long after it is due an automation still runs, as a person would say it.
func catchUpInWords(d time.Duration) string {
	if d <= 0 {
		return "only in the minute it is due"
	}

	d = d.Round(time.Minute)
	hours, minutes := int(d/time.Hour), int(d%time.Hour/time.Minute)

	switch {
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
