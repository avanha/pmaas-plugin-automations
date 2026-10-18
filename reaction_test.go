package automations

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-automations/config"
	"github.com/avanha/pmaas-spi/alert"
	"github.com/avanha/pmaas-spi/automation"
	"github.com/avanha/pmaas-spi/events"
)

// ringTrigger is due for a string event with this text.
type ringTrigger struct{ text string }

func (t ringTrigger) Matches(info *events.EventInfo) bool {
	s, ok := info.Event.(string)

	return ok && s == t.text
}

func (t ringTrigger) Description() string { return "when " + t.text + " is heard" }

// fakeReaction is an event-driven automation that notes where it was run.
type fakeReaction struct {
	id        string
	trigger   ringTrigger
	container *fakeContainer

	mu          sync.Mutex
	runs        int
	ranOnServer []bool
	failWith    error
}

func (f *fakeReaction) ID() string                            { return f.id }
func (f *fakeReaction) Name() string                          { return "Name of " + f.id }
func (f *fakeReaction) Description() string                   { return "" }
func (f *fakeReaction) EventTrigger() automation.EventTrigger { return f.trigger }

func (f *fakeReaction) Run() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs++
	f.ranOnServer = append(f.ranOnServer, f.container.onServer.Load())

	if f.failWith != nil {
		return "", f.failWith
	}

	return "did it", nil
}

func (f *fakeReaction) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.runs
}

func (f *fakeReaction) setFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failWith = err
}

type reactionFixture struct {
	*fixture
	r  *fakeReaction
	tt *testing.T
}

func newReactionFixture(t *testing.T, persisted *config.PersistentConfigV1) *reactionFixture {
	t.Helper()

	f := &fixture{container: newFakeContainer(t), now: at(12, 0)}
	f.container.persisted = persisted
	f.a = &fakeAutomation{id: "lights", trigger: scripted{nil}, catchUp: time.Hour, container: f.container}

	r := &fakeReaction{id: "doorbell", trigger: ringTrigger{"ring"}, container: f.container}

	conf := NewPluginConfig()
	conf.Add(f.a)
	conf.AddEvent(r)

	f.plugin = NewPlugin(conf).(*plugin)
	f.plugin.now = func() time.Time { f.nowMu.Lock(); defer f.nowMu.Unlock(); return f.now }
	f.start()

	return &reactionFixture{fixture: f, r: r, tt: t}
}

// happens is an event coming from the event manager, to the plugin's receiver, if its predicate wants it.
func (f *reactionFixture) happens(event any) {
	f.tt.Helper()

	f.container.mu.Lock()
	predicate, receiver := f.container.reactionPredicate, f.container.reactionReceiver
	f.container.mu.Unlock()

	if predicate == nil {
		f.tt.Fatal("the plugin isn't listening for the events its automations wait for")
	}

	info := &events.EventInfo{Event: event}
	if !predicate(info) {
		return
	}

	if err := receiver(info); err != nil {
		f.tt.Fatal(err)
	}

	f.container.settle()
}

func TestReaction_AnEventRunsTheAutomationOnTheServersGoroutineAndItIsShownAndKept(t *testing.T) {
	f := newReactionFixture(t, nil)

	f.setNow(at(12, 5))
	f.happens("ring")

	if f.r.runCount() != 1 || !f.r.ranOnServer[0] {
		t.Fatalf("ran %d times, on the server: %v", f.r.runCount(), f.r.ranOnServer)
	}

	adapter := &automationStoreAdapter{parent: f.plugin}

	shown, _ := adapter.GetAutomations()
	if len(shown.Items) != 2 {
		t.Fatalf("%d items", len(shown.Items))
	}

	item := shown.Items[1]
	if item.ID != "doorbell" || !item.EventDriven || item.Trigger != "when ring is heard" || item.Outcome != "succeeded" ||
		item.Message != "did it" || !item.LastRun.Equal(at(12, 5)) || item.HasNext || item.CanRearm || shown.Items[0].EventDriven ||
		item.CatchUp != "" || shown.Items[0].CatchUp == "" {
		t.Errorf("item %+v", item)
	}

	saved := f.container.lastSaved(t).Automations["doorbell"]
	if saved.Outcome != "succeeded" || !saved.LastRun.Equal(at(12, 5)) {
		t.Errorf("kept %+v", saved)
	}
}

func TestReaction_AnEventNobodyWaitsForIsNotEvenHandedOver(t *testing.T) {
	f := newReactionFixture(t, nil)

	f.happens("knock")
	f.happens(42)

	if f.r.runCount() != 0 {
		t.Errorf("ran %d times", f.r.runCount())
	}
}

func TestReaction_EventsRunInTheOrderTheyCame(t *testing.T) {
	f := newReactionFixture(t, nil)

	for range 5 {
		f.happens("ring")
	}

	if f.r.runCount() != 5 {
		t.Errorf("ran %d times", f.r.runCount())
	}
}

func TestReaction_AFailureRaisesAnAlertAtOnceAndIsNotRetriedAndIsClearedWhenItNextWorks(t *testing.T) {
	f := newReactionFixture(t, nil)

	f.r.setFailure(errors.New("the plug is offline"))
	f.happens("ring")

	raised, _ := f.container.alerts()
	if len(raised) != 1 || raised[0].Source != "automations" || raised[0].Key != "did-not-run:doorbell" ||
		raised[0].Severity != alert.SeverityWarning || !strings.Contains(raised[0].Message, "the plug is offline") ||
		raised[0].Title != "Automation failed: Name of doorbell" {
		t.Fatalf("raised %+v", raised)
	}

	// The minute does not run it again.
	f.minute(at(12, 1))

	if f.r.runCount() != 1 {
		t.Errorf("ran %d times", f.r.runCount())
	}

	adapter := &automationStoreAdapter{parent: f.plugin}

	shown, _ := adapter.GetAutomations()
	if item := shown.Items[1]; item.Outcome != "failed" || item.Error != "the plug is offline" || item.Attempts != 0 || shown.Failing != 0 {
		t.Errorf("item %+v, failing %d", item, shown.Failing)
	}

	f.r.setFailure(nil)
	f.happens("ring")

	if _, cleared := f.container.alerts(); len(cleared) != 1 || cleared[0].Key != "did-not-run:doorbell" {
		t.Errorf("cleared %+v", cleared)
	}
}

func TestReaction_AnEventThatIsNoReasonToActIsNotAnAlert(t *testing.T) {
	f := newReactionFixture(t, nil)

	f.r.setFailure(automation.NotReady("it is daylight"))
	f.happens("ring")

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Errorf("raised %+v", raised)
	}

	adapter := &automationStoreAdapter{parent: f.plugin}

	if shown, _ := adapter.GetAutomations(); shown.Items[1].Outcome != "condition not met" || shown.Items[1].Message != "it is daylight" {
		t.Errorf("item %+v", shown.Items[1])
	}
}

func TestReaction_PausingStopsItRunningAndIsKeptAndRestored(t *testing.T) {
	f := newReactionFixture(t, nil)
	adapter := &automationStoreAdapter{parent: f.plugin}

	if err := adapter.SetPaused("doorbell", true); err != nil {
		t.Fatal(err)
	}

	f.container.settle()

	f.happens("ring")

	if f.r.runCount() != 0 {
		t.Fatalf("a paused automation ran")
	}

	shown, _ := adapter.GetAutomations()
	if !shown.Items[1].Paused {
		t.Errorf("not shown as paused: %+v", shown.Items[1])
	}

	kept := f.container.lastSaved(t)
	if !kept.Automations["doorbell"].Paused {
		t.Fatalf("the pause was not kept: %+v", kept.Automations["doorbell"])
	}

	// A restart: still paused.
	again := newReactionFixture(t, &kept)

	again.happens("ring")

	if again.r.runCount() != 0 {
		t.Error("it ran after a restart while paused")
	}

	if err := (&automationStoreAdapter{parent: again.plugin}).SetPaused("doorbell", false); err != nil {
		t.Fatal(err)
	}

	again.container.settle()
	again.happens("ring")

	if again.r.runCount() != 1 {
		t.Errorf("ran %d times after resuming", again.r.runCount())
	}
}

func TestReaction_RunNowRunsItWithoutAnEventAndReenableSaysItHasNothingToDo(t *testing.T) {
	f := newReactionFixture(t, nil)
	adapter := &automationStoreAdapter{parent: f.plugin}

	result, err := adapter.RunNow("doorbell")
	if err != nil || !result.Succeeded || result.Message != "did it" || f.r.runCount() != 1 || !f.r.ranOnServer[0] {
		t.Fatalf("%+v %v, ran %d", result, err, f.r.runCount())
	}

	shown, _ := adapter.GetAutomations()
	if !shown.Items[1].EventDriven || shown.Items[1].Outcome != "succeeded" {
		t.Errorf("item %+v", shown.Items[1])
	}

	rearm, err := adapter.Rearm("doorbell")
	if err != nil || rearm.Armed || rearm.Why == "" {
		t.Errorf("%+v %v", rearm, err)
	}
}

func TestReaction_TheTimeAutomationsStillRunOnTheMinute(t *testing.T) {
	f := newReactionFixture(t, stateBefore(at(0, 0)))
	f.a.trigger = scripted{[]time.Time{at(12, 0).Add(time.Minute)}}

	f.minute(at(12, 1))

	if f.a.runCount() != 1 {
		t.Errorf("the time automation ran %d times", f.a.runCount())
	}
}

func TestReaction_StopStopsListeningForBothAndEventsAfterItDoNothing(t *testing.T) {
	f := newReactionFixture(t, nil)

	<-f.plugin.Stop()

	if !reflect.DeepEqual(f.container.deregistered, []int{7, 8}) {
		t.Errorf("deregistered %v", f.container.deregistered)
	}

	f.container.mu.Lock()
	receiver := f.container.reactionReceiver
	f.container.mu.Unlock()

	// An event that was already on its way when the plugin stopped is not acted on.
	if err := receiver(&events.EventInfo{Event: "ring"}); err != nil {
		t.Fatal(err)
	}

	f.container.settle()

	if f.r.runCount() != 0 {
		t.Errorf("ran %d times after stopping", f.r.runCount())
	}
}

func TestReaction_NoReceiverIsRegisteredWhenThereAreNoEventDrivenAutomations(t *testing.T) {
	f := newFixture(t, nil)
	f.start()

	if f.container.reactionReceiver != nil {
		t.Error("a receiver for events was registered with nothing to wait for them")
	}
}

func TestAddEvent_MistakesPanic(t *testing.T) {
	good := &fakeReaction{id: "a", trigger: ringTrigger{"x"}}

	for name, mistake := range map[string]func(){
		"invalid":           func() { c := NewPluginConfig(); c.AddEvent(&fakeReaction{id: "", trigger: ringTrigger{"x"}}) },
		"the same ID twice": func() { c := NewPluginConfig(); c.AddEvent(good); c.AddEvent(good) },
		"an ID a time automation has": func() {
			c := NewPluginConfig()
			c.Add(&fakeAutomation{id: "a", trigger: scripted{nil}})
			c.AddEvent(good)
		},
		"an ID an event one has, as a time automation": func() {
			c := NewPluginConfig()
			c.AddEvent(good)
			c.Add(&fakeAutomation{id: "a", trigger: scripted{nil}})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()

			mistake()
		}()
	}
}

func TestReaction_AFailureWhenRunByHandIsShownToThePersonWhoRanItAndNotAnAlert(t *testing.T) {
	f := newReactionFixture(t, nil)
	f.r.setFailure(errors.New("the plug is offline"))

	result, err := (&automationStoreAdapter{parent: f.plugin}).RunNow("doorbell")
	if err != nil || result.Succeeded || result.Error != "the plug is offline" {
		t.Fatalf("%+v %v", result, err)
	}

	f.container.settle()

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Errorf("raised %+v", raised)
	}
}
