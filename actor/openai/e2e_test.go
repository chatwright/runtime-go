package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/actor/openai"
	"chatwright.dev/runtime/examples/greetbot"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
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
// redirect the lookup). Duplicated from run/run_e2e_test.go's own
// unexported helper of the same name — this package cannot reference it
// directly (a different package).
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

// compileSchema and validateBundleFile duplicate run/run_e2e_test.go's own
// unexported helpers of the same names — this package cannot reference them
// directly (a different package), the same reason run/run_e2e_test.go's own
// doc comment gives for duplicating the sdk repository's schema_test.go
// approach.
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

// TestGreetbotCampaignThroughOpenAIProvider_BundleValidates is this
// package's principle-6 proof (AGENTS.md #6: "each feature proves its
// existence"): a greetbot campaign driven end to end through THIS
// package's Provider — real HTTP POSTs to a fake OpenAI-compatible
// httptest.Server that returns scripted-but-valid proposals, exactly the
// wire shape a real Ollama/LM Studio server would send — completes, and
// the resulting run.Run assembles into a sdk.Bundle that validates against
// the chatwright.dev/sdk module's own formats/run-bundle/v1/schema.json
// (see sdkSchemaPath).
//
// It mirrors campaign/e2e_test.go's own greetbot proof (including its
// dry-run technique for learning the real, opaque "English" action id
// ahead of time — see dryRunLearnEnglishActionID below, duplicated from
// that file since this is a different package) and run/run_e2e_test.go's
// bundle-assembly/schema-validation shape, narrowed to a single ai-goal
// run.Part since this package has no deterministic fragment to compose
// with.
func TestGreetbotCampaignThroughOpenAIProvider_BundleValidates(t *testing.T) {
	const chatID = int64(42)
	user := platform.User{ID: 7, FirstName: "Explorer"}

	englishActionID := dryRunLearnEnglishActionID(t, chatID, user)

	emu := telegram.NewEmulator()
	t.Cleanup(emu.Close)
	bot := greetbot.New(emu.BotAPIURL(), "TEST:TOKEN")
	botSrv := httptest.NewServer(bot.Handler())
	t.Cleanup(botSrv.Close)
	emu.SetWebhook(botSrv.URL, http.DefaultClient)

	// The fake OpenAI-compatible server: a fixed, ordered script of valid
	// proposals — the same script actor.NewScriptedProvider would run, but
	// delivered over real HTTP through openai.Provider, exactly as the
	// principle-6 proof requires ("driven through THIS provider against
	// the fake server").
	script := []string{
		`{"kind":"send-text","text":"/start","action_id":"","rationale":"open the language picker"}`,
		fmt.Sprintf(`{"kind":"click","text":"","action_id":%q,"rationale":"pick English"}`, englishActionID),
		`{"kind":"task-done","text":"","action_id":"","rationale":"the bot confirmed the greeting"}`,
	}
	var mu sync.Mutex
	next := 0
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := next
		next++
		mu.Unlock()
		if i >= len(script) {
			t.Errorf("fake LLM server got more requests (%d) than the script has entries (%d)", i+1, len(script))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-e2e", "object": "chat.completion", "model": "fake-e2e-model",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": script[i]},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 6, "total_tokens": 26},
		})
	}))
	t.Cleanup(llmSrv.Close)

	provider, err := openai.New(openai.Config{BaseURL: llmSrv.URL, Model: "fake-e2e-model"})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}

	g := goal.Goal{
		ID: "greetbot-language", Title: "Select a language and confirm the bot responds",
		Tasks: []goal.Task{{
			ID: "select-language", Title: "Pick English and confirm the greeting",
			SuccessCriteria: `the language-choice message is edited to show "Howdy stranger"`,
		}},
		Budgets: goal.Budgets{MaxSteps: 10, MaxDuration: time.Minute},
	}

	aiPart := run.NewAIGoalPart("select-language", "AI: select language", "", run.AIGoalPartInput{
		ActorID: "explorer", Goal: g, Provider: provider,
		Config: actor.Config{ChatID: chatID, User: user},
	})

	r := run.Run{
		ID:          "openai-greetbot-proof",
		Environment: run.Environment{Emulator: emu, ChatIDs: []int64{chatID}, Now: time.Now},
		Parts:       []run.Part{aiPart},
	}

	result, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(result.Parts) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("result = %+v, want exactly one executed part and none skipped", result)
	}
	outcome := result.Parts[0]
	if outcome.Status != run.PartCompleted {
		t.Fatalf("outcome = %+v, want PartCompleted", outcome)
	}

	if transcript := emu.Transcript(chatID); !strings.Contains(transcript, "Howdy stranger") {
		t.Fatalf("emulator transcript does not show the bot's confirmed greeting:\n%s", transcript)
	}

	entries, err := emu.Journal(chatID)
	if err != nil {
		t.Fatalf("Journal() error = %v", err)
	}

	actors := []sdk.Actor{
		{
			ID: "explorer", Type: sdk.ActorAIAgent, Name: user.FirstName,
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
	if len(bundleRun.Parts) != 1 || bundleRun.Parts[0].Kind != sdk.PartKindAIGoal || bundleRun.Parts[0].AIGoal == nil {
		t.Fatalf("bundleRun.Parts = %+v, want one kind=ai-goal part with a populated aiGoal section", bundleRun.Parts)
	}

	b := sdk.Bundle{
		Format: sdk.FormatV1,
		Metadata: sdk.Metadata{
			CreatedAt:         time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
			ChatwrightVersion: sdk.ModuleVersion(),
		},
		Runs: []sdk.Run{bundleRun},
	}

	path := filepath.Join(t.TempDir(), "openai-greetbot-proof.chatwright.json")
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

	validateBundleFile(t, compileSchema(t), path)

	mu.Lock()
	calls := next
	mu.Unlock()
	if calls != len(script) {
		t.Errorf("fake LLM server got %d requests, want exactly %d (the script's length)", calls, len(script))
	}
}

// dryRunLearnEnglishActionID duplicates campaign/e2e_test.go's own helper
// of the same name (a different package; see this file's own doc comment
// for why cross-package reuse of a frozen test helper is not done here):
// it drives greetbot's /start through a throwaway, independent Telegram
// emulator + Engine to learn the real observe AvailableAction ID for the
// "English" language button, so the actual test run's fake LLM server
// script can be built with it up front. Deterministic across independent
// emulators because Telegram message IDs are a simple per-chat counter
// starting at 1, so an identical bot driven through an identical opening
// exchange on a second, independent emulator assigns identical IDs — see
// campaign/e2e_test.go's own copy for the fuller rationale.
func dryRunLearnEnglishActionID(t *testing.T, chatID int64, user platform.User) string {
	t.Helper()
	emu := telegram.NewEmulator()
	defer emu.Close()
	bot := greetbot.New(emu.BotAPIURL(), "TEST:TOKEN")
	srv := httptest.NewServer(bot.Handler())
	defer srv.Close()
	emu.SetWebhook(srv.URL, http.DefaultClient)

	if err := emu.SubmitText(chatID, user, "/start"); err != nil {
		t.Fatalf("dry run SubmitText() error = %v", err)
	}

	engine := observe.NewEngine(emu, observe.ChatRef{ChatID: chatID})
	obs, err := engine.Observe()
	if err != nil {
		t.Fatalf("dry run Observe() error = %v", err)
	}
	for _, m := range obs.Messages {
		for _, a := range m.Actions {
			if a.Label == "English" {
				return a.ID
			}
		}
	}
	t.Fatalf("dry run found no \"English\" action among %+v", obs.Messages)
	return ""
}
