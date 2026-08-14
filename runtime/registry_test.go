package runtime_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// fakeAdapter is a minimal AgentAdapter whose descriptor is derived from the
// name it was registered under. The registry is deliberately name-agnostic, so
// tests register fake adapters as "alpha"/"beta" rather than any real agent
// name (codex/claude-code/kimi).
type fakeAdapter struct {
	name string
}

func (f fakeAdapter) Describe(ctx context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{ModelID: f.name}, nil
}

func (f fakeAdapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	return nil, errors.New("registry tests do not start sessions")
}

func fakeFactory(name string) runtime.AdapterFactory {
	return func(ctx context.Context, registeredAs string, cfg runtime.AdapterConfig) (runtime.AgentAdapter, error) {
		return fakeAdapter{name: registeredAs}, nil
	}
}

// Compile-time assertions that the fake types satisfy the canonical contracts.
var (
	_ runtime.AgentAdapter     = fakeAdapter{}
	_ runtime.ExecutionBackend = fakeBackend{}
	_ runtime.Session          = fakeSession{}
	_ runtime.ExecutionHandle  = fakeSession{}
)

// fakeSession implements the canonical Session/ExecutionHandle method set.
type fakeSession struct{}

func (fakeSession) Send(ctx context.Context, input runtime.Input) error { return nil }
func (fakeSession) Events() <-chan runtime.Event                        { return nil }
func (fakeSession) Abort(ctx context.Context) error                     { return nil }
func (fakeSession) Close(ctx context.Context) error                     { return nil }

// fakeBackend implements the canonical ExecutionBackend contract.
type fakeBackend struct{}

func (fakeBackend) Start(ctx context.Context, req runtime.StartRequest) (runtime.ExecutionHandle, error) {
	return fakeSession{}, nil
}

func TestRegistryRegisterAndListSorted(t *testing.T) {
	reg := runtime.NewRegistry()
	if err := reg.Register("beta", fakeFactory("beta")); err != nil {
		t.Fatalf("Register(beta): %v", err)
	}
	if err := reg.Register("alpha", fakeFactory("alpha")); err != nil {
		t.Fatalf("Register(alpha): %v", err)
	}

	// List must be sorted and stable, regardless of registration order.
	got := reg.List()
	want := []string{"alpha", "beta"}
	if !slices.Equal(got, want) {
		t.Fatalf("List() = %v, want %v (sorted)", got, want)
	}

	// A second call must return the same order (stability).
	if got2 := reg.List(); !slices.Equal(got2, want) {
		t.Fatalf("second List() = %v, want %v (stable)", got2, want)
	}
}

func TestRegistryListEmpty(t *testing.T) {
	reg := runtime.NewRegistry()
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("List() on empty registry = %v, want empty", got)
	}
}

func TestRegistryResolveCreatesAdapter(t *testing.T) {
	reg := runtime.NewRegistry()
	if err := reg.Register("alpha", fakeFactory("alpha")); err != nil {
		t.Fatalf("Register(alpha): %v", err)
	}

	adapter, err := reg.Resolve(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Resolve(alpha): %v", err)
	}
	desc, err := adapter.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ModelID != "alpha" {
		t.Fatalf("resolved adapter ModelID = %q, want %q", desc.ModelID, "alpha")
	}
}

func TestRegistryRejectsDuplicateRegistration(t *testing.T) {
	reg := runtime.NewRegistry()
	if err := reg.Register("alpha", fakeFactory("alpha")); err != nil {
		t.Fatalf("first Register(alpha): %v", err)
	}
	if err := reg.Register("alpha", fakeFactory("other")); err == nil {
		t.Fatal("second Register(alpha) succeeded, want duplicate-registration error")
	}
	// The original factory must remain in place.
	adapter, err := reg.Resolve(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Resolve(alpha) after rejected duplicate: %v", err)
	}
	desc, _ := adapter.Describe(context.Background())
	if desc.ModelID != "alpha" {
		t.Fatalf("after rejected duplicate, ModelID = %q, want %q", desc.ModelID, "alpha")
	}
}

func TestRegistryRejectsInvalidRegistration(t *testing.T) {
	reg := runtime.NewRegistry()
	if err := reg.Register("", fakeFactory("x")); err == nil {
		t.Fatal("Register with empty name succeeded, want error")
	}
	if err := reg.Register("alpha", nil); err == nil {
		t.Fatal("Register with nil factory succeeded, want error")
	}
}

func TestRegistryResolveUnknownModelListsAvailable(t *testing.T) {
	reg := runtime.NewRegistry()
	if err := reg.Register("alpha", fakeFactory("alpha")); err != nil {
		t.Fatalf("Register(alpha): %v", err)
	}
	if err := reg.Register("beta", fakeFactory("beta")); err != nil {
		t.Fatalf("Register(beta): %v", err)
	}

	_, err := reg.Resolve(context.Background(), "gamma")
	if err == nil {
		t.Fatal("Resolve(gamma) succeeded, want unknown-model error")
	}
	if !strings.Contains(err.Error(), "gamma") {
		t.Errorf("error %q does not name the requested model", err)
	}
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list available model %q", err, want)
		}
	}
	// The registry must not leak any real agent names it never registered.
	for _, banned := range []string{"codex", "claude-code", "kimi"} {
		if strings.Contains(err.Error(), banned) {
			t.Errorf("error %q mentions %q although it was never registered", err, banned)
		}
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	reg := runtime.NewRegistry()
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := strings.ToLower(string(rune('a' + i)))
			_ = reg.Register(name, fakeFactory(name))
		}(i)
	}
	wg.Wait()

	// Resolve and List concurrently with one more registration pass; run under
	// -race to prove the registry is thread-safe.
	var wg2 sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg2.Add(2)
		go func() {
			defer wg2.Done()
			_ = reg.List()
		}()
		go func() {
			defer wg2.Done()
			_, _ = reg.Resolve(context.Background(), "a")
		}()
	}
	wg2.Wait()
}
