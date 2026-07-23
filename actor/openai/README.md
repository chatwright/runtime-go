# actor/openai

An `actor.Provider` for any OpenAI-compatible `/chat/completions` server:
[Ollama](https://ollama.com/), [LM Studio](https://lmstudio.ai/), OpenRouter,
vLLM, or OpenAI itself. See
[`../anthropic/README.md`](../anthropic/README.md) for the sibling provider
this package deliberately mirrors, and
[`../provider.go`](../provider.go) for the `actor.Provider` interface both
implement.

This package exists for the local-development story: point a campaign at a
model already running on your laptop — no API key, no cost, no network call
beyond localhost. See
[`spec/ideas/openai-compatible-provider.md`](https://github.com/chatwright/chatwright/blob/main/spec/ideas/openai-compatible-provider.md).

## Quick start

```go
import "chatwright.dev/runtime/actor/openai"

// Ollama
provider, err := openai.New(openai.Config{
    BaseURL: "http://localhost:11434/v1",
    Model:   "qwen3.6:latest",
})

// LM Studio
provider, err := openai.New(openai.Config{
    BaseURL: "http://localhost:1234/v1",
    Model:   "your-loaded-model-id",
})
```

`Provider` satisfies `actor.Provider`, so it plugs directly into
`actor.Loop`, `run.NewAIGoalPart`, or, for CI, into an
`actor.CassetteProvider` — see Cassette workflow below.

## Config

| Field | Required | Notes |
|---|---|---|
| `BaseURL` | Yes | e.g. `http://localhost:11434/v1` (Ollama), `http://localhost:1234/v1` (LM Studio). No package default — unlike a single hosted vendor, every OpenAI-compatible server lives at its own address. `New` returns `ErrMissingBaseURL` if empty. |
| `Model` | Yes | e.g. `qwen3.6:latest`. No package default either — each server's model catalogue is whatever the developer pulled/loaded locally. `New` returns `ErrMissingModel` if empty. |
| `APIKey` | No | Sent as `Authorization: Bearer <APIKey>` only when non-empty. Local servers (Ollama, LM Studio) do not need one. |
| `MaxTokens` | No | Defaults to `openai.DefaultMaxTokens` (2048 — see its doc comment for the arena evidence behind the value). |
| `HTTPClient` | No | Defaults to `http.DefaultClient`. Tests point this at a fake `http.RoundTripper`, or drive a real `httptest.Server` and set `BaseURL` to it. |
| `Now` | No | Defaults to `time.Now`; only used to measure `Usage.Latency`. |

## Structured output and graceful degradation

`Propose` first asks for `response_format: {"type":"json_schema", ...}`
with `strict:true` — OpenAI's structured-output contract, which newer
Ollama/LM Studio builds also implement, enforcing the JSON shape
server-side. The schema is the **same proposal contract**
`actor/anthropic` enforces: `kind` (`send-text` | `click` | `task-done` |
`give-up`), `text`, `action_id`, `rationale`, all four always present (an
empty string for whichever field the chosen `kind` does not need) — a
campaign's `actor.Prompt` → `actor.Proposal` contract does not vary by
provider.

Some OpenAI-compatible servers reject an unrecognised `response_format`
with an HTTP error instead of ignoring it. When that happens — any failure
status except `401`/`403` (authentication) or `429` (rate limit), which no
change of `response_format` can fix — `Propose` retries the **same prompt
exactly once** with `response_format: {"type":"json_object"}` plus the
response schema restated in the system prompt (there is no server-side
enforcement in this mode). The reply is still parsed strictly, through the
same one-JSON-repair-attempt path `response.go` uses either way. If the
fallback attempt also fails, `Propose` returns a typed
`*openai.FallbackFailedError` naming both underlying failures — never a
third attempt, never a fabricated `Proposal`.

Call `Provider.LastResponseFormatMode()` after `Propose` to see which mode
actually served the last call (`openai.ModeJSONSchema` or
`openai.ModeJSONObjectFallback`) — a test/diagnostic hook, never required
for correct use.

## Reasoning models and `reasoning_content`

Some reasoning models served through an OpenAI-compatible endpoint route
their entire reply — including the proposal JSON the response contract
asks for — into `message.reasoning_content` instead of `message.content`,
leaving `content` empty while the server still bills output tokens for it.
(`qwen/qwen3.6-27b` via LM Studio did this on 4/4 calls in the first
actor-model arena run — see
[`chatwright/runtime-go#3`](https://github.com/chatwright/runtime-go/issues/3).)

`Propose` reads the first non-empty field in this order — the first hit
wins outright and later fields are never even inspected:

1. `message.content` — the normal path; unchanged behaviour whenever this
   is non-empty.
2. `message.reasoning_content` — the LM Studio/DeepSeek-style field name.
3. `message.reasoning` — an alternate name a minority of other servers use.

Text recovered from either reasoning field goes through the exact same
strict, one-repair-attempt parse and contract validation as `content`
does (see `response.go`'s `responseText`/`parseWireProposal`) — this
package still never fabricates a `Proposal` out of reasoning prose that
merely looks plausible. A reasoning field that does not hold a valid
proposal surfaces as the same typed `*openai.InvalidResponseError`, with
its `Source` field naming which field the text came from.

## Error taxonomy

| Go type | When | Notes |
|---|---|---|
| `*openai.AuthenticationError` | HTTP 401/403 | Bad, missing or under-scoped `APIKey`. Not retryable without fixing the key; no `json_object` fallback attempted (a schema rejection this is not). |
| `*openai.RateLimitError` | HTTP 429 | Retryable after backoff — this package does not retry internally. No fallback attempted either. |
| `*openai.FallbackFailedError` | Both the `json_schema` and the `json_object` attempts failed at the HTTP/transport level | Wraps both underlying errors (`Unwrap() []error`). |
| `*openai.InvalidResponseError` | Unparseable/contract-violating reply, or a response with no usable text in `content`/`reasoning_content`/`reasoning`, from either attempt | Carries `Raw` (truncated), `FinishReason` (called out explicitly, with a truncation hint, when `"length"`), and `Source` (which field `Raw` came from — named in the message only for a reasoning field, never for the normal `content` path). Never a fabricated `Proposal`. |
| wrapped generic error | A connection-level failure (DNS, connection refused, cancelled context) before any HTTP response at all | No fallback attempted — there is nothing to fall back from. |

## Usage and cost

`Usage.Model`, `InputTokens`, `OutputTokens` and `Latency` are read from the
response and the call's wall-clock duration. `Usage.Model` falls back to
`Config.Model` when the response omits `"model"`. `InputTokens`/
`OutputTokens` stay zero — never guessed — when the response carries no
`"usage"` block at all (some OpenAI-compatible servers omit it).

**`Usage.Cost` is always left `nil`.** Unlike `actor/anthropic`'s dated
pricing snapshot (a single hosted vendor with a stable published price
list — see `../anthropic/pricing.go`), this package fronts arbitrary
local/self-hosted servers with no shared pricing source of truth, and most
of them are free to run locally anyway — the whole point of this package.
A caller that wants `Usage.Cost` populated for a hosted OpenAI-compatible
endpoint with a known rate prices it themselves from `InputTokens`/
`OutputTokens` after `Propose` returns; the pricing-snapshot mechanism is
deliberately **not** replicated here.

## Cassette workflow (record once, replay free)

Identical to `actor/anthropic`'s — `Provider` is a plain `actor.Provider`:

```go
provider, err := openai.New(openai.Config{BaseURL: "http://localhost:11434/v1", Model: "qwen3.6:latest"})
cassette := actor.NewCassette("actor/openai model=qwen3.6:latest")
recorder, err := actor.NewCassetteProvider(actor.ModeRecord, provider, cassette)

// ... run the campaign/loop against recorder ...

err = recorder.Cassette().Save("testdata/cassettes/my-campaign.json")
```

CI replays with `actor.ModeReplay` and no local server running at all — see
`../cassette.go` and `../anthropic/README.md`'s own "Cassette workflow"
section for the full record/commit/replay contract, which this package
follows unchanged.

## Testing

- `go test ./actor/openai/...` runs the full suite — request shape, all
  four proposal kinds, the JSON-repair path, the `reasoning_content`/
  `reasoning` fallback fields (valid JSON, garbage, and `content`-wins
  precedence — see `TestPropose_ReasoningContentFallback_ValidJSON` and
  neighbours in `provider_test.go`), the `finish_reason=length` truncation
  hint (`TestPropose_FinishReasonLength_SurfacesInError`), the
  `json_schema`→`json_object` fallback (and its own failure path), the
  full error taxonomy, missing-usage/missing-model degradation, cassette
  record/replay, and a greetbot campaign end-to-end proof whose bundle
  validates against the `chatwright.dev/sdk` module's own
  `formats/run-bundle/v1/schema.json` (resolved via `go list -m`, see
  `e2e_test.go`'s `sdkSchemaPath`) — entirely against
  a fake `httptest.Server` (see `helpers_test.go`) or a fake
  `http.RoundTripper` for the two transport-failure edge cases, never the
  real network.
- One optional **live local-LLM smoke test**, gated behind
  `CHATWRIGHT_LIVE_LOCAL_LLM=1` and a set `CHATWRIGHT_LOCAL_LLM_BASE_URL` —
  skipped with a clear message otherwise, so `go test ./...` never depends
  on a local server being up:

  ```sh
  CHATWRIGHT_LIVE_LOCAL_LLM=1 \
  CHATWRIGHT_LOCAL_LLM_BASE_URL=http://localhost:11434/v1 \
  CHATWRIGHT_LOCAL_LLM_MODEL=qwen3.6:latest \
    go test ./actor/openai/ -run TestLiveLocal -v
  ```

  `CHATWRIGHT_LOCAL_LLM_MODEL` is optional — when unset, the test asks the
  server's own `GET {baseURL}/models` for its catalogue and uses whichever
  model comes first. See `live_test.go`'s own doc comment for the LM
  Studio invocation too.
