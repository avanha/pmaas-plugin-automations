package http

import (
	"embed"
	"errors"
	"fmt"
	"github.com/avanha/pmaas-plugin-automations/data"
	"github.com/avanha/pmaas-plugin-automations/internal/common"
	"github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/format"
	"github.com/avanha/pmaas-spi/logging"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"reflect"
)

//go:embed content/static content/templates
var contentFS embed.FS

var automationsTemplate = spi.TemplateInfo{
	Name: "automations_automations",
	FuncMap: template.FuncMap{
		"RelativeTime": format.Ago,
		"UntilTime":    format.Until,
	},
	Paths:   []string{"templates/automations_automations.htmlt"},
	Styles:  []string{"css/automations_automations.css"},
	Scripts: []string{"js/automations_automations.js"},
}

// Handler owns the plugin's HTTP surface: the page that shows the automations, and the route that runs one by hand.
type Handler struct {
	log       *slog.Logger
	container spi.IPMAASContainer
	store     common.AutomationStore
}

func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) Init(container spi.IPMAASContainer, store common.AutomationStore) {
	h.container = container
	h.log = logging.For(container, "http")
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
	container.AddJsonRoute(
		"pause",
		func() any { return &pauseRequest{} },
		h.handleHttpPauseRequest)
	container.RegisterEntityRenderer(
		reflect.TypeOf((*data.Automations)(nil)).Elem(),
		h.automationsRendererFactory)
}

func (h *Handler) handleHttpListRequest(w http.ResponseWriter, r *http.Request) {
	automations, err := h.store.GetAutomations()

	if err != nil {
		h.log.Error("unable to retrieve automations", "error", err)
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
		h.log.Error("unable to run", "id", body.ID, "error", err)

		return nil, errors.New("unable to run it")
	}

	return runResponse{Succeeded: result.Succeeded, Message: result.Message, Error: result.Error}, nil
}

type pauseRequest struct {
	ID string

	// Paused is whether to pause it (true) or resume it.
	Paused bool
}

type pauseResponse struct {
	Paused bool
}

func (h *Handler) handleHttpPauseRequest(_ http.ResponseWriter, r *http.Request, request any) (any, error) {
	if r.Method != http.MethodPost {
		return nil, errors.New("method not allowed")
	}

	body, ok := request.(*pauseRequest)

	if !ok || body.ID == "" {
		return nil, errors.New("an ID is required")
	}

	if err := h.store.SetPaused(body.ID, body.Paused); err != nil {
		h.log.Error("unable to set paused", "paused", body.Paused, "id", body.ID, "error", err)

		return nil, errors.New("unable to pause or resume it")
	}

	return pauseResponse{Paused: body.Paused}, nil
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
		h.log.Error("unable to re-enable", "id", body.ID, "error", err)

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
