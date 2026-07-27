# Changelog

## 0.1.2

- Telegram's emulator now captures and projects `sendRichMessage` and rich
  `editMessageText` payloads, returning a normal `Message` with its assigned
  `message_id` and preserving inline keyboards for scenario assertions.
- It rejects malformed rich-message modes, malformed explicit blocks, and
  invalid inline keyboards with Telegram-shaped HTTP 400 responses. This
  prevents a bot's unsupported or invalid rich payload from becoming a
  false-green Chatwright scenario.

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
