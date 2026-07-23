package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/cw"
	"chatwright.dev/runtime/examples/greetbot"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/platform"
	"chatwright.dev/runtime/run"
	"chatwright.dev/runtime/telegram"
	"chatwright.dev/sdk"
)

// sdkSchemaPath resolves the run-bundle JSON Schema committed inside the
// chatwright.dev/sdk module — the wire model's owner — as actually resolved
// by this build's go.mod, via `go list -m` (an offline lookup into the local
// module cache; the module is already downloaded, so no network is
// involved). Reading the schema from the resolved dependency, rather than
// keeping a copy in this repository, makes the validated schema drift-free
// by construction: it is byte-for-byte the file shipped in the exact sdk
// version these bundles are assembled with. GOWORK=off mirrors this
// module's own gate invocations (a stray user-level go.work must not
// redirect the lookup).
func sdkSchemaPath(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "chatwright.dev/sdk")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("resolve the chatwright.dev/sdk module directory (go list -m): %v\n%s", err, stderr)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatal("go list -m -f {{.Dir}} chatwright.dev/sdk returned an empty directory")
	}
	return filepath.Join(dir, "formats", "run-bundle", "v1", "schema.json")
}

// compileSchema and validateBundleFile mirror the sdk repository's own
// schema_test.go helpers of the same names, compiling the schema file
// sdkSchemaPath resolves. Shared by this file's TestTwoPartGreetbotProof
// and bundle_e2e_test.go's e2e gate (both package run_test).
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	schemaPath := sdkSchemaPath(t)
	sch, err := jsonschema.NewCompiler().Compile(schemaPath)
	if err != nil {
		t.Fatalf("compile %s: %v", schemaPath, err)
	}
	return sch
}

func validateBundleFile(t *testing.T, schema *jsonschema.Schema, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	inst, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if err := schema.Validate(inst); err != nil {
		t.Fatalf("%s does not validate against the sdk run-bundle schema:\n%v", path, err)
	}
}

// onboardingInput is the trivial input type for the onboarding fragment
// below — everything it needs is closed over from the test itself (the
// shared emulator, chat and user), not passed as typed input.
type onboardingInput struct{}

// TestTwoPartGreetbotProof is the hybrid-runs MVP proof spec/ideas/hybrid-runs.md
// calls for: a two-part run against the real greetbot fixture over the real
// Telegram emulator — Part 1 a deterministic onboarding fragment (send
// "/start", pick English, confirm the language-choice message is edited to
// "Howdy stranger"), Part 2 an ai-goal acknowledgement driven by a
// ScriptedProvider (zero tokens) — over one continuous journal, in one
// sdk.Run.
//
// It proves, end to end, exactly the pieces hybrid-runs.md's "Must-be-true"
// assumption names: "A deterministic fragment and the actor loop can hand
// over mid-conversation without state ambiguity ... fragment-established
// state visible in the AI part's first observation" — see the assertion on
// the Part 2 loop's very first retained Observation below, taken BEFORE the
// AI part itself has sent anything.
func TestTwoPartGreetbotProof(t *testing.T) {
	const chatID = int64(42)
	user := platform.User{ID: 7, FirstName: "Explorer"}

	emu := telegram.NewEmulator()
	t.Cleanup(emu.Close)
	bot := greetbot.New(emu.BotAPIURL(), "TEST:TOKEN")
	srv := httptest.NewServer(bot.Handler())
	t.Cleanup(srv.Close)
	emu.SetWebhook(srv.URL, http.DefaultClient)

	onboardingSource := cw.SourceReference{
		URI:      "https://github.com/chatwright/runtime-go/blob/HEAD/run/run_e2e_test.go#L1",
		Revision: "HEAD",
	}
	onboardingFragment := cw.Fragment[onboardingInput]{
		Definition:  cw.Definition{Name: "greetbot-onboarding", Source: onboardingSource},
		CloneInputs: func(in onboardingInput) onboardingInput { return in },
		Execute: func(ec *cw.ExecutionContext, _ onboardingInput) error {
			if err := emu.SubmitText(chatID, user, "/start"); err != nil {
				return fmt.Errorf("submit /start: %w", err)
			}
			picker, ok := emu.WaitForMessage(chatID, 0, 2*time.Second)
			if !ok {
				return errors.New("no reply to /start")
			}
			ec.RecordStep("sent /start, received the language picker", onboardingSource)

			var englishID string
			for _, row := range picker.Actions {
				for _, act := range row {
					if act.Label == "English" {
						englishID = act.ID
					}
				}
			}
			if englishID == "" {
				return fmt.Errorf("no \"English\" action offered among %+v", picker.Actions)
			}

			if err := emu.SubmitClick(chatID, user, englishID, picker.MessageID); err != nil {
				return fmt.Errorf("submit click: %w", err)
			}
			edited, ok := emu.WaitForEdit(chatID, picker.MessageID, picker.Version, 2*time.Second)
			if !ok {
				return errors.New("language-choice message was not edited")
			}
			if edited.Text != "Howdy stranger" {
				return fmt.Errorf("edited text = %q, want %q", edited.Text, "Howdy stranger")
			}
			if _, err := ec.Checkpoint("onboarding-complete", onboardingSource); err != nil {
				return err
			}
			return nil
		},
	}
	onboardingPart := run.NewDeterministicPart("onboarding", "Onboarding: pick English",
		"", onboardingFragment, cw.EffectiveInputs[onboardingInput]{})

	acknowledgeGoal := goal.Goal{
		ID: "acknowledge-greeting", Title: "Acknowledge the greeting the onboarding fragment already established",
		Tasks: []goal.Task{{
			ID: "acknowledge", Title: "Acknowledge the greeting",
			SuccessCriteria: `the actor's first observation already shows "Howdy stranger", and it sends an acknowledgement`,
		}},
		Budgets: goal.Budgets{MaxSteps: 10, MaxDuration: time.Minute},
	}
	provider := actor.NewScriptedProvider(actor.Usage{Model: "scripted-v1", InputTokens: 8, OutputTokens: 3},
		actor.Proposal{Kind: actor.ProposeSendText, Text: "Thanks!", Rationale: "acknowledge the greeting already established by onboarding"},
		actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "acknowledged"},
	)
	acknowledgePart := run.NewAIGoalPart("acknowledge", "AI acknowledgement", "", run.AIGoalPartInput{
		ActorID: "explorer", Goal: acknowledgeGoal, Provider: provider,
		Config: actor.Config{ChatID: chatID, User: user},
	})

	r := run.Run{
		ID:          "greetbot-two-part-proof",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: time.Now},
		Parts:       []run.Part{onboardingPart, acknowledgePart},
	}

	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(result.Parts) != 2 || len(result.Skipped) != 0 {
		t.Fatalf("result = %+v, want exactly two executed parts and none skipped", result)
	}
	onboardingOutcome, acknowledgeOutcome := result.Parts[0], result.Parts[1]
	if onboardingOutcome.Status != run.PartCompleted {
		t.Fatalf("onboarding outcome = %+v, want PartCompleted", onboardingOutcome)
	}
	if acknowledgeOutcome.Status != run.PartCompleted {
		t.Fatalf("acknowledge outcome = %+v, want PartCompleted", acknowledgeOutcome)
	}

	// Must-be-true assumption: the AI part's very first observation — taken
	// before it has sent anything itself — already shows the state the
	// deterministic onboarding fragment established.
	if acknowledgeOutcome.AIGoal == nil || len(acknowledgeOutcome.AIGoal.Observations) == 0 {
		t.Fatalf("acknowledgeOutcome.AIGoal = %+v, want at least one retained observation", acknowledgeOutcome.AIGoal)
	}
	firstObservation := acknowledgeOutcome.AIGoal.Observations[0]
	if firstObservation.Sequence != 1 {
		t.Fatalf("first retained observation Sequence = %d, want 1 (the AI part's own first Observe() call)", firstObservation.Sequence)
	}
	sawGreeting := false
	for _, m := range firstObservation.Observation.Messages {
		if m.Actor == sdk.MessageActorBot && m.Text == "Howdy stranger" {
			sawGreeting = true
		}
	}
	if !sawGreeting {
		t.Fatalf("AI part's first observation = %+v, want the onboarding fragment's \"Howdy stranger\" already visible", firstObservation.Observation.Messages)
	}

	// The two boundaries are adjacent, non-overlapping and together cover
	// the whole journal.
	entries, err := emu.Journal(chatID)
	if err != nil {
		t.Fatalf("Journal() error = %v", err)
	}
	if len(onboardingOutcome.Boundary.Chats) != 1 || len(acknowledgeOutcome.Boundary.Chats) != 1 {
		t.Fatalf("boundaries = onboarding:%+v acknowledge:%+v, want exactly one chat boundary each", onboardingOutcome.Boundary, acknowledgeOutcome.Boundary)
	}
	onboardingChat, acknowledgeChat := onboardingOutcome.Boundary.Chats[0], acknowledgeOutcome.Boundary.Chats[0]
	if onboardingChat.FirstEntry != 0 {
		t.Fatalf("onboarding boundary FirstEntry = %d, want 0", onboardingChat.FirstEntry)
	}
	if acknowledgeChat.FirstEntry != onboardingChat.FirstEntry+onboardingChat.EntryCount {
		t.Fatalf("acknowledge boundary FirstEntry = %d, want %d (adjacent to onboarding's end)", acknowledgeChat.FirstEntry, onboardingChat.FirstEntry+onboardingChat.EntryCount)
	}
	if got, want := acknowledgeChat.FirstEntry+acknowledgeChat.EntryCount, len(entries); got != want {
		t.Fatalf("combined boundaries cover %d entries, want %d (the whole journal)", got, want)
	}

	// Assemble the bundle: the roster (the scripted client-side actor that
	// drove both parts, and the bot-under-test), the run's continuous
	// journal, and the ordered parts run.AssembleBundleRun derived.
	actors := []sdk.Actor{
		{
			ID: "explorer", Type: sdk.ActorScripted, Name: user.FirstName,
			PlatformIdentities: map[string]sdk.PlatformIdentity{
				"telegram": {UserID: user.ID, FirstName: user.FirstName},
			},
		},
		{
			ID: "greetbot", Type: sdk.ActorBot, Name: "ChatwrightBot",
			PlatformIdentities: map[string]sdk.PlatformIdentity{
				"telegram": {UserID: telegram.EmulatedBotUserID, FirstName: "ChatwrightBot"},
			},
		},
	}
	chats := []sdk.ChatJournal{run.WireJournal(chatID, entries)}

	bundleRun := run.AssembleBundleRun(run.AssembleBundleRunInput{
		RunID: "run-1", Platform: "telegram", EndpointProfile: sdk.EndpointProfilePlatformEmulated,
		Actors: actors, Chats: chats, Result: result,
	})
	if len(bundleRun.Parts) != 2 {
		t.Fatalf("bundleRun.Parts = %+v, want exactly two parts", bundleRun.Parts)
	}
	if bundleRun.Parts[0].Kind != sdk.PartKindDeterministic || bundleRun.Parts[0].AIGoal != nil {
		t.Fatalf("bundleRun.Parts[0] = %+v, want kind=deterministic with no aiGoal section", bundleRun.Parts[0])
	}
	if bundleRun.Parts[1].Kind != sdk.PartKindAIGoal || bundleRun.Parts[1].AIGoal == nil {
		t.Fatalf("bundleRun.Parts[1] = %+v, want kind=ai-goal with a populated aiGoal section", bundleRun.Parts[1])
	}

	b := sdk.Bundle{
		Format: sdk.FormatV1,
		Metadata: sdk.Metadata{
			CreatedAt:         time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC),
			ChatwrightVersion: sdk.ModuleVersion(),
		},
		Runs: []sdk.Run{bundleRun},
	}

	path := filepath.Join(t.TempDir(), "greetbot-two-part.chatwright.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := sdk.Write(f, b); err != nil {
		_ = f.Close()
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	readBack, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = readBack.Close() }()
	decoded, err := sdk.Read(readBack)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(decoded.Runs) != 1 || len(decoded.Runs[0].Parts) != 2 {
		t.Fatalf("decoded = %+v, want one run with two parts", decoded)
	}

	var rewritten bytes.Buffer
	if err := sdk.Write(&rewritten, decoded); err != nil {
		t.Fatalf("Write(decoded) error = %v", err)
	}
	if string(written) != rewritten.String() {
		t.Fatalf("write/read/write cycle is not byte-identical:\nfirst:\n%s\nsecond:\n%s", written, rewritten.String())
	}

	validateBundleFile(t, compileSchema(t), path)
}
