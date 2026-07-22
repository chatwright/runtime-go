# actor/anthropic

The first real `actor.Provider`: it calls the [Anthropic Messages
API](https://platform.claude.com/docs/en/api/messages) to propose the next
action for an in-flight campaign task. It composes with the frozen `actor`
seam like any other `Provider` — see [`../provider.go`](../provider.go) for
the interface it implements, and
[`spec/plans/goal-driven-mvp-slice-2.md`](../../spec/plans/goal-driven-mvp-slice-2.md)
for the design decisions this package is built to.

## Quick start

```go
import "github.com/chatwright/chatwright/actor/anthropic"

provider, err := anthropic.New(anthropic.Config{})
// Config{} is enough: the API key comes from ANTHROPIC_API_KEY, the model
// defaults to anthropic.DefaultModel, MaxTokens defaults to
// anthropic.DefaultMaxTokens.
```

`Provider` satisfies `actor.Provider`, so it plugs directly into `actor.Loop`
or, for CI, into an `actor.CassetteProvider` — see Cassette workflow below.

## Config and environment variables

| Field | Default | Notes |
|---|---|---|
| `APIKey` | `$ANTHROPIC_API_KEY` | Never read from an `actor.Prompt`, never written to a cassette — see actor's own doctrine on cassette contents. `New` returns `ErrMissingAPIKey` if both are empty. |
| `Model` | `anthropic.DefaultModel` (`claude-haiku-4-5`) | See "Why Haiku 4.5" below. |
| `MaxTokens` | `anthropic.DefaultMaxTokens` (1024) | The response is one small JSON object plus a one-sentence rationale — 1024 tokens is generous headroom, well under the ~16000-token threshold where the Anthropic SDKs require streaming. |
| `HTTPClient`, `BaseURL`, `MaxRetries` | SDK defaults | For tests: point `HTTPClient` at a fake `http.RoundTripper`, or `BaseURL` at an `httptest.Server`. `MaxRetries` overrides the SDK's automatic retry-with-backoff on 429/5xx (default 2) — tests set it to `0` so an error-taxonomy assertion doesn't wait through real backoff delays. |
| `Now` | `time.Now` | Only used to measure `Usage.Latency`; inject a fake clock for deterministic latency assertions. |
| `DisableCostEstimate` | `false` | See Cost estimate below. |

Only `ANTHROPIC_API_KEY` is read from the environment — no other Anthropic
env var (`ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, ...) is consulted by
this package, though the underlying SDK client may still see them if they
happen to be set in the process environment.

## Why `claude-haiku-4-5`

A campaign is many small, latency-sensitive turns per task — choosing one of
four actions from a short, well-structured prompt, not open-ended reasoning
— so the fast/cheap tier is the right default, not the most capable model.
Per Anthropic's current model line-up, `claude-haiku-4-5` is "the fastest and
most cost-effective model for simple tasks" at $1.00 / $5.00 per million
input/output tokens, versus $3+ / $15+ for the Sonnet and Opus tiers. A
caller running a harder campaign (more ambiguous goals, more reasoning to
pick the right action) sets `Config.Model` to a stronger model explicitly —
nothing in this package assumes Haiku.

## Response contract: structured outputs, not a bare JSON prompt

Propose asks the model to reply with **exactly one JSON object** — `kind`
(`send-text` | `click` | `task-done` | `give-up`), `text`, `action_id`,
`rationale` — via Anthropic's
[structured outputs](https://platform.claude.com/docs/en/build-with-claude/structured-outputs)
(`output_config.format` with a `json_schema`), which `claude-haiku-4-5`
supports. This is the reliable route: the API enforces the JSON shape
server-side, rather than us hoping a plain-text instruction is followed.

Even so, `response.go` makes exactly **one repair attempt** if the reply
somehow doesn't parse as-is (e.g. wrapped in prose or a markdown fence): it
retries against the substring from the first `{` to the last `}`. A second
failure — or a reply that parses but violates the contract (missing `kind`,
a `click` with no `action_id`, ...) — returns a typed
`*anthropic.InvalidResponseError` wrapping the raw text. **Propose never
fabricates a Proposal**: on any parse or validation failure it returns the
zero-value `actor.Proposal` and a non-nil error, never a guess.

`ObservationSequence` on a `click` proposal is never taken from the model's
reply — it is always `prompt.Observation.Sequence`, the only observation the
model could have seen that turn. The model only ever needs to name an
`action_id`; Chatwright is the one that stamps which observation it was
chosen from, matching `actor.Proposal.ObservationSequence`'s own contract.

Whether a `click`'s `action_id` is still valid is the loop's job
(`observe.Engine.Validate` against the engine's *current* state) — this
package deliberately does not duplicate that check. Providers are dumb
transports; per `actor`'s design, safety lives in the loop.

## Error taxonomy

| Go type | When | Notes |
|---|---|---|
| `*anthropic.AuthenticationError` | HTTP 401/403 | Bad, missing, revoked or under-scoped API key. Not retryable without fixing the key. |
| `*anthropic.RateLimitError` | HTTP 429 | Retryable after backoff — this package makes exactly `Config.MaxRetries` SDK-level retry attempts (default 2) before returning this. |
| `*anthropic.InvalidResponseError` | Unparseable/contract-violating reply, or a refusal (`stop_reason: "refusal"`, empty content) | Carries `Raw` (truncated) and `StopReason`. Never a fabricated Proposal. |
| wrapped generic error | Any other transport/API failure (5xx, network failure, cancelled context) | Still unwraps to the underlying `*anthropic-sdk-go.Error` via `errors.As`/`errors.Unwrap` when the failure reached the API at all. |

## Usage/cost mapping

`Usage.Model`, `Usage.InputTokens`, `Usage.OutputTokens` and `Usage.Latency`
are read straight from the API response (`usage.input_tokens` /
`usage.output_tokens`) and the call's wall-clock duration.

`Usage.Cost` is filled in automatically for models `pricing.go` has an entry
for (see `PricingUSDPerMillionTokens`), sourced from Anthropic's published
pricing as of **`PricingSnapshotDate` = 2026-06-24**
(<https://platform.claude.com/docs/en/pricing>) — this is a point-in-time
snapshot, not a live price feed, and Anthropic can change list prices at any
time. Every entry is the model's **standard, non-promotional** rate:
`claude-sonnet-5` in particular carries a temporary lower "intro" rate
through 2026-08-31 that is deliberately *not* used, so an estimated spend
against `goal.Budgets.MaxCost` never understates the model's steady-state
cost. A model with no pricing-table entry leaves `Usage.Cost` nil rather than
guess. Set `Config.DisableCostEstimate` to leave it nil unconditionally.
Treat `Usage.Cost` as a budgeting estimate, never an invoice — refresh
`pricing.go` (and `PricingSnapshotDate`) when it drifts from the source URL
above.

## Cassette workflow (record once, replay free)

`Provider` is a plain `actor.Provider`, so it wraps in
`actor.CassetteProvider` exactly like the `ScriptedProvider` docs describe:

```go
provider, err := anthropic.New(anthropic.Config{}) // ANTHROPIC_API_KEY set
cassette := actor.NewCassette("actor/anthropic model=" + anthropic.DefaultModel)
recorder, err := actor.NewCassetteProvider(actor.ModeRecord, provider, cassette)

// ... run the campaign/loop against recorder ...

err = recorder.Cassette().Save("testdata/cassettes/my-campaign.json")
```

1. **Record once**, locally, with a real `ANTHROPIC_API_KEY` set and
   `actor.ModeRecord`. Every `Propose` call is appended to the cassette,
   keyed by a hash of the provider config plus the exact `actor.Prompt` sent
   — see [`../cassette.go`](../cassette.go).
2. **Commit the cassette** under `testdata/cassettes/` — it is
   human-readable, indented JSON with no provider auth in it (API keys never
   enter an `actor.Prompt`, so there is nothing to redact), safe to review in
   a PR diff.
3. **CI replays it** with `actor.ModeReplay` and no API key at all: a cache
   hit returns the recorded `Proposal`/`Usage` verbatim, at zero token cost;
   a cache miss (the campaign's behaviour changed enough to ask a new
   question) is a hard test failure naming the missing prompt, never a
   silent live fallback.

Re-record whenever the campaign's goal/task/observation shape changes enough
that the old cassette no longer covers the prompts a run actually asks.
Changing this package's own prompt rendering (`prompt.go`,
`promptContractVersion`) does **not** by itself require re-recording — a
cassette entry's lookup key is a hash of the canonical `actor.Prompt` JSON,
not of anything this package renders from it — but a rendering change can
still shift what the *live* model would say next time you do re-record, so
bump `promptContractVersion` alongside any change that could plausibly
affect model behaviour, as a breadcrumb for whoever reviews the next
recording.

## Testing

- `go test ./actor/anthropic/...` runs the full suite above at zero token
  cost — everything is driven through a fake `http.RoundTripper` (see
  `helpers_test.go`), never the network.
- One optional **live smoke test**, gated behind `CHATWRIGHT_LIVE_LLM=1` AND
  a set `ANTHROPIC_API_KEY` — skipped with a clear message otherwise, so
  `go test ./...` never spends a token in CI:

  ```sh
  CHATWRIGHT_LIVE_LLM=1 ANTHROPIC_API_KEY=sk-ant-... \
    go test ./actor/anthropic/ -run TestLive -v
  ```
