package arena

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
)

// LoadResult is one Loader.Load call's outcome — always recorded in the
// environment block (spec/ideas/actor-model-arena.md's right-sizing rule:
// "Recorded in the report's environment block so entries stay comparable")
// regardless of whether right-sizing actually happened, so a report reader
// can tell "loaded at a 4k context" apart from "server defaulted to
// whatever it felt like" at a glance.
type LoadResult struct {
	// Performed is true when eviction plus a right-sized load actually
	// ran. False means Run degrades to a JIT load, relying on a long
	// warm-up timeout instead (the spec's documented fallback) — never a
	// hard failure.
	Performed bool
	// Note is a short, human-readable explanation: what ran, or why it
	// didn't (tooling absent, a non-fatal error).
	Note string
}

// Loader evicts whatever else a server has loaded and pre-loads a
// ProviderSpec's model at its declared ContextLength, when the server's
// tooling supports it — see LMStudioLoader, OllamaLoader. Deliberately
// optional (spec/ideas/actor-model-arena.md: "both optional (degrade to
// JIT with a long warm-up timeout when tooling is absent)"): a Loader that
// cannot do this degrades to LoadResult{Performed: false, ...} rather than
// failing the run — the mandatory warm-up call (see Run) still measures
// cold-start either way, just against a JIT load path instead of a
// pre-sized one.
type Loader interface {
	Load(ctx context.Context, spec ProviderSpec) (LoadResult, error)
}

// NoopLoader performs no eviction/right-sizing at all — the default for
// KindOpenAICompat, and any ProviderKind a caller does not supply a Loader
// for.
type NoopLoader struct{}

// Load implements Loader: always degraded, never an error.
func (NoopLoader) Load(context.Context, ProviderSpec) (LoadResult, error) {
	return LoadResult{Performed: false, Note: "no loader configured for this provider kind — JIT load, no eviction"}, nil
}

// LMStudioLoader right-sizes and evicts via the `lms` CLI — LM Studio's own
// command-line tool (spec/ideas/actor-model-arena.md: "lms load
// --context-length"): `lms unload --all` (the evict-others hook) then
// `lms load <model> --context-length <n> --yes`. Both steps are
// best-effort: a missing `lms` binary, or either subcommand failing,
// degrades to LoadResult{Performed: false} rather than returning an error —
// the arena keeps running against whatever the server already has loaded.
type LMStudioLoader struct {
	// Exec runs name with args, returning combined output — defaults to
	// os/exec (exec.CommandContext(ctx, name, args...).CombinedOutput());
	// overridden in tests so this package's unit tests never shell out for
	// real.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
	// LookPath resolves name to an executable path — defaults to
	// exec.LookPath; overridden in tests alongside Exec.
	LookPath func(name string) (string, error)
}

func (l LMStudioLoader) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if l.Exec != nil {
		return l.Exec(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (l LMStudioLoader) lookPath(name string) (string, error) {
	if l.LookPath != nil {
		return l.LookPath(name)
	}
	return exec.LookPath(name)
}

// Load implements Loader.
func (l LMStudioLoader) Load(ctx context.Context, spec ProviderSpec) (LoadResult, error) {
	if _, err := l.lookPath("lms"); err != nil {
		return LoadResult{
			Performed: false,
			Note:      "lms CLI not found on PATH — degraded to JIT load (context not right-sized); relying on the warm-up timeout for cold-start",
		}, nil
	}

	if out, err := l.exec(ctx, "lms", "unload", "--all"); err != nil {
		return LoadResult{
			Performed: false,
			Note:      fmt.Sprintf("lms unload --all failed (%v): %s — degraded to JIT load", err, strings.TrimSpace(string(out))),
		}, nil
	}

	args := []string{"load", spec.Model, "--yes"}
	if spec.ContextLength > 0 {
		args = append(args, "--context-length", fmt.Sprintf("%d", spec.ContextLength))
	}
	out, err := l.exec(ctx, "lms", args...)
	if err != nil {
		return LoadResult{
			Performed: false,
			Note:      fmt.Sprintf("lms load failed (%v): %s — degraded to JIT load", err, strings.TrimSpace(string(out))),
		}, nil
	}

	note := fmt.Sprintf("evicted other models, loaded %s", spec.Model)
	if spec.ContextLength > 0 {
		note = fmt.Sprintf("%s at context-length=%d", note, spec.ContextLength)
	}
	return LoadResult{Performed: true, Note: note}, nil
}

// OllamaLoader right-sizes via Ollama's native API (spec/ideas/
// actor-model-arena.md: "a native-API pre-load with num_ctx options
// (Ollama)"): it POSTs an empty-prompt /api/generate request carrying
// options.num_ctx and a long keep_alive, which loads the model without
// generating anything. Ollama itself evicts whatever else it had resident
// (its default is a single resident model), so — unlike LM Studio, which
// can cohabit several loaded models — no separate evict step is needed. A
// transport error (server unreachable, an old Ollama build with no native
// API) degrades rather than failing the run.
type OllamaLoader struct {
	HTTPClient *http.Client
	// KeepAlive is sent as the pre-load request's "keep_alive" — how long
	// Ollama keeps the model resident after this call. Defaults to "10m".
	KeepAlive string
}

func (l OllamaLoader) httpClient() *http.Client {
	if l.HTTPClient != nil {
		return l.HTTPClient
	}
	return http.DefaultClient
}

// Load implements Loader.
func (l OllamaLoader) Load(ctx context.Context, spec ProviderSpec) (LoadResult, error) {
	keepAlive := l.KeepAlive
	if keepAlive == "" {
		keepAlive = "10m"
	}

	body := map[string]any{"model": spec.Model, "prompt": "", "keep_alive": keepAlive}
	if spec.ContextLength > 0 {
		body["options"] = map[string]any{"num_ctx": spec.ContextLength}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return LoadResult{}, fmt.Errorf("arena: OllamaLoader: encode pre-load request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaNativeBaseURL(spec.BaseURL)+"/api/generate", bytes.NewReader(payload))
	if err != nil {
		return LoadResult{}, fmt.Errorf("arena: OllamaLoader: build pre-load request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.httpClient().Do(req)
	if err != nil {
		return LoadResult{Performed: false, Note: fmt.Sprintf("Ollama native pre-load unreachable (%v) — degraded to JIT load", err)}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return LoadResult{Performed: false, Note: fmt.Sprintf("Ollama native pre-load returned %s — degraded to JIT load", resp.Status)}, nil
	}

	note := fmt.Sprintf("pre-loaded %s (keep_alive=%s)", spec.Model, keepAlive)
	if spec.ContextLength > 0 {
		note = fmt.Sprintf("%s, num_ctx=%d", note, spec.ContextLength)
	}
	return LoadResult{Performed: true, Note: note}, nil
}

// ollamaNativeBaseURL derives Ollama's native API base (which /api/generate
// hangs off) from an OpenAI-compatible BaseURL (typically ending in "/v1",
// e.g. "http://localhost:11434/v1") by trimming that suffix. A BaseURL not
// ending in "/v1" is returned unchanged — best effort: the caller may
// already have pointed BaseURL at the native root.
func ollamaNativeBaseURL(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// DefaultLoaders returns the built-in Loader for each ProviderKind this
// package has a specific mechanism for: LMStudioLoader{} for KindLMStudio,
// OllamaLoader{} for KindOllama. RunOptions.Loaders starts from this map
// when left nil; a caller overrides one entry (e.g. to inject a fake
// Loader in a test) by setting RunOptions.Loaders itself. A ProviderKind
// with no entry gets NoopLoader.
func DefaultLoaders() map[ProviderKind]Loader {
	return map[ProviderKind]Loader{
		KindLMStudio: LMStudioLoader{},
		KindOllama:   OllamaLoader{},
	}
}
