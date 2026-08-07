package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// AdapterFactory creates a new AgentAdapter instance for the given
// registration name. A single factory may serve multiple registrations.
type AdapterFactory func(ctx context.Context, name string) (AgentAdapter, error)

// Registry is a thread-safe registry of adapter factories keyed by string
// name. It is deliberately name-agnostic: the first-generation agent names are
// nothing special to it, and any adapter can be registered under any name.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]AdapterFactory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]AdapterFactory)}
}

// Register binds a factory to a name. Registering the same name twice is an
// error; the original registration is left untouched.
func (r *Registry) Register(name string, factory AdapterFactory) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("runtime: register: empty adapter name")
	}
	if factory == nil {
		return fmt.Errorf("runtime: register %q: nil factory", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[name]; ok {
		return fmt.Errorf("runtime: register %q: adapter already registered", name)
	}
	r.factories[name] = factory
	return nil
}

// List returns the registered adapter names in sorted order. The result is
// deterministic and safe for concurrent callers.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve creates an adapter for the requested name. Unknown names return an
// error that lists the available adapters so callers can self-correct.
func (r *Registry) Resolve(ctx context.Context, name string) (AgentAdapter, error) {
	r.mu.RLock()
	factory, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		available := r.List()
		if len(available) == 0 {
			return nil, fmt.Errorf("runtime: adapter %q: no adapters registered", name)
		}
		return nil, fmt.Errorf("runtime: adapter %q: unknown (available: %s)", name, strings.Join(available, ", "))
	}
	return factory(ctx, name)
}

// defaultRegistry is the process-wide registry that adapter packages populate
// from their init() functions (see cmd/gateway/plugin_agent_*.go).
var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide registry used by adapter packages.
// The gateway entrypoint owns this registry; adapter packages register into it
// via Register from their init() functions.
func DefaultRegistry() *Registry {
	return defaultRegistry
}

// Register registers an adapter factory on the default registry. Adapter
// packages call this from init().
func Register(name string, factory AdapterFactory) error {
	return defaultRegistry.Register(name, factory)
}

// Resolve creates an adapter from the default registry.
func Resolve(ctx context.Context, name string) (AgentAdapter, error) {
	return defaultRegistry.Resolve(ctx, name)
}

// List returns the default registry's adapter names in sorted order.
func List() []string {
	return defaultRegistry.List()
}
