# Changelog

## Unreleased

- **Evidence-defined completion**
  ([spec/ideas/evidence-defined-completion.md](https://github.com/chatwright/chatwright/blob/main/spec/ideas/evidence-defined-completion.md)):
  `goal.Task` gained an optional, Go-only, never-serialised `Criteria`
  field (`func(ctx, observe.Observation) (bool, error)`, `json:"-"`) —
  a machine-checkable completion predicate alongside the prose
  `SuccessCriteria` the actor reads. `actor.Loop.RunTask` evaluates it
  after every `ActionExecuted` action against the fresh post-action
  observation; the moment it holds, the task completes deterministically
  via the new `goal.CampaignState.CompleteByEvidence` (new stop reason
  `goal.StopGoalMetByEvidence`, used exactly when this completion is what
  ends the whole campaign — otherwise identical to `Complete`). Unless
  `actor.Config.DisableOvershootProbe`, one more `Provider.Propose` call is
  then requested, recorded (new `actor.ActionOvershootProbe` outcome kind)
  and never executed — the "overshoot probe" the idea describes, catching
  a model that keeps proposing after the goal is already met (the exact
  shape the first arena run observed: `ollama/qwen3.6:latest` re-clicking
  an already-activated button 10 extra times). `campaign.Assemble` now
  classifies every such recorded-but-never-executed event as the new
  `campaign.FindingActorOvershoot` finding, regardless of the owning
  task's eventual status. A premature `task-done` proposed while
  `Criteria` is set and still false is rejected — recorded as
  `ActionSkippedInvalid`, the task continues — reusing (unchanged) the
  pre-existing `ai-navigation-failure` classification, never a new finding
  kind. Requires `chatwright.dev/sdk` >= v0.2.0
  ([chatwright/sdk-go#2](https://github.com/chatwright/sdk-go/pull/2)),
  which widened `ActionOutcomeKind` (`overshoot-probe`,
  `blocked-constraint-violation` — see below) and `FindingKind`
  (`actor-overshoot`, `constraint-violation`); `goal.StopGoalMetByEvidence`
  needs no sdk change at all, since `Report.StopReason` is already a plain
  wire string, never a closed enum.
- **Proposal content constraints**
  ([spec/ideas/proposal-content-constraints.md](https://github.com/chatwright/chatwright/blob/main/spec/ideas/proposal-content-constraints.md)):
  `goal.Task` and `goal.Goal` both gained an optional, Go-only, never-
  serialised `ContentRules` field (new type in `goal/content_rules.go`:
  a case-insensitive `Vocabulary` allowlist, regexp `DenyPatterns`, and a
  custom `Predicate` seam — all deterministic, no semantic/NLP judgement).
  `goal.EffectiveContentRules` resolves the idea's own open question
  ("task overriding goal"): a Task's own non-empty `ContentRules` entirely
  overrides its Goal's, never merges with it. `actor.Loop`'s validate
  stage now checks every `ProposeSendText` proposal's text before
  submitting it: a violation is blocked before it ever reaches the bot,
  recorded as the new `actor.ActionBlockedConstraintViolation` outcome
  kind, and re-prompted, counting toward `Config.NonProgressLimit` exactly
  like any other invalid proposal. `campaign.Assemble` classifies every
  such event as the new `campaign.FindingConstraintViolation` finding.
  Text-only for this MVP (click proposals are not constrainable — the
  idea's own open question, left for later).
- **Campaign progress reporting**
  ([spec/ideas/campaign-progress-reporting.md](https://github.com/chatwright/chatwright/blob/main/spec/ideas/campaign-progress-reporting.md)):
  `actor.Config` gained an optional `OnProgress func(actor.ProgressSnapshot)`
  callback (new `actor/progress.go`), called once per loop iteration and
  once at each task-start/task-end boundary with the idea's "three honest
  gauges" — goal progress (task j/m, tasks completed), budget burn (steps/
  duration/cost/repeated-failures as fractions of their maxima), and
  health (non-progress streak, retry counts by `ActionOutcomeKind`) — pure
  derived state, nothing added to `Loop.Events`, `campaign.Report` or any
  run bundle. `run.Run` gained the matching `OnProgress
  func(run.ProgressSnapshot)` (new `run/progress.go`), wrapping the idea's
  "part k/n" gauge around every Part boundary
  (`PartProgressStarted`/`PartProgressCompleted`, for every Part kind) and
  forwarding each ai-goal Part's own `actor.ProgressSnapshot`
  (`PartProgressTask`). `arena.RunOptions` gained an optional
  `ProgressWriter io.Writer`, wired through `run.Run.OnProgress` per cell
  via a minimal internal `formatProgressLine` helper, so a future arena
  run can print live stage lines (e.g. "model 2/4 · repeat 1/3 · part 1/1
  [part-task] · task 1/2 · step 5 · steps-burn 42% · non-progress 0").
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
