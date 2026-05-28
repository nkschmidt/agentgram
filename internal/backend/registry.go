package backend

import "fmt"

// Factory creates a fresh Backend instance for the given userID.
// The factory needs userID to seal per-user settings (e.g. the working
// directory from settings) into the created Backend's closure.
type Factory func(userID int64) Backend

// Registry — registry of available backend adapters by name.
// session.Manager uses it to create a process by the name from the UI
// ("claude", "opencode", ...), without knowing about concrete packages.
type Registry struct {
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// Register adds a factory under the name. Re-registration overwrites —
// this is explicit behavior for tests and overrides.
func (r *Registry) Register(name string, f Factory) {
	r.factories[name] = f
}

// New creates a backend instance by name for the given userID.
func (r *Registry) New(name string, userID int64) (Backend, error) {
	f, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", name)
	}
	return f(userID), nil
}
