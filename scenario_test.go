package automations

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	_ "time/tzdata"

	pmaasautomations "github.com/avanha/pmaas-automations"
	"github.com/avanha/pmaas-automations/sun"
	"github.com/avanha/pmaas-automations/trigger"
	"github.com/avanha/pmaas-plugin-fakes/fake"
	"github.com/avanha/pmaas-spi/clock"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/events"
)

// These tests run whole days through the real plugin and the real automations, on the virtual clock of a testing/synctest
// bubble: time moves only when everything is waiting for it, so a day takes milliseconds and nothing depends on how
// fast the machine is. Only the server around the plugin and the devices are fakes.

var lafayette = sun.Location{Latitude: 39.9967, Longitude: -105.0944}

// inColorado makes the bubble's local time Mountain time, which is where the sun is worked out, and moves the bubble's
// clock to midnight on the given date there. It returns that midnight.
func inColorado(t *testing.T, year int, month time.Month, day int) (midnight time.Time, denver *time.Location) {
	t.Helper()

	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}

	previous := time.Local
	time.Local = denver

	t.Cleanup(func() { time.Local = previous })

	midnight = time.Date(year, month, day, 0, 0, 0, 0, denver)
	time.Sleep(time.Until(midnight))

	return midnight, denver
}

// atticFanAutomations is what test-assembly has for the attic fan, over the given devices.
func atticFanAutomations(fan device.ISwitch, attic, outside *fake.Thermometer) *PluginConfig {
	conf := NewPluginConfig()

	conf.Add(pmaasautomations.New(
		"attic-fan", "Run the attic fan when the attic is over 90°F and warmer than outside, until it is under 88°F",
		trigger.Windowed(
			trigger.Every(time.Minute),
			trigger.AtTimeOfDay(8, 0),
			trigger.Sun(lafayette, sun.Sunset, time.Hour)),
		pmaasautomations.Regulate(
			fan,
			// On when the attic is over 90 and warmer than outside...
			pmaasautomations.AllOf(
				pmaasautomations.TemperatureAbove(attic, pmaasautomations.Fahrenheit(90), 10*time.Minute),
				pmaasautomations.Warmer(attic, outside, 10*time.Minute)),
			// ...and off when it is under 88, or outside is as warm.
			pmaasautomations.AnyOf(
				pmaasautomations.TemperatureBelow(attic, pmaasautomations.Fahrenheit(88), 10*time.Minute),
				pmaasautomations.Not(pmaasautomations.Warmer(attic, outside, 10*time.Minute))),
			5*time.Minute),
		pmaasautomations.WithCatchUp(0),
	))

	conf.Add(pmaasautomations.New(
		"attic-fan-off", "Turn off the attic fan an hour after sunset",
		trigger.Sun(lafayette, sun.Sunset, time.Hour),
		pmaasautomations.TurnOff(fan),
	))

	return &conf
}

// bubbleFixture is a started plugin on the bubble's clock, and a way to let a minute go by.
type bubbleFixture struct {
	t         *testing.T
	container *fakeContainer
	plugin    *plugin
}

func startInBubble(t *testing.T, conf *PluginConfig, persisted *fakeContainer) *bubbleFixture {
	t.Helper()

	f := &bubbleFixture{t: t, container: newFakeContainer(t)}

	if persisted != nil && len(persisted.saved) > 0 {
		kept := persisted.saved[len(persisted.saved)-1]
		f.container.persisted = &kept
	}

	// The plugin's own clock is the real one, which in the bubble is the virtual one.
	f.plugin = NewPlugin(*conf).(*plugin)
	f.plugin.Init(f.container)
	f.plugin.Start()

	synctest.Wait()

	return f
}

// minute lets a minute go by, and tells the plugin so, as the server's clock would. Anything the test wants to happen at
// the top of the minute (a sensor reporting) is done first, by before.
func (f *bubbleFixture) minute(before func(now time.Time)) time.Time {
	f.t.Helper()

	time.Sleep(time.Minute)

	now := time.Now()

	if before != nil {
		before(now)
	}

	if err := f.container.receiver(&events.EventInfo{Event: clock.MinuteChangedEvent{Time: now}}); err != nil {
		f.t.Fatal(err)
	}

	synctest.Wait()

	return now
}

func (f *bubbleFixture) stop() {
	f.t.Helper()

	<-f.plugin.Stop()
}

type fanChange struct {
	at time.Time
	on bool
}

func (c fanChange) String() string {
	state := "off"
	if c.on {
		state = "on"
	}

	return fmt.Sprintf("%s %s", c.at.Format("15:04"), state)
}

// outsideAt is a summer day's outside temperature in °F: 61 at 3:30 AM, 89 at 3:30 PM.
func outsideAt(at time.Time) float64 {
	hours := float64(at.Hour()) + float64(at.Minute())/60

	return 75 + 14*math.Sin(2*math.Pi*(hours-9.5)/24)
}

// atticAt is the attic's: the outside plus what the sun on the roof adds, and about what's left in the evening.
func atticAt(at time.Time) float64 {
	hours := float64(at.Hour()) + float64(at.Minute())/60

	sunshine := 0.0
	if hours > 8 && hours < 19 {
		sunshine = math.Sin(math.Pi * (hours - 8) / 11)
	}

	return outsideAt(at) + 4 + 26*sunshine
}

func hm(day time.Time, hour int, minute int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, day.Location())
}

func TestAtticFan_ADayOfHeatFlappingAStaleSensorAndAWarmOutside(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		midnight, _ := inColorado(t, 2000, time.June, 21)

		fan := fake.NewSwitch("Attic fan", device.SwitchStateOff)
		attic := fake.NewThermometer("Attic", 20)
		outside := fake.NewThermometer("Outside", 20)

		closing, ok := trigger.Sun(lafayette, sun.Sunset, time.Hour).Next(midnight)
		if !ok {
			t.Fatal("no sunset")
		}

		// What the day does to the temperatures, on top of the curves above:
		trueAttic := func(at time.Time) float64 {
			switch {
			case !at.Before(hm(midnight, 10, 30)) && at.Before(hm(midnight, 11, 0)):
				// The attic hovers around 90, a degree either side one minute and the next, for half an hour.
				if at.Minute()%2 == 0 {
					return 88.5
				}

				return 92.5
			case !at.Before(hm(midnight, 12, 0)) && at.Before(hm(midnight, 12, 30)):
				return 95
			default:
				return atticAt(at)
			}
		}
		trueOutside := func(at time.Time) float64 {
			if !at.Before(hm(midnight, 12, 0)) && at.Before(hm(midnight, 12, 30)) {
				return 100 // a hot wind: warmer than the attic
			}

			return outsideAt(at)
		}
		// The attic's thermometer goes quiet from 14:00 to 14:30 (it reports at 14:00 and then not again until 14:30).
		silent := func(at time.Time) bool {
			return at.After(hm(midnight, 14, 0)) && at.Before(hm(midnight, 14, 30))
		}

		f := startInBubble(t, atticFanAutomations(fan, attic, outside), nil)

		var changes []fanChange
		last := device.SwitchStateOff
		wantOn := false

		report := func(at time.Time) {
			outside.Set(float32(pmaasautomations.Fahrenheit(float32(trueOutside(at))).Celsius()))

			if !silent(at) {
				attic.Set(float32(pmaasautomations.Fahrenheit(float32(trueAttic(at))).Celsius()))
			}
		}

		type tick struct {
			at          time.Time
			state       device.SwitchState
			wantOn      bool
			readingsOld bool
		}

		var ticks []tick

		for time.Now().Before(midnight.Add(24 * time.Hour)) {
			now := f.minute(report)

			state := fan.GetSwitchData().State
			if state != last {
				changes = append(changes, fanChange{at: now, on: state == device.SwitchStateOn})
				last = state
			}

			// On above 90 and warmer than outside; off below 88 or when outside is as warm; and between, as it was.
			switch {
			case trueAttic(now) > 90 && trueAttic(now) > trueOutside(now):
				wantOn = true
			case trueAttic(now) < 88 || trueAttic(now) <= trueOutside(now):
				wantOn = false
			}

			ticks = append(ticks, tick{at: now, state: state, wantOn: wantOn, readingsOld: silent(now)})
		}

		f.stop()

		summary := fmt.Sprint(changes)

		// 1. Never changed more often than every five minutes.
		for i := 1; i < len(changes); i++ {
			if gap := changes[i].at.Sub(changes[i-1].at); gap < 5*time.Minute {
				t.Errorf("the fan changed %v after it last did: %s", gap, summary)
			}
		}

		// 2. Only changed in the window, apart from being switched off as it closes.
		for _, c := range changes {
			inWindow := !c.at.Before(hm(midnight, 8, 0)) && c.at.Before(closing)
			closesNow := c.at.Equal(closing) && !c.on

			if !inWindow && !closesNow {
				t.Errorf("the fan changed at %s, outside the window (8:00 to %s): %s", c.at.Format("15:04"), closing.Format("15:04"), summary)
			}
		}

		// 3. Not touched while the attic's thermometer was quiet and its last reading too old to go by.
		for _, c := range changes {
			if !c.at.Before(hm(midnight, 14, 11)) && c.at.Before(hm(midnight, 14, 30)) {
				t.Errorf("the fan changed at %s, while the attic thermometer was quiet: %s", c.at.Format("15:04"), summary)
			}
		}

		// 4. Off at the end of the day.
		if fan.GetSwitchData().State != device.SwitchStateOff {
			t.Errorf("the fan ended the day on: %s", summary)
		}

		// 5. Where the right answer has been the same for six minutes, and the readings are good, the fan has it.
		stableSince := ticks[0].at

		for i, tk := range ticks {
			if i > 0 && tk.wantOn != ticks[i-1].wantOn {
				stableSince = tk.at
			}

			inWindow := !tk.at.Before(hm(midnight, 8, 0)) && tk.at.Before(closing)
			readingsGood := !tk.readingsOld && !(tk.at.After(hm(midnight, 14, 0)) && tk.at.Before(hm(midnight, 14, 31)))

			if inWindow && readingsGood && tk.at.Sub(stableSince) >= 6*time.Minute && (tk.state == device.SwitchStateOn) != tk.wantOn {
				t.Errorf("at %s the fan is %v but has wanted to be %v since %s: %s",
					tk.at.Format("15:04"), tk.state, map[bool]string{true: "on", false: "off"}[tk.wantOn], stableSince.Format("15:04"), summary)
			}
		}

		// 6. It did its job: on in the heat, and not flapping at all through the half hour the attic hovered either side of 90,
		// which is within the gap between coming on at 90 and going off at 88.
		if len(changes) < 3 {
			t.Errorf("the fan hardly changed all day: %s", summary)
		}

		flapping := 0

		for _, c := range changes {
			if !c.at.Before(hm(midnight, 10, 30)) && c.at.Before(hm(midnight, 11, 0)) {
				flapping++
			}
		}

		if flapping > 1 {
			t.Errorf("the fan changed %d times in the half hour the attic hovered around 90: %s", flapping, summary)
		}

		t.Logf("fan changes: %s", summary)
		t.Logf("window closed at %s", closing.Format("15:04"))
	})
}

func TestAtticFan_ARestartDoesNotLetItChangeTheFanSoonerThanFiveMinutesAfterItLastChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		midnight, _ := inColorado(t, 2000, time.June, 21)

		fan := fake.NewSwitch("Attic fan", device.SwitchStateOff)
		attic := fake.NewThermometer("Attic", 30)
		outside := fake.NewThermometer("Outside", 20)

		set := func(atticF, outsideF float32) {
			attic.Set(pmaasautomations.Fahrenheit(atticF).Celsius())
			outside.Set(pmaasautomations.Fahrenheit(outsideF).Celsius())
		}

		f := startInBubble(t, atticFanAutomations(fan, attic, outside), nil)

		// Get to just before the window, and let the heat start: the fan comes on as it opens.
		for time.Now().Before(hm(midnight, 7, 59)) {
			f.minute(nil)
		}

		set(95, 80)
		f.minute(func(time.Time) { set(95, 80) }) // 8:00

		if fan.GetSwitchData().State != device.SwitchStateOn {
			t.Fatalf("the fan is %v at %s", fan.GetSwitchData().State, time.Now().Format("15:04"))
		}

		turnedOn := time.Now()

		// The server restarts a minute later, as the attic cools, with everything it kept.
		f.minute(func(time.Time) { set(85, 80) })
		f.stop()

		f = startInBubble(t, atticFanAutomations(fan, attic, outside), f.container)

		// The fan changed at 8:00, so the first it can change again is 8:05.
		for time.Since(turnedOn) < 5*time.Minute {
			f.minute(func(time.Time) { set(85, 80) })

			if time.Since(turnedOn) < 5*time.Minute && fan.GetSwitchData().State != device.SwitchStateOn {
				t.Fatalf("the fan was changed at %s, only %v after it last was", time.Now().Format("15:04"), time.Since(turnedOn))
			}
		}

		if fan.GetSwitchData().State != device.SwitchStateOff {
			t.Errorf("the fan is %v at %s, when the attic has been cool for minutes", fan.GetSwitchData().State, time.Now().Format("15:04"))
		}

		f.stop()
	})
}

func TestAtticFan_IsSwitchedOffWhenTheWindowClosesAndStaysOffAfterEvenThoughTheAtticIsStillHot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		midnight, _ := inColorado(t, 2000, time.December, 21)

		fan := fake.NewSwitch("Attic fan", device.SwitchStateOff)
		attic := fake.NewThermometer("Attic", 40)
		outside := fake.NewThermometer("Outside", 5)

		f := startInBubble(t, atticFanAutomations(fan, attic, outside), nil)

		closing, ok := trigger.Sun(lafayette, sun.Sunset, time.Hour).Next(midnight)
		if !ok {
			t.Fatal("no sunset")
		}

		// A hot attic all day and night (a fire, say) against a cold outside: the fan should run all through the window.
		var changes []fanChange
		last := device.SwitchStateOff

		for time.Now().Before(midnight.Add(24 * time.Hour)) {
			now := f.minute(func(time.Time) {
				attic.Set(pmaasautomations.Fahrenheit(120).Celsius())
				outside.Set(pmaasautomations.Fahrenheit(20).Celsius())
			})

			if state := fan.GetSwitchData().State; state != last {
				changes = append(changes, fanChange{at: now, on: state == device.SwitchStateOn})
				last = state
			}
		}

		f.stop()

		// On as the window opens at 8:00 and off as it closes, an hour after a sunset at about 4:38pm, and then no more:
		// the automation that follows the temperature stops looking when the window closes.
		want := []fanChange{{at: hm(midnight, 8, 0), on: true}, {at: closing, on: false}}

		if fmt.Sprint(changes) != fmt.Sprint(want) {
			t.Errorf("the fan changed %v, want %v", changes, want)
		}

		if !strings.HasPrefix(closing.Format("15:04"), "17:") {
			t.Errorf("the window closes at %s on the shortest day", closing.Format("15:04"))
		}
	})
}

func TestAtticFan_AnOutsideThermometerThatWentQuietAfterACoolReadingDoesNotStartTheFan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		midnight, _ := inColorado(t, 2000, time.June, 21)

		fan := fake.NewSwitch("Attic fan", device.SwitchStateOff)
		attic := fake.NewThermometer("Attic", 20)
		outside := fake.NewThermometer("Outside", 20)

		f := startInBubble(t, atticFanAutomations(fan, attic, outside), nil)

		for time.Now().Before(hm(midnight, 9, 0)) {
			f.minute(nil)
		}

		// At 9:00 the outside reports a cool 60°F, and is not heard from again for most of an hour. The attic is warm
		// but not hot.
		outside.Set(pmaasautomations.Fahrenheit(60).Celsius())

		for time.Now().Before(hm(midnight, 9, 30)) {
			f.minute(func(time.Time) { attic.Set(pmaasautomations.Fahrenheit(80).Celsius()) })
		}

		// Then the attic gets hot. Compared with that old 60°F it is far warmer than outside; but the outside has been
		// quiet for half an hour, and may be 100°F for all anyone knows, so the fan is left alone.
		for time.Now().Before(hm(midnight, 9, 45)) {
			f.minute(func(time.Time) { attic.Set(pmaasautomations.Fahrenheit(95).Celsius()) })

			if state := fan.GetSwitchData().State; state != device.SwitchStateOff {
				t.Fatalf("the fan was %v at %s, on the strength of an outside reading half an hour old", state, time.Now().Format("15:04"))
			}
		}

		// The outside speaks up: it is hot too, and the fan stays off.
		f.minute(func(time.Time) {
			attic.Set(pmaasautomations.Fahrenheit(95).Celsius())
			outside.Set(pmaasautomations.Fahrenheit(100).Celsius())
		})

		if fan.GetSwitchData().State != device.SwitchStateOff {
			t.Errorf("the fan is %v, with the outside hotter than the attic", fan.GetSwitchData().State)
		}

		// And when it cools, the fan comes on.
		f.minute(func(time.Time) {
			attic.Set(pmaasautomations.Fahrenheit(95).Celsius())
			outside.Set(pmaasautomations.Fahrenheit(85).Celsius())
		})

		if fan.GetSwitchData().State != device.SwitchStateOn {
			t.Errorf("the fan is %v, with the attic hotter than the outside", fan.GetSwitchData().State)
		}

		f.stop()
	})
}

func TestAtticFan_AFanThatCanNotBeReachedRaisesOneAlertAndClearsItWhenItIsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		midnight, _ := inColorado(t, 2000, time.June, 21)

		fan := fake.NewSwitch("Attic fan", device.SwitchStateOff)
		attic := fake.NewThermometer("Attic", 20)
		outside := fake.NewThermometer("Outside", 20)

		f := startInBubble(t, atticFanAutomations(fan, attic, outside), nil)

		cool := func(time.Time) {
			attic.Set(pmaasautomations.Fahrenheit(70).Celsius())
			outside.Set(pmaasautomations.Fahrenheit(65).Celsius())
		}
		hot := func(time.Time) {
			attic.Set(pmaasautomations.Fahrenheit(100).Celsius())
			outside.Set(pmaasautomations.Fahrenheit(80).Celsius())
		}

		// A cool morning, with the fan off, up to and including 9:59.
		for time.Now().Before(hm(midnight, 9, 59)) {
			f.minute(cool)
		}

		// At 10:00 the attic heats up and the fan can't be reached. The automation wants to turn it on every minute, and
		// every minute it can't. It never gives up, since there is always a new minute it is due.
		fan.FailWith(errors.New("the plug is offline"))

		f.minute(hot) // 10:00, the first failure

		for failures := 2; failures <= 9; failures++ {
			f.minute(hot)
		}

		if raised, _ := f.container.alerts(); len(raised) != 0 {
			t.Fatalf("an alert at %s, after nine failures: %+v", time.Now().Format("15:04"), raised)
		}

		f.minute(hot) // 10:09, the tenth

		raised, _ := f.container.alerts()
		if len(raised) != 1 || raised[0].Title != "Automation is failing: Run the attic fan when the attic is over 90°F and warmer than outside, until it is under 88°F" ||
			!strings.Contains(raised[0].Message, "failed 10 times in a row") || !strings.Contains(raised[0].Message, "the plug is offline") {
			t.Fatalf("at %s: %+v", time.Now().Format("15:04"), raised)
		}

		// Two more hours of it: still the one alert.
		for time.Now().Before(hm(midnight, 12, 0)) {
			f.minute(hot)
		}

		if raised, _ := f.container.alerts(); len(raised) != 1 {
			t.Errorf("%d alerts after hours of failing", len(raised))
		}

		// The plug comes back at noon: the fan comes on at once, and the alert is cleared.
		fan.FailWith(nil)
		f.minute(hot)

		if fan.GetSwitchData().State != device.SwitchStateOn {
			t.Errorf("the fan is %v at %s", fan.GetSwitchData().State, time.Now().Format("15:04"))
		}

		if _, cleared := f.container.alerts(); len(cleared) == 0 {
			t.Errorf("the alert was not cleared")
		}

		f.stop()
	})
}
