# Changelog

## Unreleased

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
