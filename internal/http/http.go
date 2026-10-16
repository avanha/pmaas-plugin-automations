package http

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/avanha/pmaas-plugin-automations/data"
	"github.com/avanha/pmaas-plugin-automations/internal/common"
	"github.com/avanha/pmaas-spi"
)

//go:embed content/static content/templates
var contentFS embed.FS

var automationsTemplate = spi.TemplateInfo{
	Name: "automations_automations",
	FuncMap: template.FuncMap{
		"RelativeTime": relativeTime,
		"UntilTime":    untilTime,
	},
	Paths:   []string{"templates/automations_automations.htmlt"},
	Styles:  []string{"css/automations_automations.css"},
	Scripts: []string{"js/automations_automations.js"},
}

// Handler owns the plugin's HTTP surface: the page that shows the automations, and the route that runs one by hand.
type Handler struct {
	container spi.IPMAASContainer
	store     common.AutomationStore
}

func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) Init(container spi.IPMAASContainer, store common.AutomationStore) {
	h.container = container
	h.store = store

	container.ProvideContentFS(&contentFS, "content")
	container.EnableStaticContent("static")
	container.AddRouteWithOptions(
		"",
		h.handleHttpListRequest,
		&spi.HttpHandlerOptions{SupportsXsrfValidation: true, MenuLabel: "Automations"})
	container.AddJsonRoute(
		"run",
		func() any { return &runRequest{} },
		h.handleHttpRunRequest)
	container.AddJsonRoute(
		"rearm",
		func() any { return &runRequest{} },
		h.handleHttpRearmRequest)
	container.RegisterEntityRenderer(
		reflect.TypeOf((*data.Automations)(nil)).Elem(),
		h.automationsRendererFactory)
}

func (h *Handler) handleHttpListRequest(w http.ResponseWriter, r *http.Request) {
	automations, err := h.store.GetAutomations()

	if err != nil {
		fmt.Printf("automations.http handleHttpListRequest: unable to retrieve automations: %v\n", err)
		automations = data.Automations{}
	}

	h.container.RenderList(w, r, spi.RenderListOptions{Title: "Automations"}, []any{&automations})
}

type runRequest struct {
	ID string
}

type runResponse struct {
	// Succeeded is whether it ran. Message is the note it gave, and Error why it didn't run.
	Succeeded bool
	Message   string
	Error     string
}

func (h *Handler) handleHttpRunRequest(_ http.ResponseWriter, r *http.Request, request any) (any, error) {
	if r.Method != http.MethodPost {
		return nil, errors.New("method not allowed")
	}

	body, ok := request.(*runRequest)

	if !ok || body.ID == "" {
		return nil, errors.New("an ID is required")
	}

	result, err := h.store.RunNow(body.ID)

	if err != nil {
		// What is wrong with the request is for the person who made it; everything else is in the log.
		fmt.Printf("automations.http: unable to run %q: %v\n", body.ID, err)

		return nil, errors.New("unable to run it")
	}

	return runResponse{Succeeded: result.Succeeded, Message: result.Message, Error: result.Error}, nil
}

type rearmResponse struct {
	// Armed is whether it will run again, and Why why not, if not.
	Armed bool
	Why   string
}

func (h *Handler) handleHttpRearmRequest(_ http.ResponseWriter, r *http.Request, request any) (any, error) {
	if r.Method != http.MethodPost {
		return nil, errors.New("method not allowed")
	}

	body, ok := request.(*runRequest)

	if !ok || body.ID == "" {
		return nil, errors.New("an ID is required")
	}

	result, err := h.store.Rearm(body.ID)

	if err != nil {
		// What is wrong with the request is for the person who made it; everything else is in the log.
		fmt.Printf("automations.http: unable to re-enable %q: %v\n", body.ID, err)

		return nil, errors.New("unable to re-enable it")
	}

	return rearmResponse{Armed: result.Armed, Why: result.Why}, nil
}

func (h *Handler) automationsRendererFactory() (spi.EntityRenderer, error) {
	compiledTemplate, err := h.container.GetTemplate(&automationsTemplate)

	if err != nil {
		return spi.EntityRenderer{}, fmt.Errorf("unable to load automations_automations template: %v", err)
	}

	renderer := func(w io.Writer, entity any) error {
		automations, ok := entity.(*data.Automations)

		if !ok {
			return errors.New("item is not an instance of *Automations")
		}

		if err := compiledTemplate.Instance.Execute(w, automations); err != nil {
			return fmt.Errorf("unable to execute automations_automations template: %w", err)
		}

		return nil
	}

	return spi.EntityRenderer{
		StreamingRenderFunc: renderer,
		Styles:              compiledTemplate.Styles,
		Scripts:             compiledTemplate.Scripts,
	}, nil
}

// relativeTime says how long ago a time was, in its two most significant units: "5m ago", "2h 5m ago",
// "3d 4h ago". A time that is zero reads as "never".
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	return words(time.Since(t), "ago", "just now")
}

// untilTime says how long until a time, in the same way: "in 5m", "in 2h 5m", "in 3d 4h".
func untilTime(t time.Time) string {
	return words(time.Until(t), "", "now")
}

// words says how long a span is in its two most significant units, followed by suffix, or prefixed by "in" if there
// is no suffix. A span of less than a minute, or less than nothing, reads as zero.
func words(span time.Duration, suffix string, zero string) string {
	span = span.Truncate(time.Minute)

	var text string

	switch {
	case span <= 0:
		return zero
	case span < time.Hour:
		text = fmt.Sprintf("%dm", int(span.Minutes()))
	case span < 24*time.Hour:
		text = fmt.Sprintf("%dh %dm", int(span.Hours()), int(span.Minutes())%60)
	default:
		text = fmt.Sprintf("%dd %dh", int(span.Hours())/24, int(span.Hours())%24)
	}

	if suffix == "" {
		return "in " + text
	}

	return text + " " + suffix
}
