package agent

import (
	"sync"

	"github.com/rcliao/shell/internal/process"
)

// AgentFactory creates an Agent from a manifest.
type AgentFactory func(m *Manifest) (process.Agent, error)

// Pool manages multiple agents and routes chats to the right one.
type Pool struct {
	agents       map[string]process.Agent // name → Agent instance
	manifests    map[string]*Manifest     // name → Manifest
	routing      map[int64]string         // chatID → agent name
	defaultAgent string                   // fallback agent name
	mu           sync.RWMutex
}

// NewPool creates a pool from manifests using the given factory.
func NewPool(manifests []*Manifest, factory AgentFactory) (*Pool, error) {
	p := &Pool{
		agents:    make(map[string]process.Agent),
		manifests: make(map[string]*Manifest),
		routing:   make(map[int64]string),
	}

	for _, m := range manifests {
		agent, err := factory(m)
		if err != nil {
			return nil, err
		}
		p.agents[m.Name] = agent
		p.manifests[m.Name] = m

		// Register explicit chat ID bindings.
		for _, chatID := range m.Routing.ChatIDs {
			p.routing[chatID] = m.Name
		}
		if m.Routing.Default {
			p.defaultAgent = m.Name
		}
	}

	// If no default is set, use the first manifest.
	if p.defaultAgent == "" && len(manifests) > 0 {
		p.defaultAgent = manifests[0].Name
	}

	return p, nil
}

// Resolve returns the Agent for a given chatID.
// Falls back to the default agent if no explicit routing exists.
func (p *Pool) Resolve(chatID int64) process.Agent {
	p.mu.RLock()
	name, ok := p.routing[chatID]
	p.mu.RUnlock()

	if !ok {
		name = p.defaultAgent
	}

	return p.agents[name]
}

// ResolveManifest returns the Manifest for a given chatID.
func (p *Pool) ResolveManifest(chatID int64) *Manifest {
	p.mu.RLock()
	name, ok := p.routing[chatID]
	p.mu.RUnlock()

	if !ok {
		name = p.defaultAgent
	}

	return p.manifests[name]
}

// Route binds a chatID to a named agent.
func (p *Pool) Route(chatID int64, agentName string) bool {
	if _, ok := p.manifests[agentName]; !ok {
		return false
	}
	p.mu.Lock()
	p.routing[chatID] = agentName
	p.mu.Unlock()
	return true
}

// AgentNames returns all registered agent names.
func (p *Pool) AgentNames() []string {
	names := make([]string, 0, len(p.manifests))
	for name := range p.manifests {
		names = append(names, name)
	}
	return names
}

// Get returns the Agent for a named agent.
func (p *Pool) Get(name string) (process.Agent, bool) {
	a, ok := p.agents[name]
	return a, ok
}

// Manifest returns the Manifest for a named agent.
func (p *Pool) Manifest(name string) (*Manifest, bool) {
	m, ok := p.manifests[name]
	return m, ok
}

// CurrentAgent returns the agent name for a chatID.
func (p *Pool) CurrentAgent(chatID int64) string {
	p.mu.RLock()
	name, ok := p.routing[chatID]
	p.mu.RUnlock()
	if !ok {
		return p.defaultAgent
	}
	return name
}
