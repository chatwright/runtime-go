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
