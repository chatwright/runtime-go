package actor

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrScriptExhausted means a ScriptedProvider's Propose was called more
// times than its script has entries.
var ErrScriptExhausted = errors.New("actor: scripted provider's script is exhausted")

// ScriptedProvider is a deterministic Provider driven by a fixed, ordered
// script of Proposals — no model, no network, no cassette needed. It exists
// for tests and the CI replay gate: a campaign run against a
// ScriptedProvider is exactly reproducible on its own, at zero cost, every
// time.
//
// ScriptedProvider ignores the Prompt it is given — it is a fixed sequence,
// not a policy — so a caller that needs to react to what is actually
// observed (e.g. an opaque observe.AvailableAction.ID only known once the
// conversation is under way) should use ProviderFunc instead.
type ScriptedProvider struct {
	usage Usage

	mu     sync.Mutex
	script []Proposal
	next   int
}

// NewScriptedProvider returns a ScriptedProvider that proposes each of
// script's entries in order, one per Propose call, always reporting usage
// verbatim.
func NewScriptedProvider(usage Usage, script ...Proposal) *ScriptedProvider {
	return &ScriptedProvider{usage: usage, script: append([]Proposal(nil), script...)}
}

// Propose returns the next scripted Proposal, or ErrScriptExhausted once the
// script runs out.
func (p *ScriptedProvider) Propose(_ context.Context, prompt Prompt) (Proposal, Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next >= len(p.script) {
		return Proposal{}, Usage{}, fmt.Errorf("%w: %d proposals scripted, asked for one more at task %q",
			ErrScriptExhausted, len(p.script), prompt.TaskID)
	}
	next := p.script[p.next]
	p.next++
	return next, p.usage, nil
}
