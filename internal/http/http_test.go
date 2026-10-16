package http

import (
	"bytes"
	"errors"
	"net/http"
	"path"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/avanha/pmaas-plugin-automations/data"
	"github.com/avanha/pmaas-plugin-automations/internal/common"
)

// render renders the template the way PMAAS does. Its template engine plugin uses text/template, which doesn't escape
// anything, so a template has to do that itself: rendering with html/template here would escape automatically and
// hide a template that doesn't.
func render(t *testing.T, automations data.Automations) string {
	t.Helper()

	name := path.Base(automationsTemplate.Paths[0])
	tmpl, err := template.New(name).Funcs(automationsTemplate.FuncMap).ParseFS(contentFS, "content/"+automationsTemplate.Paths[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out bytes.Buffer
	if err := tmpl.ExecuteTemplate(&out, name, &automations); err != nil {
		t.Fatalf("execute: %v", err)
	}

	return out.String()
}

func item(name string) data.Automation {
	return data.Automation{
		ID: "id-" + name, Name: name, Trigger: "15 minutes before sunset", CatchUp: "1h",
		Next: time.Now().Add(3 * time.Hour).Truncate(time.Minute), HasNext: true,
		LastRun: time.Now().Add(-26 * time.Hour), Outcome: "succeeded", OutcomeClass: "succeeded", Message: "turned on Porch",
	}
}

func TestTemplate_Empty(t *testing.T) {
	out := render(t, data.Automations{})

	if !strings.Contains(out, "No automations are configured") || strings.Contains(out, "run-button") {
		t.Errorf("empty page:\n%s", out)
	}
}

func TestTemplate_AnAutomationShowsWhenWhatHappenedAndHowToRunIt(t *testing.T) {
	out := render(t, data.Automations{Items: []data.Automation{item("Holiday lights on")}})

	for _, want := range []string{
		"1 automation", "Holiday lights on", `outcome-badge succeeded">succeeded`, "15 minutes before sunset",
		"(in 2h 59m)", "(1d 2h ago)", "turned on Porch", "1h",
		`data-id="id-Holiday lights on" data-name="Holiday lights on"`, `class="run-button"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	if strings.Contains(out, "failing") || strings.Contains(out, `class="error"`) {
		t.Errorf("a healthy automation shown as failing:\n%s", out)
	}
}

func TestTemplate_AFailingOneSaysWhy(t *testing.T) {
	failing := item("Lights")
	failing.Outcome, failing.OutcomeClass = "failed", "failed"
	failing.Error, failing.Attempts = "kp-3: the device is offline", 3

	out := render(t, data.Automations{Items: []data.Automation{failing, item("Other")}, Failing: 1})

	for _, want := range []string{"2 automations", "1 failing", `outcome-badge failed">failed`, "kp-3: the device is offline", "(attempt 3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestTemplate_NeverRunAndNeverDue(t *testing.T) {
	fresh := item("Fresh")
	fresh.LastRun, fresh.HasNext, fresh.Outcome, fresh.OutcomeClass, fresh.Message = time.Time{}, false, "not yet", "none", ""

	out := render(t, data.Automations{Items: []data.Automation{fresh}})

	for _, want := range []string{"never", "not due in the next year", "not yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestTemplate_AManualRunSaysSo(t *testing.T) {
	manual := item("Lights")
	manual.Manual = true

	if out := render(t, data.Automations{Items: []data.Automation{manual}}); !strings.Contains(out, ", by hand") {
		t.Errorf("not marked:\n%s", out)
	}
}

// Names and notes come from the assembly, and errors from devices, where some of the text may be partly under the
// control of something outside. It must reach the page as text.
func TestTemplate_EscapesEverythingThatIsText(t *testing.T) {
	hostile := item(`<script>alert("name")</script>`)
	hostile.ID = `x" onclick="alert(1)`
	hostile.Description = `<img src=x onerror=alert(1)>`
	hostile.Trigger = `<b>trigger</b>`
	hostile.Message = `<i>message</i>`
	hostile.Error = `<u>error</u>`
	hostile.CatchUp = `<s>catch</s>`
	hostile.Outcome = `<blink>outcome</blink>`
	hostile.OutcomeClass = `x" onmouseover="alert(2)`

	out := render(t, data.Automations{Items: []data.Automation{hostile}})

	for _, dangerous := range []string{
		"<script>", "<img src=x", "<b>trigger", "<i>message", "<u>error", "<s>catch", "<blink>", `onclick="alert(1)"`,
		`onmouseover="alert(2)"`,
	} {
		if strings.Contains(out, dangerous) {
			t.Errorf("unescaped %q in:\n%s", dangerous, out)
		}
	}
}

type fakeStore struct {
	ran     []string
	rearmed []string
	result  common.RunResult
	rearm   common.RearmResult
	err     error
}

func (s *fakeStore) GetAutomations() (data.Automations, error) { return data.Automations{}, s.err }

func (s *fakeStore) Rearm(id string) (common.RearmResult, error) {
	s.rearmed = append(s.rearmed, id)

	return s.rearm, s.err
}

func (s *fakeStore) RunNow(id string) (common.RunResult, error) {
	s.ran = append(s.ran, id)

	return s.result, s.err
}

func TestRunRequest(t *testing.T) {
	post := &http.Request{Method: http.MethodPost}

	t.Run("runs it and says how it went", func(t *testing.T) {
		store := &fakeStore{result: common.RunResult{Succeeded: true, Message: "turned on Porch"}}
		h := &Handler{store: store}

		response, err := h.handleHttpRunRequest(nil, post, &runRequest{ID: "lights"})
		if err != nil {
			t.Fatal(err)
		}

		got := response.(runResponse)
		if !got.Succeeded || got.Message != "turned on Porch" || got.Error != "" {
			t.Errorf("response %+v", got)
		}

		if len(store.ran) != 1 || store.ran[0] != "lights" {
			t.Errorf("ran %v", store.ran)
		}
	})

	t.Run("an automation that failed is a response, not an error", func(t *testing.T) {
		store := &fakeStore{result: common.RunResult{Error: "the plug is offline"}}
		h := &Handler{store: store}

		response, err := h.handleHttpRunRequest(nil, post, &runRequest{ID: "lights"})
		if err != nil || response.(runResponse).Error != "the plug is offline" || response.(runResponse).Succeeded {
			t.Errorf("response %+v, err %v", response, err)
		}
	})

	t.Run("only POST", func(t *testing.T) {
		store := &fakeStore{}
		h := &Handler{store: store}

		if _, err := h.handleHttpRunRequest(nil, &http.Request{Method: http.MethodGet}, &runRequest{ID: "lights"}); err == nil {
			t.Error("a GET was accepted")
		}

		if len(store.ran) != 0 {
			t.Error("a rejected request still ran something")
		}
	})

	t.Run("needs an ID", func(t *testing.T) {
		for _, body := range []any{&runRequest{}, nil, "not a request"} {
			store := &fakeStore{}
			h := &Handler{store: store}

			if _, err := h.handleHttpRunRequest(nil, post, body); err == nil {
				t.Errorf("%v was accepted", body)
			}

			if len(store.ran) != 0 {
				t.Errorf("%v still ran something", body)
			}
		}
	})

	t.Run("what went wrong inside is not told to the browser", func(t *testing.T) {
		h := &Handler{store: &fakeStore{err: errors.New("secret internal detail: /var/pmaas/db")}}

		_, err := h.handleHttpRunRequest(nil, post, &runRequest{ID: "lights"})
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/var") {
			t.Errorf("error %v", err)
		}
	})
}

func TestWords(t *testing.T) {
	for _, c := range []struct {
		span   time.Duration
		suffix string
		want   string
	}{
		{30 * time.Second, "ago", "just now"},
		{5*time.Minute + 20*time.Second, "ago", "5m ago"},
		{2*time.Hour + 5*time.Minute, "ago", "2h 5m ago"},
		{3*24*time.Hour + 4*time.Hour + time.Minute, "ago", "3d 4h ago"},
		{5 * time.Minute, "", "in 5m"},
		{25 * time.Hour, "", "in 1d 1h"},
		{20 * time.Second, "", "just now"},
	} {
		if got := words(c.span, c.suffix, "just now"); got != c.want {
			t.Errorf("%v %q: got %q, want %q", c.span, c.suffix, got, c.want)
		}
	}

	if got := relativeTime(time.Time{}); got != "never" {
		t.Errorf("a zero time: %q", got)
	}

	if got := untilTime(time.Now().Add(-time.Hour)); got != "now" {
		t.Errorf("a time that has passed: %q", got)
	}
}

func TestTemplate_WaitingShowsWhyAndOnlyAnAutomationThatCanBeReenabledGetsTheButton(t *testing.T) {
	waiting := item("Fan")
	waiting.Outcome, waiting.OutcomeClass, waiting.Message = "waiting", "waiting", "78.1°F is not below 75.0°F"

	done := item("Lights")
	done.CanRearm = true

	out := render(t, data.Automations{Items: []data.Automation{waiting, done}})

	for _, want := range []string{`outcome-badge waiting">waiting`, "78.1°F is not below 75.0°F", `automation waiting`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	if got := strings.Count(out, `class="rearm-button"`); got != 1 {
		t.Errorf("%d re-enable buttons, want 1:\n%s", got, out)
	}

	if !strings.Contains(out, `class="rearm-button" data-id="id-Lights"`) {
		t.Errorf("the button is not on the automation that can be re-enabled:\n%s", out)
	}
}

func TestRearmRequest(t *testing.T) {
	post := &http.Request{Method: http.MethodPost}

	t.Run("re-enables it and says so", func(t *testing.T) {
		store := &fakeStore{rearm: common.RearmResult{Armed: true}}
		h := &Handler{store: store}

		response, err := h.handleHttpRearmRequest(nil, post, &runRequest{ID: "fan"})
		if err != nil || !response.(rearmResponse).Armed {
			t.Fatalf("response %+v, err %v", response, err)
		}

		if len(store.rearmed) != 1 || store.rearmed[0] != "fan" {
			t.Errorf("rearmed %v", store.rearmed)
		}
	})

	t.Run("not being able to is a response with the reason, not an error", func(t *testing.T) {
		h := &Handler{store: &fakeStore{rearm: common.RearmResult{Why: "its time has passed"}}}

		response, err := h.handleHttpRearmRequest(nil, post, &runRequest{ID: "fan"})
		if err != nil || response.(rearmResponse).Armed || response.(rearmResponse).Why != "its time has passed" {
			t.Errorf("response %+v, err %v", response, err)
		}
	})

	t.Run("only POST, and an ID", func(t *testing.T) {
		store := &fakeStore{}
		h := &Handler{store: store}

		if _, err := h.handleHttpRearmRequest(nil, &http.Request{Method: http.MethodGet}, &runRequest{ID: "fan"}); err == nil {
			t.Error("a GET was accepted")
		}

		for _, body := range []any{&runRequest{}, nil, "x"} {
			if _, err := h.handleHttpRearmRequest(nil, post, body); err == nil {
				t.Errorf("%v was accepted", body)
			}
		}

		if len(store.rearmed) != 0 {
			t.Error("a rejected request still re-enabled something")
		}
	})

	t.Run("what went wrong inside is not told to the browser", func(t *testing.T) {
		h := &Handler{store: &fakeStore{err: errors.New("secret internal detail: /var/pmaas/db")}}

		_, err := h.handleHttpRearmRequest(nil, post, &runRequest{ID: "fan"})
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/var") {
			t.Errorf("error %v", err)
		}
	})
}
