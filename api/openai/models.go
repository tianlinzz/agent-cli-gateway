package openai

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// modelObject is one entry of the /v1/models response data array.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// modelList is the /v1/models response envelope.
type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// modelCatalog derives the public model list from the runtime registry. It is
// deliberately name-agnostic: every registered adapter that is both enabled by
// configuration and discoverable (Describe succeeds) is exposed as a model,
// sorted by model id for a stable response.
type modelCatalog struct {
	reg     *runtime.Registry
	enabled func(name string) bool
	models  map[string][]string
}

type modelRoute struct{ PublicID, AdapterID, ProviderModel string }

func parseModelRoute(id string, models map[string][]string) (modelRoute, bool) {
	parts := strings.SplitN(strings.TrimSpace(id), "/", 2)
	if len(parts) == 1 {
		return modelRoute{PublicID: id, AdapterID: id}, id != ""
	}
	if parts[0] == "" || parts[1] == "" {
		return modelRoute{}, false
	}
	for _, m := range models[parts[0]] {
		if m == parts[1] {
			return modelRoute{PublicID: id, AdapterID: parts[0], ProviderModel: parts[1]}, true
		}
	}
	return modelRoute{}, false
}

func (c *modelCatalog) route(id string) (modelRoute, bool) {
	r, ok := parseModelRoute(id, c.models)
	if !ok || !c.has(id) {
		return modelRoute{}, false
	}
	return r, true
}

// has reports whether name is a known and enabled model. This is the cheap
// chat-request validation; discovery (Describe) is the heavier /v1/models
// path.
func (c *modelCatalog) has(name string) bool {
	if c == nil || c.reg == nil {
		return false
	}
	route, ok := parseModelRoute(name, c.models)
	if !ok {
		return false
	}
	found := false
	for _, n := range c.reg.List() {
		if n == route.AdapterID {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if c.enabled != nil && !c.enabled(route.AdapterID) {
		return false
	}
	adapter, err := c.reg.Resolve(context.Background(), route.AdapterID)
	if err != nil {
		return false
	}
	_, err = adapter.Describe(context.Background())
	return err == nil
}

// discover resolves every enabled adapter and returns the descriptors whose
// discovery succeeded, sorted by ModelID. Adapters that fail to resolve or
// describe (e.g. a CLI binary not installed) are skipped: the public catalog
// only advertises models that actually work.
func (c *modelCatalog) discover(ctx context.Context) []runtime.Descriptor {
	var out []runtime.Descriptor
	if c == nil || c.reg == nil {
		return out
	}
	for _, name := range c.reg.List() {
		if c.enabled != nil && !c.enabled(name) {
			continue
		}
		adapter, err := c.reg.Resolve(ctx, name)
		if err != nil {
			continue
		}
		desc, err := adapter.Describe(ctx)
		if err != nil {
			continue
		}
		if configured := c.models[name]; len(configured) > 0 {
			for _, model := range configured {
				d := desc
				d.ModelID = name + "/" + model
				out = append(out, d)
			}
		} else {
			out = append(out, desc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	return out
}

// handleModels serves GET /v1/models with the stable, sorted, dynamic catalog.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	descs := h.catalog.discover(r.Context())
	data := make([]modelObject, 0, len(descs))
	for _, d := range descs {
		data = append(data, modelObject{
			ID:      d.ModelID,
			Object:  "model",
			Created: h.now().Unix(),
			OwnedBy: "agent-cli-gateway",
		})
	}
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: data})
}
