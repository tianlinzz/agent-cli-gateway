package openai

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

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
// modelCacheTTL is how long the derived catalog (availability probes + static
// descriptors) is reused before it is recomputed. Probing on every chat
// request would call Describe/LookPath per request (the U3 finding); a short
// TTL keeps discovery cheap while still reacting to a CLI being installed or
// removed within seconds.
const modelCacheTTL = 30 * time.Second

// probeTimeout bounds one model-catalog recompute (adapter Resolve/Describe)
// so a slow adapter cannot wedge model discovery or chat-request validation.
const probeTimeout = 5 * time.Second

// catalogSnapshot is one computed view of the public catalog: the descriptors
// advertised on /v1/models plus the set of adapter names considered available.
type catalogSnapshot struct {
	descs    []runtime.Descriptor
	adapters map[string]bool // adapter id -> available (enabled + probe + describable)
}

type modelCatalog struct {
	reg     *runtime.Registry
	enabled func(name string) bool
	models  map[string][]string
	// commands maps adapter id -> configured CLI command. A nil map disables
	// availability probing (tests and defaults advertise on Describe success
	// alone); a non-nil map probes each enabled adapter via exec.LookPath so
	// /v1/models never advertises a CLI that is not installed.
	commands map[string]string

	mu     sync.Mutex
	snap   catalogSnapshot
	snapAt time.Time
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

// has reports whether name is a known, enabled, and available model. It uses
// the cached catalog snapshot so the chat-request validation path does not
// resolve/describe/probe an adapter on every request.
func (c *modelCatalog) has(name string) bool {
	if c == nil || c.reg == nil {
		return false
	}
	route, ok := parseModelRoute(name, c.models)
	if !ok {
		return false
	}
	return c.snapshot().adapters[route.AdapterID]
}

// discover returns every available adapter's public descriptors, sorted by
// model id, backing GET /v1/models.
func (c *modelCatalog) discover(ctx context.Context) []runtime.Descriptor {
	if c == nil || c.reg == nil {
		return nil
	}
	return c.snapshot().descs
}

// snapshot returns the cached catalog, recomputing it once per modelCacheTTL.
// Concurrent callers may compute the same snapshot redundantly; that is
// idempotent (last writer wins) and bounded by the TTL.
func (c *modelCatalog) snapshot() catalogSnapshot {
	c.mu.Lock()
	if c.snap.adapters != nil && time.Since(c.snapAt) < modelCacheTTL {
		s := c.snap
		c.mu.Unlock()
		return s
	}
	c.mu.Unlock()

	s := c.compute()

	c.mu.Lock()
	c.snap = s
	c.snapAt = time.Now()
	c.mu.Unlock()
	return s
}

// compute derives the fresh catalog: every enabled adapter that is available
// (command probe passes when wired, Describe succeeds) is advertised, sorted by
// public model id. The Resolve/Describe step is bounded so a slow or wedged
// adapter can never stall /v1/models or the chat-request validation path.
func (c *modelCatalog) compute() catalogSnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	snap := catalogSnapshot{adapters: make(map[string]bool)}
	names := c.reg.List()
	sort.Strings(names)
	for _, name := range names {
		if c.enabled != nil && !c.enabled(name) {
			continue
		}
		if !c.commandAvailable(name) {
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
		snap.adapters[name] = true
		if models := c.models[name]; len(models) > 0 {
			for _, model := range models {
				d := desc
				d.ModelID = name + "/" + model
				snap.descs = append(snap.descs, d)
			}
		} else {
			snap.descs = append(snap.descs, desc)
		}
	}
	sort.Slice(snap.descs, func(i, j int) bool { return snap.descs[i].ModelID < snap.descs[j].ModelID })
	return snap
}

// commandAvailable reports whether an enabled adapter may be advertised.
// Probing only happens when the catalog was wired with command knowledge
// (commands != nil — the production gateway always wires it); without it
// (tests, un-wired defaults) an adapter is available on Describe success alone.
// When wired, an enabled adapter whose command is empty or does not resolve on
// PATH is hidden so /v1/models never advertises a CLI that is not installed.
func (c *modelCatalog) commandAvailable(name string) bool {
	if c.commands == nil {
		return true
	}
	cmd := strings.TrimSpace(c.commands[name])
	if cmd == "" {
		slog.Warn("openai: hiding enabled agent without a configured command", "adapter", name)
		return false
	}
	// The configured command may carry argv (see the splitCommand adapters);
	// probe only the executable's first token. Absolute paths and bare names
	// are both handled by LookPath.
	exe := strings.Fields(cmd)[0]
	if _, err := exec.LookPath(exe); err != nil {
		slog.Warn("openai: hiding agent whose CLI is not installed", "adapter", name, "command", exe, "error", err)
		return false
	}
	return true
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
