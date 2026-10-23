# pmaas-plugin-automations
A plugin that runs automations, shows how they are getting on, and lets them be run by hand.

An automation (see `pmaas-spi/automation`) is plain Go in an assembly, or from a library of re-usable ones such as
`pmaas-automations`: a trigger that says when it is due ("15 minutes before sunset"), and something to do. The
assembly adds them to the plugin's configuration:

```go
conf := automations.NewPluginConfig()
conf.Add(pmaasautomations.New("holiday-lights-on", "Turn on the holiday lights",
	trigger.Sun(lafayette, sun.Sunset, -15*time.Minute),
	pmaasautomations.TurnOn(lights)))
coreConfig.AddPlugin(automations.NewPlugin(conf), config.PluginConfig{})
```

## How it runs them

Once a minute, on the server's clock, the plugin asks each automation when it was last due. If that is later than the
last time it dealt with, it runs it. That one question covers running on time, catching up after the machine slept,
and catching up after a restart.

- **Retry.** An automation that fails (a plug is offline, say) stays due, and is run again at the next minute.
- **Catch-up.** An automation is run for the time it was due only for as long after as its `CatchUp()` allows
  (an hour, by default). After that it gives up if it was failing, raising a warning on the alert console, or notes that
  it was missed if the server wasn't running.
- **Failing.** An automation that keeps failing raises an alert on the alert console after ten failed runs in a row
  (`PluginConfig.AlertAfterFailures`), which matters most for one that runs every minute, since that never gives up:
  there is always a new time it is due. The alert is cleared the next time it runs. One that gives up, because its
  catch-up passed, raises it then too, if it has not already.
- **Waiting.** An automation that is due but not ready (`automation.NotReady("the room is still warm")`) is asked
  again every minute, quietly: it shows as "waiting" with the reason, and is neither logged nor saved each minute. If
  its catch-up passes while it is still waiting it ends as "condition not met", which is not worth an alert, since it
  never had anything to do.
- **New automations don't run retroactively.** One seen for the first time is not run for a time it was due before it
  was there.
- **Restarts.** When each automation last got on is kept with the server's persistent configuration, so a restart
  neither forgets nor repeats.
- **One goroutine.** Automations are called only on the server's goroutine, the one that runs the assembly's code,
  one at a time, so they don't need locks.

The page, under "Automations", shows each one's trigger, when it is next due, when it last ran and how that went, and a
"Run now" button. Running one by hand doesn't affect when it is next due.

An automation that has dealt with the time it was last due, while that is still within its catch-up, also has a
"Re-enable" button, which makes it run again for that time: for when someone took manual control and wants the
automation to take the matter up again.

Each also has a "Pause" button. A paused automation is marked as paused on the page and is not run until it is resumed.
The times it is due while it is paused are dealt with as they come, so resuming it carries on from the next time it is
due, and does not catch up on the ones it was paused through. "Run now" still works on a paused automation, and an
alert about it not running is cleared when it is paused. Which are paused is kept across restarts.

## Automations that wait for an event

An automation can also be due when something happens, such as a button being pressed, instead of at a time (see
`automation.EventAutomation`, and `automations.NewOnEvent` and `trigger.ButtonPressed` in `pmaas-automations`):

```go
conf.AddEvent(pmaasautomations.NewOnEvent("couch-lamp", "Turn on the couch lamp",
	trigger.ButtonPressed(couchButton, device.PressSingle),
	pmaasautomations.TurnOn(lamp)))
```

- **It runs at once.** The plugin listens for the events they wait for, and hands each to the server's goroutine as it arrives, in order,
  so the automation runs within milliseconds, not at the next minute. Only events some automation is waiting for are handed
  over; the rest never reach the plugin.
- **No retry and no catch-up.** A press is wanted at the moment of the press, and it would be a surprise for the lamp to come on an hour
  later because a plug was offline. A failed run raises a warning alert on the alert console at once (a run by hand, from the page, does not:
  the person sees it), and the alert is cleared the next time it works.
- **Conditions.** Wrapped in `When(...)`, a press does nothing unless the condition holds. The page then shows "condition not met" and the
  reason, and there is no alert.
- **On the page** it has the same card as the others, with "when the Couch button is single-clicked" for its trigger and no next time, catch-up or
  re-enable. "Run now" runs it as if the event had come, and "Pause" stops events running it, which is kept across restarts. Events it misses while
  paused are not made up for.
- **The time automations are unchanged**, and an ID is unique across both kinds.

## Old browsers

The page is used on an iPad that stopped at iOS 9 (Safari 9), so `automations_automations.js` is plain ES5 with `XMLHttpRequest` (a script that uses an
arrow function, a class field or `fetch` doesn't run there at all, and the buttons can't be clicked), and the stylesheet spaces items with margins,
not `gap`, which needs Safari 14.1. Tests in `internal/http` (`oldbrowsers_test.go`, `js_test/`) look for what it doesn't have.
