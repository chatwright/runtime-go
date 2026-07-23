# chatwright.dev/runtime — instructions for AI agents and humans

- This repository follows the conventions of the Chatwright standard
  repository's AGENTS.md
  ([github.com/chatwright/chatwright](https://github.com/chatwright/chatwright)).
- Docs use British English; Go code/comments may use American English; never
  mixed within a file.
- Go: `gofmt` clean, `go vet ./...`, `go test -race ./...` before pushing.
- The four end-to-end gates are release gates — all must pass before any
  release: the greetbot scripted campaign (`campaign`), the bundle e2e
  (`run`'s `TestScriptedCampaignBundleAgainstGreetbotEndToEnd`), the
  two-part hybrid proof (`run`'s `TestTwoPartGreetbotProof`), and pybot
  (`examples/pybot`, a real Python subprocess — a skip is not a pass).
- Wire shapes are owned by `chatwright.dev/sdk` — never redefine a run-bundle
  wire type in this repository; convert to the sdk's types at the assembly
  seam (`run/wire.go`) instead.
