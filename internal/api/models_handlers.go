package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/pricing"
)

// openAIModelEntry is the canonical OpenAI /v1/models row shape.
// Third-party SDKs (openai-python, openai-node, LangChain, the
// Vercel AI SDK, …) all parse this exact envelope; if we return
// our internal modelEntry / modelsResponse the SDK's schema
// validator rejects the response before any downstream call
// runs. Operator-observed 2026-05-16: OpenAI SDK clients
// surfaced this as a 500 on their side, masquerading as a
// daemon-side error.
type openAIModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // always "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// openAIModelsResponse is the wire shape openai-python checks
// for: {object:"list", data:[{...}]}. The data is sorted to
// match the canonical /api/v1/models flat list so clients
// paginate predictably even though we don't yet honour a real
// cursor.
type openAIModelsResponse struct {
	Object string             `json:"object"` // always "list"
	Data   []openAIModelEntry `json:"data"`
}

// modelEntry is the per-model record returned by GET /api/v1/models.
// It extends chat.ModelInfo with the pricing crosswalk so operators
// can see, in one place, which models a provider serves AND whether
// they have a cost entry that the executor can use to compute spend.
type modelEntry struct {
	// embedded discovery fields
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Source   string `json:"source"`
	OwnedBy  string `json:"owned_by,omitempty"`
	Created  int64  `json:"created,omitempty"`
	// pricing crosswalk; Priced is false when the model isn't in
	// pricing.yaml — those models will accrue cost at the configured
	// `default` rate (or zero) until an entry is added.
	Priced              bool    `json:"priced"`
	InputUSDPerMillion  float64 `json:"input_usd_per_m,omitempty"`
	OutputUSDPerMillion float64 `json:"output_usd_per_m,omitempty"`
	ReasoningMultiplier float64 `json:"reasoning_multiplier,omitempty"`
}

// modelsResponse is the top-level shape for GET /api/v1/models.
// Models is the flat aggregated list (all sub-providers concatenated,
// sorted by provider then ID) so a CLI can render a single table
// without re-stitching. Errors mirrors chat.ListModelsResult.Errors —
// keys are sub-provider names whose ListModels failed; the rest of
// the response is unaffected.
type modelsResponse struct {
	Models      []modelEntry      `json:"models"`
	Errors      map[string]string `json:"errors,omitempty"`
	PricingPath string            `json:"pricing_path,omitempty"`
}

// ListModels handles GET /api/v1/models. It walks every chat
// sub-provider that implements ModelLister, aggregates their lists
// (via the router), and crosswalks against pricing.yaml when
// configured. Returns 503 CHAT_NOT_CONFIGURED when no chat provider
// is wired, mirroring the chat-completions proxy's behaviour.
func (s *Server) ListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.chatProvider == nil {
		respondError(w, http.StatusServiceUnavailable, "CHAT_NOT_CONFIGURED",
			"chat provider not configured; model discovery requires chat.provider to be enabled")
		return
	}

	// The aggregating ListModels lives on *chat.Router. When the
	// dispatcher is configured with a single sub-provider directly
	// (provider != "router"), the chatProvider may itself implement
	// ModelLister — fall through to that path.
	// The aggregating ListModels lives on *chat.Router. The container
	// often wraps it with QueuedProvider for concurrency bounding, so
	// we ask both shapes in order:
	//   1. *chat.Router directly (no wrapping)
	//   2. *chat.QueuedProvider via its ListModelsAggregated, which
	//      unwraps to the inner *Router when present
	//   3. Any chat.ModelLister (single-provider deployments — only
	//      one sub-provider, no router involved)
	var result chat.ListModelsResult
	if r1, ok := s.chatProvider.(*chat.Router); ok {
		result = r1.ListModels(r.Context())
	} else if q, ok := s.chatProvider.(chat.ModelAggregator); ok {
		if agg, ok := q.ListModelsAggregated(r.Context()); ok {
			result = agg
		} else if models, err := q.ListModels(r.Context()); err == nil {
			for i := range models {
				if models[i].Provider == "" {
					models[i].Provider = "chat"
				}
			}
			result = chat.ListModelsResult{Providers: map[string][]chat.ModelInfo{"chat": models}}
		} else {
			result = chat.ListModelsResult{
				Providers: map[string][]chat.ModelInfo{},
				Errors:    map[string]string{"chat": err.Error()},
			}
		}
	} else if lister, ok := s.chatProvider.(chat.ModelLister); ok {
		models, err := lister.ListModels(r.Context())
		if err != nil {
			result = chat.ListModelsResult{
				Providers: map[string][]chat.ModelInfo{},
				Errors:    map[string]string{"chat": err.Error()},
			}
		} else {
			for i := range models {
				if models[i].Provider == "" {
					models[i].Provider = "chat"
				}
			}
			result = chat.ListModelsResult{Providers: map[string][]chat.ModelInfo{"chat": models}}
		}
	} else {
		respondError(w, http.StatusNotImplemented, "DISCOVERY_UNSUPPORTED",
			"the configured chat provider does not implement model discovery")
		return
	}

	// Pricing crosswalk. A missing or unparseable pricing.yaml is a
	// soft failure — log and proceed with Priced=false everywhere so
	// the discovery surface still returns useful data.
	var table *pricing.Table
	if s.pricingPath != "" {
		t, err := pricing.Load(s.pricingPath)
		if err != nil {
			s.logger.Warn().Err(err).Str("path", s.pricingPath).Msg("models endpoint: pricing.yaml load failed")
		} else {
			table = t
		}
	}

	flat := make([]modelEntry, 0, 32)
	for provider, models := range result.Providers {
		for _, m := range models {
			entry := modelEntry{
				ID:       m.ID,
				Provider: provider,
				Source:   m.Source,
				OwnedBy:  m.OwnedBy,
				Created:  m.Created,
			}
			if table != nil {
				if p, known := table.Lookup(m.ID); known {
					entry.Priced = true
					entry.InputUSDPerMillion = p.InputUSDPerMillion
					entry.OutputUSDPerMillion = p.OutputUSDPerMillion
					entry.ReasoningMultiplier = p.ReasoningMultiplier
				}
			}
			flat = append(flat, entry)
		}
	}

	// Stable sort: provider first (so the operator-facing CLI groups
	// rows naturally), model ID second.
	sort.SliceStable(flat, func(i, j int) bool {
		if flat[i].Provider != flat[j].Provider {
			return flat[i].Provider < flat[j].Provider
		}
		return flat[i].ID < flat[j].ID
	})

	w.Header().Set("Content-Type", "application/json")

	// OpenAI-canonical response shape for the /v1/models alias path
	// (registered alongside /api/v1/models in routes.go). The
	// canonical path keeps the vornik-internal {models, errors,
	// pricing_path} envelope that the CLI + dashboard consume; the
	// /v1/ path emits {object:"list", data:[...]} that openai-python
	// + the rest of the OpenAI client ecosystem expects.
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		nowUnix := time.Now().Unix()
		data := make([]openAIModelEntry, 0, len(flat))
		for _, m := range flat {
			created := m.Created
			if created == 0 {
				created = nowUnix
			}
			data = append(data, openAIModelEntry{
				ID:      m.ID,
				Object:  "model",
				Created: created,
				OwnedBy: m.OwnedBy,
			})
		}
		if err := json.NewEncoder(w).Encode(openAIModelsResponse{Object: "list", Data: data}); err != nil {
			s.logger.Warn().Err(err).Msg("models endpoint (/v1): response encode failed")
		}
		return
	}

	resp := modelsResponse{
		Models:      flat,
		Errors:      result.Errors,
		PricingPath: s.pricingPath,
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn().Err(err).Msg("models endpoint: response encode failed")
	}
}
