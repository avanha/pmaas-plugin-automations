package automations

import (
	"encoding/json"
	"errors"
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

func TestInit_ProvidesThePageAndTheRouteToRunOne(t *testing.T) {
	f := newFixture(t, nil)
	f.plugin.Init(f.container)

	options, ok := f.container.routes[""]
	if !ok || options.MenuLabel != "Automations" || !options.SupportsXsrfValidation {
		t.Errorf("list route: %+v", options)
	}

	if !reflect.DeepEqual(f.container.jsonRoutes, []string{"run"}) {
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
