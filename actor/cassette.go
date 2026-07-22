package actor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Mode selects a CassetteProvider's record/replay behaviour. See
// NewCassetteProvider. It is a string type, not an int enum, so it marshals
// to human-readable JSON and prints readably in diagnostics (see AGENTS.md's
// "JSON artefacts carry human-readable string constants" convention) rather
// than a bare, meaningless integer.
type Mode string

// Cassette modes. See Mode.
const (
	// ModeLive calls the wrapped Provider directly and performs no cassette
	// I/O at all — exploratory only, never used in CI.
	ModeLive Mode = "live"
	// ModeRecord calls the wrapped (live) Provider and appends every
	// Propose call's prompt/outcome to the cassette; call Cassette then
	// Cassette.Save to persist it afterwards.
	ModeRecord Mode = "record"
	// ModeReplay never calls the wrapped Provider: every Propose call is
	// served from the cassette, and a cache miss is an error — the CI
	// default, at zero token cost.
	ModeReplay Mode = "replay"
)

// String renders m for diagnostics and error messages.
func (m Mode) String() string { return string(m) }

// ErrCassetteCacheMiss means a ModeReplay CassetteProvider was asked to
// propose for a prompt its cassette has no recorded entry for.
var ErrCassetteCacheMiss = errors.New("actor: replay cache miss")

// Cassette is a JSON-serialisable, ordered record of Provider interactions,
// keyed by a deterministic hash of the provider configuration plus the
// canonical prompt content — see NewCassetteProvider. It is meant to be
// checked into the repository under testdata/cassettes/ as human-readable,
// reviewable JSON; it never carries provider auth (that lives outside the
// prompt entirely).
type Cassette struct {
	// ProviderConfig is a free-form, caller-supplied description of the
	// wrapped provider's configuration (model, system prompt, temperature,
	// ...), folded into every entry's key so replaying against a
	// differently configured provider is a cache miss, not a silent
	// mismatch.
	ProviderConfig string `json:"providerConfig"`

	Entries []CassetteEntry `json:"entries"`
}

// CassetteEntry is one recorded Propose call.
type CassetteEntry struct {
	// Key is the deterministic hash of ProviderConfig plus the canonical
	// prompt this entry was recorded for; see promptKey.
	Key string `json:"key"`
	// PromptSummary is a short, human-readable description of the prompt —
	// for PR review and for ErrCassetteCacheMiss's error message. It is not
	// itself used to look the entry up (Key is).
	PromptSummary string   `json:"promptSummary"`
	Proposal      Proposal `json:"proposal"`
	Usage         Usage    `json:"usage"`
}

// NewCassette returns an empty Cassette for providerConfig, ready to record
// into.
func NewCassette(providerConfig string) *Cassette {
	return &Cassette{ProviderConfig: providerConfig}
}

// LoadCassette reads a Cassette from a JSON file at path.
func LoadCassette(path string) (*Cassette, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a caller-supplied testdata fixture, not user input
	if err != nil {
		return nil, fmt.Errorf("actor: load cassette %s: %w", path, err)
	}
	var c Cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("actor: parse cassette %s: %w", path, err)
	}
	return &c, nil
}

// Save writes c to path as indented, human-readable JSON, creating path's
// parent directory if needed.
func (c *Cassette) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("actor: save cassette %s: %w", path, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("actor: encode cassette: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // cassette files are not sensitive
		return fmt.Errorf("actor: save cassette %s: %w", path, err)
	}
	return nil
}

// CassetteProvider decorates any Provider with record/replay determinism
// (see Mode) — any Provider is recordable simply by wrapping it.
type CassetteProvider struct {
	mode     Mode
	wrapped  Provider
	cassette *Cassette

	mu    sync.Mutex
	index map[string]int // Key -> index into cassette.Entries, latest write wins
}

// NewCassetteProvider wraps wrapped with cassette in mode:
//
//   - ModeLive: wrapped must be non-nil; Propose calls it directly, no
//     cassette I/O.
//   - ModeRecord: wrapped must be non-nil; every Propose call invokes it and
//     appends the outcome to cassette. Call Cassette then Cassette.Save
//     afterwards to persist it.
//   - ModeReplay: wrapped may be nil — it is never called. A cache miss
//     returns an error wrapping ErrCassetteCacheMiss, carrying the missing
//     prompt's summary, never a live call.
//
// cassette must not be nil; use NewCassette for a fresh one or LoadCassette
// to replay a checked-in one.
func NewCassetteProvider(mode Mode, wrapped Provider, cassette *Cassette) (*CassetteProvider, error) {
	if cassette == nil {
		return nil, errors.New("actor: NewCassetteProvider: cassette is nil")
	}
	if mode != ModeReplay && wrapped == nil {
		return nil, fmt.Errorf("actor: NewCassetteProvider: wrapped provider is nil (required for mode %s)", mode)
	}
	index := make(map[string]int, len(cassette.Entries))
	for i, e := range cassette.Entries {
		index[e.Key] = i
	}
	return &CassetteProvider{mode: mode, wrapped: wrapped, cassette: cassette, index: index}, nil
}

// Cassette returns the provider's underlying Cassette — after a ModeRecord
// session, pass it to Cassette.Save to persist what was recorded.
func (p *CassetteProvider) Cassette() *Cassette { return p.cassette }

// Propose implements Provider per p's configured Mode.
func (p *CassetteProvider) Propose(ctx context.Context, prompt Prompt) (Proposal, Usage, error) {
	key, err := promptKey(p.cassette.ProviderConfig, prompt)
	if err != nil {
		return Proposal{}, Usage{}, fmt.Errorf("actor: hash prompt: %w", err)
	}

	switch p.mode {
	case ModeLive:
		return p.wrapped.Propose(ctx, prompt)

	case ModeReplay:
		p.mu.Lock()
		idx, ok := p.index[key]
		p.mu.Unlock()
		if !ok {
			return Proposal{}, Usage{}, fmt.Errorf("%w: %s", ErrCassetteCacheMiss, summarizePrompt(prompt))
		}
		entry := p.cassette.Entries[idx]
		return entry.Proposal, entry.Usage, nil

	case ModeRecord:
		proposal, usage, err := p.wrapped.Propose(ctx, prompt)
		if err != nil {
			return proposal, usage, err
		}
		p.mu.Lock()
		p.cassette.Entries = append(p.cassette.Entries, CassetteEntry{
			Key: key, PromptSummary: summarizePrompt(prompt), Proposal: proposal, Usage: usage,
		})
		p.index[key] = len(p.cassette.Entries) - 1
		p.mu.Unlock()
		return proposal, usage, nil

	default:
		return Proposal{}, Usage{}, fmt.Errorf("actor: unknown cassette mode %v", p.mode)
	}
}

// promptKey deterministically hashes providerConfig and prompt's canonical
// (encoding/json) serialisation together — same provider config, same
// prompt content, always the same key; a stable input always JSON-encodes
// identically since Prompt and everything it embeds is built from structs
// and slices only, never a map.
func promptKey(providerConfig string, prompt Prompt) (string, error) {
	canonical, err := json.Marshal(prompt)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(providerConfig+"\x00"), canonical...))
	return hex.EncodeToString(sum[:]), nil
}

// summarizePrompt renders a short, human-readable description of prompt for
// a cache-miss error message and a cassette entry's PromptSummary.
func summarizePrompt(prompt Prompt) string {
	return fmt.Sprintf("goal=%s task=%s observation#%d messages=%d history=%d",
		prompt.GoalID, prompt.TaskID, prompt.Observation.Sequence, len(prompt.Observation.Messages), len(prompt.History))
}
