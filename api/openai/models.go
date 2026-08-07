package openai

import (
	"context"
	"net/http"
	"sort"

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
}

// has reports whether name is a known and enabled model. This is the cheap
// chat-request validation; discovery (Describe) is the heavier /v1/models
// path.
func (c *modelCatalog) has(name string) bool {
	if c == nil || c.reg == nil {
		return false
	}
	found := false
	for _, n := range c.reg.List() {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	return c.enabled == nil || c.enabled(name)
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
		out = append(out, desc)
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
