package automations

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-automations/config"
	"github.com/avanha/pmaas-plugin-automations/data"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/alert"
	"github.com/avanha/pmaas-spi/automation"
	"github.com/avanha/pmaas-spi/clock"
	"github.com/avanha/pmaas-spi/events"
)

var day = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

func at(hour, minute int) time.Time {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// fakeContainer has the two goroutines the plugin deals with, the server's and its own, as goroutines of their own, so
// that the work lands where it does in the real thing.
type fakeContainer struct {
	spi.IPMAASContainer

	serverCh chan func()
	pluginCh chan func()

	// onServer is set while the server's goroutine is running something.
	onServer atomic.Bool

	mu           sync.Mutex
	predicate    events.EventPredicate
	receiver     events.EventReceiver
	deregistered []int
	saved        []config.PersistentConfigV1
	persisted    *config.PersistentConfigV1
	broadcasts   []any
	broadcastAt  []time.Time

	routes        map[string]*spi.HttpHandlerOptions
	jsonRoutes    []string
	rendererTypes []reflect.Type
}

func newFakeContainer(t *testing.T) *fakeContainer {
	t.Helper()

	c := &fakeContainer{serverCh: make(chan func(), 64), pluginCh: make(chan func(), 64)}

	go func() {
		for f := range c.serverCh {
			c.onServer.Store(true)
			f()
			c.onServer.Store(false)
		}
	}()

	go func() {
		for f := range c.pluginCh {
			f()
		}
	}()

	t.Cleanup(func() { close(c.serverCh); close(c.pluginCh) })

	return c
}

func (c *fakeContainer) EnqueueOnServerGoRoutine(invocations []func()) error {
	for _, f := range invocations {
		c.serverCh <- f
	}

	return nil
}

func (c *fakeContainer) EnqueueOnPluginGoRoutine(f func()) error {
	c.pluginCh <- f

	return nil
}

// settle waits until what has been queued, and what that queues in turn, has been done.
func (c *fakeContainer) settle() {
	for range 3 {
		serverDone := make(chan struct{})
		c.serverCh <- func() { close(serverDone) }
		<-serverDone

		pluginDone := make(chan struct{})
		c.pluginCh <- func() { close(pluginDone) }
		<-pluginDone
	}
}

func (c *fakeContainer) ProvideContentFS(fs.FS, string) {}
func (c *fakeContainer) EnableStaticContent(string)     {}

func (c *fakeContainer) AddRouteWithOptions(path string, _ http.HandlerFunc, options *spi.HttpHandlerOptions) {
	if c.routes == nil {
		c.routes = make(map[string]*spi.HttpHandlerOptions)
	}

	c.routes[path] = options
}

func (c *fakeContainer) AddJsonRoute(path string, _ spi.RequestObjectFactoryFunc, _ spi.JsonHandlerFunc) {
	c.jsonRoutes = append(c.jsonRoutes, path)
}

func (c *fakeContainer) RegisterEntityRenderer(entityType reflect.Type, _ spi.EntityRendererFactory) {
	c.rendererTypes = append(c.rendererTypes, entityType)
}

func (c *fakeContainer) RegisterEventReceiver(predicate events.EventPredicate, receiver events.EventReceiver) (int, error) {
	c.predicate, c.receiver = predicate, receiver

	return 7, nil
}

func (c *fakeContainer) DeregisterEventReceiver(handle int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.deregistered = append(c.deregistered, handle)

	return nil
}

func (c *fakeContainer) BroadcastEvent(_ string, event any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.broadcasts = append(c.broadcasts, event)
	c.broadcastAt = append(c.broadcastAt, time.Now())

	return nil
}

func (c *fakeContainer) SaveConfig(value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.saved = append(c.saved, value.(config.PersistentConfigV1))

	return nil
}

// LoadConfig hands back what was "saved" the way the real one does: by filling in what the plugin's factory makes for
// the type's name.
func (c *fakeContainer) LoadConfig(factory func(typeName string) any) (any, error) {
	if c.persisted == nil {
		return nil, errors.New("nothing saved")
	}

	persistedType := reflect.TypeFor[config.PersistentConfigV1]()

	target := factory(persistedType.PkgPath() + "/" + persistedType.Name())
	if target == nil {
		return nil, errors.New("not a type the plugin knows")
	}

	encoded, err := json.Marshal(c.persisted)
	if err != nil {
		return nil, err
	}

	return target, json.Unmarshal(encoded, target)
}

func (c *fakeContainer) ClosedCallbackChannel() chan func() {
	ch := make(chan func())
	close(ch)

	return ch
}

func (c *fakeContainer) lastSaved(t *testing.T) config.PersistentConfigV1 {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.saved) == 0 {
		t.Fatal("nothing was saved")
	}

	return c.saved[len(c.saved)-1]
}

func (c *fakeContainer) saveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.saved)
}

func (c *fakeContainer) alerts() (raised []alert.RaisedEvent, cleared []alert.ClearedEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, b := range c.broadcasts {
		switch e := b.(type) {
		case alert.RaisedEvent:
			raised = append(raised, e)
		case alert.ClearedEvent:
			cleared = append(cleared, e)
		}
	}

	return raised, cleared
}

// scripted is a trigger due at the times it is given.
type scripted struct{ dues []time.Time }

func (s scripted) Previous(t time.Time) (time.Time, bool) {
	var found time.Time
	ok := false

	for _, due := range s.dues {
		if !due.After(t) && (!ok || due.After(found)) {
			found, ok = due, true
		}
	}

	return found, ok
}

func (s scripted) Next(t time.Time) (time.Time, bool) {
	var found time.Time
	ok := false

	for _, due := range s.dues {
		if due.After(t) && (!ok || due.Before(found)) {
			found, ok = due, true
		}
	}

	return found, ok
}

func (s scripted) Description() string { return "scripted" }

// fakeAutomation records where it was run, and fails as it is told to.
type fakeAutomation struct {
	id        string
	trigger   scripted
	catchUp   time.Duration
	container *fakeContainer

	mu          sync.Mutex
	runs        int
	failWith    error
	ranOnServer []bool
}

func (f *fakeAutomation) ID() string                  { return f.id }
func (f *fakeAutomation) Name() string                { return "Name of " + f.id }
func (f *fakeAutomation) Description() string         { return "" }
func (f *fakeAutomation) Trigger() automation.Trigger { return f.trigger }
func (f *fakeAutomation) CatchUp() time.Duration      { return f.catchUp }

func (f *fakeAutomation) Run() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs++
	f.ranOnServer = append(f.ranOnServer, f.container.onServer.Load())

	if f.failWith != nil {
		return "", f.failWith
	}

	return "did it", nil
}

func (f *fakeAutomation) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.runs
}

func (f *fakeAutomation) setFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failWith = err
}

// fixture is a started plugin whose time is the test's.
type fixture struct {
	container *fakeContainer
	plugin    *plugin
	a         *fakeAutomation
	nowMu     sync.Mutex
	now       time.Time
}

func newFixture(t *testing.T, persisted *config.PersistentConfigV1, dues ...time.Time) *fixture {
	t.Helper()

	f := &fixture{container: newFakeContainer(t), now: at(12, 0)}
	f.container.persisted = persisted
	f.a = &fakeAutomation{id: "lights", trigger: scripted{dues}, catchUp: time.Hour, container: f.container}

	conf := NewPluginConfig()
	conf.Add(f.a)

	f.plugin = NewPlugin(conf).(*plugin)
	f.plugin.now = func() time.Time { f.nowMu.Lock(); defer f.nowMu.Unlock(); return f.now }

	return f
}

func (f *fixture) start() {
	f.plugin.Init(f.container)
	f.plugin.Start()
	f.container.settle()
}

func (f *fixture) setNow(t time.Time) {
	f.nowMu.Lock()
	defer f.nowMu.Unlock()

	f.now = t
}

// minute is the clock saying a minute has begun, to the plugin's receiver.
func (f *fixture) minute(t time.Time) {
	f.setNow(t)

	err := f.container.receiver(&events.EventInfo{Event: clock.MinuteChangedEvent{Time: t}})
	if err != nil {
		panic(err)
	}

	f.container.settle()
}

func stateBefore(due time.Time) *config.PersistentConfigV1 {
	return &config.PersistentConfigV1{Automations: map[string]config.AutomationStateV1{"lights": {LastDue: due}}}
}

func TestInit_ProvidesThePageAndTheRoutesToRunOneAndToReenableOne(t *testing.T) {
	f := newFixture(t, nil)
	f.plugin.Init(f.container)

	options, ok := f.container.routes[""]
	if !ok || options.MenuLabel != "Automations" || !options.SupportsXsrfValidation {
		t.Errorf("list route: %+v", options)
	}

	if !reflect.DeepEqual(f.container.jsonRoutes, []string{"run", "rearm"}) {
		t.Errorf("json routes: %v", f.container.jsonRoutes)
	}

	if !reflect.DeepEqual(f.container.rendererTypes, []reflect.Type{reflect.TypeFor[data.Automations]()}) {
		t.Errorf("renderers: %v", f.container.rendererTypes)
	}

	if f.plugin.ShortName() != "automations" {
		t.Errorf("name %q", f.plugin.ShortName())
	}
}

func TestStart_ListensOnlyForTheMinute(t *testing.T) {
	f := newFixture(t, nil)
	f.start()

	for name, event := range map[string]any{
		"hour": clock.HourChangedEvent{}, "day": clock.DayChangedEvent{}, "alert": alert.RaisedEvent{}, "other": "x",
	} {
		if f.container.predicate(&events.EventInfo{Event: event}) {
			t.Errorf("the %s event is wanted", name)
		}
	}

	if !f.container.predicate(&events.EventInfo{Event: clock.MinuteChangedEvent{}}) {
		t.Error("the minute event isn't wanted")
	}
}

func TestStart_ANewAutomationIsNotRunForATimeItWasDueBeforeItWasThereButIsRemembered(t *testing.T) {
	f := newFixture(t, nil, at(11, 0))
	f.start()

	if f.a.runCount() != 0 {
		t.Fatalf("ran %d times", f.a.runCount())
	}

	saved := f.container.lastSaved(t).Automations["lights"]
	if !saved.LastDue.Equal(at(11, 0)) {
		t.Errorf("kept %+v", saved)
	}
}

func TestStart_CatchesUpWhatWasDueWhileTheServerWasDownAtOnce(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(11, 30))
	f.start()

	if f.a.runCount() != 1 {
		t.Fatalf("ran %d times", f.a.runCount())
	}

	saved := f.container.lastSaved(t).Automations["lights"]
	if !saved.LastDue.Equal(at(11, 30)) || saved.Outcome != "succeeded" || saved.Message != "did it" {
		t.Errorf("kept %+v", saved)
	}
}

func TestStart_DoesNotCatchUpWhatIsTooOld(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(9, 0)) // three hours ago, and the catch-up is an hour
	f.start()

	if f.a.runCount() != 0 {
		t.Fatalf("ran %d times", f.a.runCount())
	}

	if saved := f.container.lastSaved(t).Automations["lights"]; saved.Outcome != "missed" {
		t.Errorf("kept %+v", saved)
	}
}

func TestStart_DoesNotRunWhatWasAlreadyRunBeforeTheRestart(t *testing.T) {
	f := newFixture(t, stateBefore(at(11, 30)), at(11, 30))
	f.start()

	if f.a.runCount() != 0 {
		t.Errorf("ran %d times", f.a.runCount())
	}
}

func TestStart_AStateThatCantBeReadIsAsIfThereWasNone(t *testing.T) {
	f := newFixture(t, nil, at(11, 30))
	f.start()

	if f.a.runCount() != 0 {
		t.Errorf("ran %d times", f.a.runCount())
	}
}

func TestMinute_RunsWhatIsDueOnTheServersGoroutineAndOnlyOnce(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0))
	f.start()

	f.minute(at(16, 59))

	if f.a.runCount() != 0 {
		t.Fatalf("ran early")
	}

	f.minute(at(17, 0))
	f.minute(at(17, 1))
	f.minute(at(17, 2))

	if f.a.runCount() != 1 || !f.a.ranOnServer[0] {
		t.Errorf("ran %d times, on the server's goroutine: %v", f.a.runCount(), f.a.ranOnServer)
	}
}

func TestMinute_OnlySavesWhenSomethingChanged(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0))
	f.start()

	before := f.container.saveCount()

	f.minute(at(12, 1))
	f.minute(at(12, 2))

	if f.container.saveCount() != before {
		t.Errorf("saved %d more times with nothing new", f.container.saveCount()-before)
	}

	f.minute(at(17, 0))

	if f.container.saveCount() != before+1 {
		t.Errorf("saved %d times for the run", f.container.saveCount()-before)
	}
}

func TestMinute_AFailureIsRetriedAndAnAlertIsRaisedOnlyWhenItGivesUpAndClearedWhenItNextWorks(t *testing.T) {
	boom := errors.New("the plug is offline")
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0), at(17, 0).Add(24*time.Hour))
	f.a.catchUp = 2 * time.Minute
	f.a.setFailure(boom)
	f.start()

	f.minute(at(17, 0))
	f.minute(at(17, 1))
	f.minute(at(17, 2))

	if raised, _ := f.container.alerts(); len(raised) != 0 || f.a.runCount() != 3 {
		t.Fatalf("raised %d alerts while still trying, ran %d times", len(raised), f.a.runCount())
	}

	f.minute(at(17, 3))

	raised, _ := f.container.alerts()
	if len(raised) != 1 || raised[0].Source != "automations" || raised[0].Key != "did-not-run:lights" ||
		raised[0].Severity != alert.SeverityWarning || !strings.Contains(raised[0].Title, "Name of lights") ||
		!strings.Contains(raised[0].Message, "the plug is offline") || !strings.Contains(raised[0].Message, "3 times") {
		t.Fatalf("raised %+v", raised)
	}

	if f.a.runCount() != 3 {
		t.Errorf("tried again after giving up: %d", f.a.runCount())
	}

	// The next day it works.
	f.a.setFailure(nil)
	f.minute(at(17, 0).Add(24 * time.Hour))

	if _, cleared := f.container.alerts(); len(cleared) == 0 || cleared[len(cleared)-1].Key != "did-not-run:lights" {
		t.Errorf("cleared %+v", cleared)
	}
}

func TestStop_StopsListeningAndWhatIsStillQueuedDoesNotRun(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0))
	f.start()

	<-f.plugin.Stop()

	if !reflect.DeepEqual(f.container.deregistered, []int{7}) {
		t.Errorf("deregistered %v", f.container.deregistered)
	}

	f.minute(at(17, 0))

	if f.a.runCount() != 0 {
		t.Errorf("ran %d times after the plugin was told to stop", f.a.runCount())
	}
}

func TestGetAutomations_ShowsHowEachIsGettingOn(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0), at(17, 0).Add(24*time.Hour))
	f.a.setFailure(errors.New("offline"))
	f.start()
	f.minute(at(17, 0))

	shown, err := (&automationStoreAdapter{parent: f.plugin}).GetAutomations()
	if err != nil {
		t.Fatal(err)
	}

	if len(shown.Items) != 1 || shown.Failing != 1 {
		t.Fatalf("shown %+v", shown)
	}

	item := shown.Items[0]
	if item.ID != "lights" || item.Name != "Name of lights" || item.Trigger != "scripted" || item.CatchUp != "1h" ||
		item.Outcome != "failed" || item.OutcomeClass != "failed" || item.Error != "offline" || item.Attempts != 1 ||
		!item.HasNext || !item.Next.Equal(at(17, 0).Add(24*time.Hour)) {
		t.Errorf("item %+v", item)
	}
}

func TestRunNow_RunsOnTheServersGoroutineAndReportsWhatCameOfIt(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0))
	f.start()

	result, err := (&automationStoreAdapter{parent: f.plugin}).RunNow("lights")
	if err != nil || !result.Succeeded || result.Message != "did it" || result.Error != "" {
		t.Fatalf("result %+v, err %v", result, err)
	}

	if f.a.runCount() != 1 || !f.a.ranOnServer[0] {
		t.Errorf("ran %d times, on the server's goroutine: %v", f.a.runCount(), f.a.ranOnServer)
	}

	f.container.settle()

	if saved := f.container.lastSaved(t).Automations["lights"]; !saved.Manual || saved.Outcome != "succeeded" {
		t.Errorf("kept %+v", saved)
	}

	f.a.setFailure(errors.New("offline"))

	result, err = (&automationStoreAdapter{parent: f.plugin}).RunNow("lights")
	if err != nil || result.Succeeded || result.Error != "offline" {
		t.Errorf("result %+v, err %v", result, err)
	}
}

func TestGetAutomations_AFailureByHandIsNotAnAutomationFailingBecauseNothingRetriesIt(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0))
	f.a.setFailure(errors.New("offline"))
	f.start()

	adapter := &automationStoreAdapter{parent: f.plugin}

	if _, err := adapter.RunNow("lights"); err != nil {
		t.Fatal(err)
	}

	f.container.settle()

	shown, _ := adapter.GetAutomations()
	if shown.Failing != 0 || shown.Items[0].Outcome != "failed" || !shown.Items[0].Manual {
		t.Errorf("shown %+v", shown)
	}
}

func TestRunNow_RefusesWhatIsNotThereAndWhatIsStopping(t *testing.T) {
	f := newFixture(t, nil)
	f.start()

	adapter := &automationStoreAdapter{parent: f.plugin}

	if _, err := adapter.RunNow("nope"); err == nil {
		t.Error("an unknown automation was run")
	}

	<-f.plugin.Stop()

	if _, err := adapter.RunNow("lights"); err == nil || f.a.runCount() != 0 {
		t.Errorf("err %v, ran %d times", err, f.a.runCount())
	}
}

func TestPersistedState_RoundTripsAndToleratesWordsItDoesNotKnow(t *testing.T) {
	f := newFixture(t, nil)
	f.start()

	for _, word := range []string{"succeeded", "failed", "gave up", "missed"} {
		if got := persistedState(stateOf(config.AutomationStateV1{Outcome: word})).Outcome; got != word {
			t.Errorf("%q came back as %q", word, got)
		}
	}

	if got := persistedState(stateOf(config.AutomationStateV1{Outcome: "something new"})).Outcome; got != "" {
		t.Errorf("an unknown outcome came back as %q", got)
	}

	full := config.AutomationStateV1{
		LastDue: at(1, 0), LastRun: at(2, 0), Outcome: "failed", Manual: true, Message: "m", Error: "e", Attempts: 4,
	}

	if got := persistedState(stateOf(full)); got != full {
		t.Errorf("got %+v, want %+v", got, full)
	}
}

func TestConfig_AddRefusesWhatCouldNotRun(t *testing.T) {
	container := newFakeContainer(t)
	good := &fakeAutomation{id: "a", trigger: scripted{}, container: container}

	conf := NewPluginConfig()
	conf.Add(good)

	for name, add := range map[string]func(){
		"duplicate": func() { conf.Add(&fakeAutomation{id: "a", trigger: scripted{}, container: container}) },
		"invalid":   func() { conf.Add(&fakeAutomation{trigger: scripted{}, container: container}) },
		"nil":       func() { conf.Add(nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: didn't panic", name)
				}
			}()

			add()
		}()
	}

	var zero PluginConfig
	zero.Add(good) // works without NewPluginConfig
}

func TestCatchUpInWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "only in the minute it is due", -time.Minute: "only in the minute it is due",
		time.Minute: "1m", 45 * time.Minute: "45m", time.Hour: "1h", 90 * time.Minute: "1h 30m", 6 * time.Hour: "6h",
	} {
		if got := catchUpInWords(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestWaiting_IsShownQuietlyNotSavedNotAlertedAndActsWhenReady(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(1, 0))
	f.a.catchUp = 7 * time.Hour
	f.a.setFailure(automation.NotReady("78.1 is not below 75.0"))
	f.setNow(at(0, 59)) // before the window opens
	f.start()
	f.minute(at(1, 0))

	adapter := &automationStoreAdapter{parent: f.plugin}
	saves := f.container.saveCount()

	for minute := 1; minute < 30; minute++ {
		f.minute(at(1, minute))
	}

	if f.container.saveCount() != saves {
		t.Errorf("saved %d more times while only waiting", f.container.saveCount()-saves)
	}

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Errorf("alerts raised: %+v", raised)
	}

	shown, _ := adapter.GetAutomations()
	item := shown.Items[0]

	if item.Outcome != "waiting" || item.OutcomeClass != "waiting" || item.Message != "78.1 is not below 75.0" ||
		item.Error != "" || shown.Failing != 0 || f.a.runCount() != 30 {
		t.Errorf("item %+v, failing %d, ran %d times", item, shown.Failing, f.a.runCount())
	}

	// It's ready.
	f.a.setFailure(nil)
	f.minute(at(1, 30))

	shown, _ = adapter.GetAutomations()
	if shown.Items[0].Outcome != "succeeded" || f.container.saveCount() != saves+1 {
		t.Errorf("item %+v, saved %d more times", shown.Items[0], f.container.saveCount()-saves)
	}

	if saved := f.container.lastSaved(t).Automations["lights"]; saved.Outcome != "succeeded" || !saved.LastDue.Equal(at(1, 0)) {
		t.Errorf("kept %+v", saved)
	}
}

func TestWaiting_WhenTheTimeRunsOutItExpiresWithoutAnAlert(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(1, 0))
	f.a.catchUp = 5 * time.Minute
	f.a.setFailure(automation.NotReady("too warm"))
	f.setNow(at(0, 59)) // before the window opens
	f.start()

	for minute := 0; minute <= 6; minute++ {
		f.minute(at(1, minute))
	}

	shown, _ := (&automationStoreAdapter{parent: f.plugin}).GetAutomations()
	item := shown.Items[0]

	if item.Outcome != "condition not met" || item.OutcomeClass != "expired" || item.Message != "too warm" {
		t.Errorf("item %+v", item)
	}

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Errorf("alerts raised: %+v", raised)
	}

	if saved := f.container.lastSaved(t).Automations["lights"]; saved.Outcome != "condition not met" || !saved.LastDue.Equal(at(1, 0)) {
		t.Errorf("kept %+v", saved)
	}

	// And what was kept is understood after a restart.
	kept := f.container.lastSaved(t)
	again := newFixture(t, &kept, at(1, 0), at(1, 0).Add(24*time.Hour))
	again.setNow(at(1, 10))
	again.start()

	shown, _ = (&automationStoreAdapter{parent: again.plugin}).GetAutomations()
	if shown.Items[0].Outcome != "condition not met" {
		t.Errorf("after a restart: %+v", shown.Items[0])
	}
}

func TestRearm_ARunAutomationGoesBackToWaitingForTheSameTime(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(1, 0))
	f.a.catchUp = 7 * time.Hour
	f.setNow(at(0, 59)) // before the window opens
	f.start()
	f.minute(at(1, 0)) // it runs, and is done for the night

	adapter := &automationStoreAdapter{parent: f.plugin}

	shown, _ := adapter.GetAutomations()
	if !shown.Items[0].CanRearm || f.a.runCount() != 1 {
		t.Fatalf("item %+v, ran %d times", shown.Items[0], f.a.runCount())
	}

	// Someone turned the fan back on, and wants the automation to take it from here: but it isn't ready.
	f.a.setFailure(automation.NotReady("the fan is off"))
	f.setNow(at(3, 0))

	result, err := adapter.Rearm("lights")
	if err != nil || !result.Armed || result.Why != "" {
		t.Fatalf("result %+v, err %v", result, err)
	}

	f.container.settle()

	shown, _ = adapter.GetAutomations()
	item := shown.Items[0]

	if item.Outcome != "waiting" || item.Message != "the fan is off" || item.CanRearm || f.a.runCount() != 2 {
		t.Errorf("item %+v, ran %d times", item, f.a.runCount())
	}

	if !f.a.ranOnServer[1] {
		t.Error("it was run off the server's goroutine")
	}

	// What is kept says it is due again, so a restart doesn't forget.
	if saved := f.container.lastSaved(t).Automations["lights"]; !saved.LastDue.Before(at(1, 0)) {
		t.Errorf("kept %+v", saved)
	}

	// It's ready, and acts.
	f.a.setFailure(nil)
	f.minute(at(3, 1))

	shown, _ = adapter.GetAutomations()
	if shown.Items[0].Outcome != "succeeded" || f.a.runCount() != 3 {
		t.Errorf("item %+v, ran %d times", shown.Items[0], f.a.runCount())
	}
}

func TestRearm_SaysWhyWhenItCanNotAndRefusesWhatIsNotThere(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(1, 0), at(1, 0).Add(24*time.Hour))
	f.a.catchUp = time.Hour
	f.setNow(at(0, 59)) // before the window opens
	f.start()
	f.minute(at(1, 0))
	f.setNow(at(9, 0))

	adapter := &automationStoreAdapter{parent: f.plugin}

	result, err := adapter.Rearm("lights")
	if err != nil || result.Armed || !strings.Contains(result.Why, "its time has passed") {
		t.Errorf("result %+v, err %v", result, err)
	}

	if _, err := adapter.Rearm("nope"); err == nil {
		t.Error("an unknown automation")
	}

	<-f.plugin.Stop()

	if _, err := adapter.Rearm("lights"); err == nil {
		t.Error("re-enabled while stopping")
	}
}

func TestPersistedState_TheWordsForWaitingAndExpiredRoundTrip(t *testing.T) {
	for _, word := range []string{"succeeded", "failed", "gave up", "missed", "waiting", "condition not met"} {
		if got := persistedState(stateOf(config.AutomationStateV1{Outcome: word})).Outcome; got != word {
			t.Errorf("%q came back as %q", word, got)
		}
	}
}

// failingFixture is a plugin whose one automation, due at 17:00 with an hour to catch up in, fails every time it is run.
func failingFixture(t *testing.T, configure func(*PluginConfig)) *fixture {
	t.Helper()

	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0))
	f.a.setFailure(errors.New("the plug is offline"))

	if configure != nil {
		configure(&f.plugin.config)
	}

	f.start()

	return f
}

func failuresFrom17(f *fixture, count int) {
	for minute := 0; minute < count; minute++ {
		f.minute(at(17, minute))
	}
}

func TestFailing_AnAlertIsRaisedOnTheTenthFailureInARowAndOnlyOnce(t *testing.T) {
	f := failingFixture(t, nil)

	failuresFrom17(f, 9)

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Fatalf("an alert after nine failures: %+v", raised)
	}

	f.minute(at(17, 9))

	raised, _ := f.container.alerts()
	if len(raised) != 1 || raised[0].Source != "automations" || raised[0].Key != "did-not-run:lights" ||
		raised[0].Severity != alert.SeverityWarning || raised[0].Title != "Automation is failing: Name of lights" ||
		!strings.Contains(raised[0].Message, "failed 10 times in a row") || !strings.Contains(raised[0].Message, "the plug is offline") {
		t.Fatalf("raised %+v", raised)
	}

	// It keeps failing, and keeps being tried: the alert stays, and is not raised again each minute.
	for minute := 10; minute < 40; minute++ {
		f.minute(at(17, minute))
	}

	if raised, _ := f.container.alerts(); len(raised) != 1 {
		t.Errorf("%d alerts after forty failures", len(raised))
	}

	// It works again: the alert is cleared.
	f.a.setFailure(nil)
	f.minute(at(17, 40))

	if _, cleared := f.container.alerts(); len(cleared) != 1 || cleared[0].Key != "did-not-run:lights" {
		t.Errorf("cleared %+v", cleared)
	}
}

func TestFailing_TheNumberOfFailuresThatRaisesTheAlertCanBeSet(t *testing.T) {
	f := failingFixture(t, func(c *PluginConfig) { c.AlertAfterFailures = 3 })

	failuresFrom17(f, 2)

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Fatalf("an alert after two failures: %+v", raised)
	}

	f.minute(at(17, 2))

	if raised, _ := f.container.alerts(); len(raised) != 1 || !strings.Contains(raised[0].Message, "failed 3 times") {
		t.Errorf("raised %+v", raised)
	}
}

func TestFailing_ANegativeNumberMeansNoAlertUntilItGivesUp(t *testing.T) {
	f := failingFixture(t, func(c *PluginConfig) { c.AlertAfterFailures = -1 })
	f.a.catchUp = 20 * time.Minute

	failuresFrom17(f, 21)

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Fatalf("an alert before it gave up: %+v", raised)
	}

	f.minute(at(17, 21))

	raised, _ := f.container.alerts()
	if len(raised) != 1 || raised[0].Title != "Automation did not run: Name of lights" {
		t.Errorf("raised %+v", raised)
	}
}

func TestFailing_AConfigurationThatNeverSaidUsesTheDefault(t *testing.T) {
	if got := (PluginConfig{}).alertAfterFailures(); got != DefaultAlertAfterFailures || DefaultAlertAfterFailures != 10 {
		t.Errorf("zero value: %d", got)
	}

	if got := NewPluginConfig(); got.AlertAfterFailures != DefaultAlertAfterFailures || got.alertAfterFailures() != 10 {
		t.Errorf("new: %d, %d", got.AlertAfterFailures, got.alertAfterFailures())
	}

	if got := (PluginConfig{AlertAfterFailures: 4}).alertAfterFailures(); got != 4 {
		t.Errorf("set: %d", got)
	}

	if got := (PluginConfig{AlertAfterFailures: -5}).alertAfterFailures(); got != 0 {
		t.Errorf("negative: %d", got)
	}
}

func TestFailing_TheAlertSpeaksOfGivingUpWhenItDoesAndKeepsTheSameKeySoItIsOneAlert(t *testing.T) {
	f := failingFixture(t, func(c *PluginConfig) { c.AlertAfterFailures = 2 })
	f.a.catchUp = 5 * time.Minute

	failuresFrom17(f, 7) // 17:00..17:06: failing, then gives up as 17:06 is past the five minutes

	raised, _ := f.container.alerts()
	if len(raised) != 2 || raised[0].Key != raised[1].Key || raised[0].Title != "Automation is failing: Name of lights" ||
		raised[1].Title != "Automation did not run: Name of lights" {
		t.Errorf("raised %+v", raised)
	}
}

func TestFailing_AFailureByHandIsNotCountedTowardTheAlert(t *testing.T) {
	f := failingFixture(t, func(c *PluginConfig) { c.AlertAfterFailures = 1 })

	adapter := &automationStoreAdapter{parent: f.plugin}

	for range 5 {
		if _, err := adapter.RunNow("lights"); err != nil {
			t.Fatal(err)
		}
	}

	f.container.settle()

	if raised, _ := f.container.alerts(); len(raised) != 0 {
		t.Errorf("raised %+v", raised)
	}
}

func TestAlerts_WorkFromWhenThePluginHasStartedUntilItStops(t *testing.T) {
	f := newFixture(t, nil)
	broadcaster := f.plugin.config.Alerts()

	// Taken while configuring, before the plugin is made: the assembly's copy of the configuration is the one the plugin has.
	if err := broadcaster.BroadcastEvent("automations", alert.RaisedEvent{Source: "automations", Key: "k"}); !errors.Is(err, errAlertsNotAvailable) {
		t.Fatalf("before the plugin started: %v", err)
	}

	f.start()

	if err := alert.Raise(broadcaster, "automations", "k", alert.SeverityWarning, "title", "message"); err != nil {
		t.Fatalf("while running: %v", err)
	}

	raised, _ := f.container.alerts()
	if len(raised) != 1 || raised[0].Key != "k" || raised[0].Title != "title" {
		t.Errorf("raised %+v", raised)
	}

	<-f.plugin.Stop()

	if err := alert.Clear(broadcaster, "automations", "k"); !errors.Is(err, errAlertsNotAvailable) {
		t.Errorf("after the plugin stopped: %v", err)
	}
}

func TestAlerts_AreTheSameOneEachTimeAndWorkForAConfigurationNotMadeWithNew(t *testing.T) {
	var zero PluginConfig

	first, second := zero.Alerts(), zero.Alerts()
	if first != second || first == nil {
		t.Errorf("%v %v", first, second)
	}

	// A plugin made from a configuration whose alerts were never asked for has none to set.
	f := newFixture(t, nil)
	f.start()
	<-f.plugin.Stop()
}

func TestAlerts_APluginMadeFromTheConfigurationSharesItsAlerts(t *testing.T) {
	container := newFakeContainer(t)
	conf := NewPluginConfig()
	alerts := conf.Alerts()
	conf.Add(&fakeAutomation{id: "a", trigger: scripted{}, container: container})

	p := NewPlugin(conf).(*plugin)
	p.Init(container)

	if err := alert.Raise(alerts, "automations", "k", alert.SeverityInfo, "t", "m"); err != nil {
		t.Errorf("an alert broadcaster taken before the plugin was made: %v", err)
	}
}

// alertEntry is one alert event as it happened.
type alertEntry struct {
	at       time.Time
	cleared  bool
	key      string
	severity alert.Severity
	title    string
}

func (e alertEntry) String() string {
	if e.cleared {
		return fmt.Sprintf("%s cleared %s", e.at.Format("15:04"), e.key)
	}

	return fmt.Sprintf("%s %s %s", e.at.Format("15:04"), e.severity, e.key)
}

// alertLog is the alerts raised and cleared, in order, with when.
func (c *fakeContainer) alertLog() []alertEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	var log []alertEntry

	for i, b := range c.broadcasts {
		switch e := b.(type) {
		case alert.RaisedEvent:
			log = append(log, alertEntry{at: c.broadcastAt[i], key: e.Key, severity: e.Severity, title: e.Title})
		case alert.ClearedEvent:
			log = append(log, alertEntry{at: c.broadcastAt[i], cleared: true, key: e.Key})
		}
	}

	return log
}

func TestSuccess_ClearsOnlyAnAlertThatWasRaisedNotOneThatNeverWas(t *testing.T) {
	f := newFixture(t, stateBefore(at(0, 0)), at(17, 0), at(17, 0).Add(24*time.Hour))
	f.a.catchUp = time.Hour
	f.setNow(at(16, 59))
	f.start()

	// An automation that has never failed runs, and there is nothing to clear.
	f.minute(at(17, 0))

	if _, cleared := f.container.alerts(); len(cleared) != 0 {
		t.Fatalf("cleared an alert that was never raised: %+v", cleared)
	}

	// The next day it fails ten times, so there is an alert, and then works: that is cleared, once.
	f.a.setFailure(errors.New("offline"))

	for minute := 0; minute < 10; minute++ {
		f.minute(at(17, minute).Add(24 * time.Hour))
	}

	f.a.setFailure(nil)
	f.minute(at(17, 10).Add(24 * time.Hour))

	if _, cleared := f.container.alerts(); len(cleared) != 1 {
		t.Errorf("cleared %+v", cleared)
	}
}
