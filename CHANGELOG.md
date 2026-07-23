# Changelog

## Unreleased

- `actor.Loop` no longer scores a content-identical re-render — a message
  re-edited in place with byte-identical text and the same action labels,
  only its Version bumping — as progress. Fixes
  [#2](https://github.com/chatwright/runtime-go/issues/2): the model arena's
  non-progress detector was fooled by exactly this shape of idempotent
  re-edit (a model re-clicking an already-activated button), letting a
  model re-click the same button indefinitely without ever tripping
  `Config.NonProgressLimit`. `observe.Engine`'s `Changes` feed is unchanged
  and stays truthful (a version bump is still a recorded Change); only the
  loop's own PROGRESS judgement (`actor.ActionExecuted` vs
  `actor.ActionExecutedNoEffect`) now also requires the change to be
  semantically real — see `observedEffect`/`semanticallyEqualMessage` in
  `actor/loop.go`.
- `actor.Loop.RunTask` now records a `LoopEvent` — carrying `Index`, `At`,
  `TaskID`, `ObservationSequence` and the new `LoopEvent.ProposeError` —
  before returning when `Provider.Propose` errors, instead of the failure
  vanishing from `Loop.Events` (and so from any assembled run bundle) with
  only a returned Go error. Fixes
  [#4](https://github.com/chatwright/runtime-go/issues/4). `RunTask`'s own
  abort-via-returned-error behaviour is otherwise unchanged: a `Propose`
  error still aborts the call, and the campaign is not itself stopped
  (`goal.CampaignState.Abort` is not called) — only the evidence trail
  changes. Requires `chatwright.dev/sdk` >= v0.1.1
  ([chatwright/sdk-go#1](https://github.com/chatwright/sdk-go/pull/1)),
  which added the additive `LoopEvent.ProposeError` wire field;
  `run/wire.go`'s `wireLoopEvent` carries it through mechanically, like
  every other field.
- `arena`: a public package running Chatwright's actor-model comparison
  matrix — the same `Scenario` (goal + platform environment), the same
  budgets, across a declared set of provider/model configurations, N
  repeats each. Ported from the scratchpad harness that produced the first
  actor-model arena report
  ([chatwright/backstage research/model-arena-2026-07-23](https://github.com/chatwright/backstage/tree/main/research/model-arena-2026-07-23)),
  restructured per [spec/ideas/actor-model-arena.md](https://github.com/chatwright/chatwright/blob/main/spec/ideas/actor-model-arena.md):
  `arena.Matrix`/`arena.Run` execute one per-model block (an
  evict-others-then-right-size `Loader` hook, one untimed mandatory
  warm-up with cold-start recorded as its own metric, then N timed
  repeats) sequentially per provider; `arena.WriteReport` renders the
  spec's full metric list (completion vs. independently-verified
  evidence, cold-start, latency p50/p95, wall time, tokens, structured-
  output-mode share, the required retry breakdown, stop reasons, steps,
  per-cell bundle names) plus an environment block recording declared
  hardware, context lengths and each provider's actual load outcome.
  `LMStudioLoader` (the `lms` CLI) and `OllamaLoader` (Ollama's native
  `/api/generate` pre-load) both degrade to a JIT load with a long
  warm-up timeout, never a hard failure, when their tooling is absent —
  recorded in the environment block either way. `GreetbotScenario` is the
  built-in first scenario (send `/start`, click "English", acknowledge
  the in-place-edited greeting, declare done), with a deterministic
  journal-level `Verify` step independent of the model's own claim
  (evidence over claims). Unlike the harness, `Run` never writes a bundle
  or a JSON "sidecar" to disk itself — every cell's per-call detail
  (response-format mode, wall time, transport errors) moves from a
  sidecar file into a typed `CallRecord` inside the returned `Results`;
  callers persist whatever they need.
- `actor/openai`: an OpenAI-compatible `actor.Provider` for
  `POST {BaseURL}/chat/completions` — Ollama, LM Studio, OpenRouter, vLLM and
  OpenAI itself. Ported from
  [github.com/chatwright/chatwright](https://github.com/chatwright/chatwright)
  commit `257d99f`, which landed on the old repository after this module's
  extraction snapshot; adapted to this module's import paths and to
  assembling bundles via `run.AssembleBundleRun`/`run.WireJournal` and
  `chatwright.dev/sdk` types (the pre-split `bundle` package's successor).
  Mirrors `actor/anthropic`'s structured-output contract with a graceful
  `json_schema` → `json_object` one-retry degradation for servers that
  reject strict structured output.
- `actor/openai`: fixed [#3](https://github.com/chatwright/runtime-go/issues/3),
  found by the first actor-model arena run — `qwen/qwen3.6-27b` via LM
  Studio billed 39-54 output tokens on 4/4 calls while `Propose` still
  reported "empty content", because the model routed its entire reply into
  `message.reasoning_content` instead of `message.content`. `Propose` now
  reads `message.reasoning_content`, then the alternate name
  `message.reasoning`, whenever `content` is empty — the same strict,
  never-fabricate parse/validate path `content` already went through, so a
  reasoning field holding prose instead of the proposal JSON still
  surfaces as `*openai.InvalidResponseError` (now carrying a `Source`
  field naming which field was read). `content` continues to win outright
  whenever it is non-empty, so existing behaviour is unchanged for every
  non-reasoning model. Also raised `openai.DefaultMaxTokens` 1024 → 2048,
  matching the value the arena reran every cell at after observing
  `finish_reason=length` truncating replies mid-JSON at 1024; that
  `finish_reason` is now called out explicitly, with a truncation hint, in
  `*openai.InvalidResponseError`'s message.

## 0.1.0

Initial extraction from
[github.com/chatwright/chatwright](https://github.com/chatwright/chatwright).

- `chatwright.dev/runtime` now owns the Chatwright engine: platform
  emulation (`telegram`, `whatsapp`, `platform`) and the testing runtime
  (`observe`, `goal`, `actor`, `campaign`, `datastate`, `branching`, `run`,
  plus the `cw` scenario API — formerly the repository-root `chatwright`
  package).
- Bundle assembly now converts runtime internals to
  [chatwright.dev/sdk](https://github.com/chatwright/sdk-go) wire types:
  `run.SingleAIGoalRun` (moved from the old `bundle` package, taking runtime
  types and wire-converting internally, absorbing `SortObservations`),
  `run.AssembleBundleRun` and the new `run.WireJournal` helper, over the
  mechanical field-by-field converters in `run/wire.go`. The sdk owns every
  wire shape; the emitted JSON is unchanged.
