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
- **New automations don't run retroactively.** One seen for the first time is not run for a time it was due before it
  was there.
- **Restarts.** When each automation last got on is kept with the server's persistent configuration, so a restart
  neither forgets nor repeats.
- **One goroutine.** Automations are called only on the server's goroutine, the one that runs the assembly's code,
  one at a time, so they don't need locks.

The page, under "Automations", shows each one's trigger, when it is next due, when it last ran and how that went, and a
"Run now" button. Running one by hand doesn't affect when it is next due.
