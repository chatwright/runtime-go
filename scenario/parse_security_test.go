package scenario

import (
	"os"
	"strings"
	"testing"
)

// validBaseDocument is a minimal, otherwise-valid document string each
// security test mutates one field of — kept as a template so every test
// below exercises exactly one rule at a time.
const validBaseDocument = `{
  "format": "https://chatwright.dev/formats/scenario-document/v1",
  "schemaVersion": 1,
  "id": "t",
  "version": "v1",
  "title": "t",
  "requires": ["ai-goal", "exampleBot:greetbot"],
  "fidelity": {"endpointProfile": "platform-emulated", "environment": "dev", "dataSensitivity": "synthetic"},
  "platform": "telegram",
  "chats": [{"id": "main", "platformChatId": 42}],
  "bot": {"id": "b", "name": "B", "exampleBot": "greetbot"},
  "cast": [
    {
      "id": "arena", "type": "ai-agent", "name": "Arena",
      "platformIdentity": {"userId": 7, "firstName": "Arena"},
      "provider": {"kind": "cassette", "mode": "replay", "cassette": "c.json"}
    }
  ],
  "parts": [
    {
      "id": "p1", "kind": "ai-goal", "chat": "main", "actorId": "arena",
      "goal": {
        "id": "g1", "title": "g",
        "tasks": [{"id": "t1", "successCriteria": "sc"}],
        "budgets": {"maxSteps": 5}
      }
    }
  ]
}`

func mustErrorCode(t *testing.T, report Report, wantCode string) Issue {
	t.Helper()
	for _, issue := range report.Errors() {
		if issue.Code == wantCode {
			return issue
		}
	}
	t.Fatalf("no error-severity issue with code %q found; errors = %+v", wantCode, report.Errors())
	return Issue{}
}

// --- AC: inline-secret-is-rejected ---

func TestParse_InlineSecretIsRejected(t *testing.T) {
	doc := strings.Replace(validBaseDocument,
		`"provider": {"kind": "cassette", "mode": "replay", "cassette": "c.json"}`,
		`"provider": {"kind": "model", "providerId": "openai", "model": "gpt-x", "apiKey": "sk-live-THIS-IS-A-SECRET"}`,
		1)

	parsed, report, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() error = nil, want rejection for an inline apiKey literal")
	}
	if parsed != nil {
		t.Fatal("Parse() returned a non-nil Document for a rejected document")
	}
	issue := mustErrorCode(t, report, "inline-secret")
	if issue.Pointer != "/cast/0/provider/apiKey" {
		t.Errorf("issue.Pointer = %q, want /cast/0/provider/apiKey", issue.Pointer)
	}
	if strings.Contains(issue.Message, "sk-live-THIS-IS-A-SECRET") {
		t.Fatalf("issue.Message echoes the secret literal: %q", issue.Message)
	}
	if err.Error() != "" && strings.Contains(err.Error(), "sk-live-THIS-IS-A-SECRET") {
		t.Fatalf("Parse() error echoes the secret literal: %v", err)
	}
}

// TestParse_SecretRefShapeViolationsAreRejected covers the sibling-member
// and undeclared-name variants of the same rule — a $secretRef with a
// disallowed sibling (the "${VAR:-literal}"-shaped default the README
// warns about) and a secretRef naming a secret never declared in
// "secrets".
func TestParse_SecretRefShapeViolationsAreRejected(t *testing.T) {
	t.Run("sibling member (default) is rejected", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument,
			`"provider": {"kind": "cassette", "mode": "replay", "cassette": "c.json"}`,
			`"provider": {"kind": "model", "providerId": "openai", "model": "gpt-x", "apiKey": {"secretRef": "k", "default": "fallback-secret"}}`,
			1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a secretRef carrying a sibling default")
		}
		issue := mustErrorCode(t, report, "secret-ref-shape")
		if issue.Pointer != "/cast/0/provider/apiKey" {
			t.Errorf("issue.Pointer = %q, want /cast/0/provider/apiKey", issue.Pointer)
		}
	})

	t.Run("undeclared secret name is rejected", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument,
			`"provider": {"kind": "cassette", "mode": "replay", "cassette": "c.json"}`,
			`"provider": {"kind": "model", "providerId": "openai", "model": "gpt-x", "apiKey": {"secretRef": "never-declared"}}`,
			1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a secretRef naming an undeclared secret")
		}
		mustErrorCode(t, report, "secret-ref-undeclared")
	})
}

// --- AC: credential-bearing-url-is-rejected ---

func TestParse_CredentialBearingURLIsRejected(t *testing.T) {
	base := strings.Replace(validBaseDocument, `"exampleBot": "greetbot"`, `"transport": "http", "delivery": "webhook", "url": "URLPLACEHOLDER"`, 1)
	base = strings.Replace(base, `"requires": ["ai-goal", "exampleBot:greetbot"],`, `"requires": ["ai-goal"],`, 1)

	t.Run("userinfo", func(t *testing.T) {
		doc := strings.Replace(base, "URLPLACEHOLDER", "https://user:CorrectHorseBattery@bot.example.com/webhook", 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a URL carrying userinfo")
		}
		issue := mustErrorCode(t, report, "credential-bearing-url")
		if issue.Pointer != "/bot/url" {
			t.Errorf("issue.Pointer = %q, want /bot/url", issue.Pointer)
		}
		if strings.Contains(issue.Message, "CorrectHorseBattery") {
			t.Fatalf("issue.Message echoes the credential: %q", issue.Message)
		}
	})

	t.Run("token query parameter", func(t *testing.T) {
		doc := strings.Replace(base, "URLPLACEHOLDER", "https://bot.example.com/webhook?token=abc123SECRET", 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a URL carrying a token query parameter")
		}
		issue := mustErrorCode(t, report, "credential-bearing-url")
		if strings.Contains(issue.Message, "abc123SECRET") {
			t.Fatalf("issue.Message echoes the credential: %q", issue.Message)
		}
	})

	t.Run("api_key query parameter, underscore/case variant", func(t *testing.T) {
		doc := strings.Replace(base, "URLPLACEHOLDER", "https://bot.example.com/webhook?api_key=abc123SECRET", 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a URL carrying an api_key query parameter")
		}
		mustErrorCode(t, report, "credential-bearing-url")
	})

	t.Run("plain url with no credentials is accepted", func(t *testing.T) {
		doc := strings.Replace(base, "URLPLACEHOLDER", "https://bot.example.com/webhook", 1)
		_, report, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("Parse() error = %v, want no error for a credential-free URL; report=%+v", err, report)
		}
	})
}

// --- AC: executable-string-members-are-rejected ---

func TestParse_ExecutableStringMembersAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(doc string) string
	}{
		{"command at document root", func(doc string) string {
			return strings.Replace(doc, `"format":`, `"command": "rm -rf /", "format":`, 1)
		}},
		{"script nested in bot", func(doc string) string {
			return strings.Replace(doc, `"exampleBot": "greetbot"`, `"exampleBot": "greetbot", "script": "curl evil.example"`, 1)
		}},
		{"shell nested in goal", func(doc string) string {
			return strings.Replace(doc, `"title": "g",`, `"title": "g", "shell": "/bin/sh",`, 1)
		}},
		{"entrypoint nested in a task", func(doc string) string {
			return strings.Replace(doc, `"successCriteria": "sc"`, `"successCriteria": "sc", "entrypoint": "main.sh"`, 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.mutate(validBaseDocument)
			_, report, err := Parse([]byte(doc))
			if err == nil {
				t.Fatalf("Parse() error = nil, want rejection for an executable-string member; doc=%s", doc)
			}
			mustErrorCode(t, report, "executable-string-member")
		})
	}
}

// --- AC: ai-goal-part-without-budgets-is-rejected ---

func TestParse_AIGoalPartWithoutBudgetsIsRejected(t *testing.T) {
	t.Run("absent budgets", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument, `"budgets": {"maxSteps": 5}`, `"budgets": {}`, 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for empty budgets")
		}
		issue := mustErrorCode(t, report, "ai-goal-budgets-required")
		if issue.Pointer != "/parts/0/goal/budgets" {
			t.Errorf("issue.Pointer = %q, want /parts/0/goal/budgets", issue.Pointer)
		}
	})

	t.Run("every bound zero", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument, `"budgets": {"maxSteps": 5}`, `"budgets": {"maxSteps": 0, "maxDurationSeconds": 0}`, 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection when every budget bound is zero")
		}
		mustErrorCode(t, report, "ai-goal-budgets-required")
	})

	t.Run("one positive bound is accepted", func(t *testing.T) {
		for _, budgets := range []string{
			`{"maxSteps": 1}`,
			`{"maxDurationSeconds": 1}`,
			`{"maxCost": 0.01}`,
		} {
			doc := strings.Replace(validBaseDocument, `"budgets": {"maxSteps": 5}`, `"budgets": `+budgets, 1)
			_, report, err := Parse([]byte(doc))
			if err != nil {
				t.Fatalf("Parse() error = %v for budgets %s, want acceptance; report=%+v", err, budgets, report)
			}
		}
	})
}

// --- AC: parsing-resolves-nothing-and-starts-nothing ---

func TestParse_ResolvesNothingAndStartsNothing(t *testing.T) {
	// A document declaring an env-backed secret, a credential-backed
	// secret, an exampleBot, an HTTP bot URL (well-formed but pointing
	// nowhere reachable) and a cassette provider referencing a file that
	// does not exist on disk. Parse must succeed (or fail on structural
	// grounds only) without ever touching any of them.
	doc := `{
  "format": "https://chatwright.dev/formats/scenario-document/v1",
  "schemaVersion": 1,
  "id": "t", "version": "v1", "title": "t",
  "requires": ["ai-goal"],
  "fidelity": {"endpointProfile": "platform-emulated", "environment": "dev", "dataSensitivity": "synthetic"},
  "platform": "telegram",
  "chats": [{"id": "main", "platformChatId": 42}],
  "bot": {"id": "b", "name": "B", "transport": "http", "delivery": "webhook", "url": "https://nonexistent.invalid.example/webhook",
          "headers": {"Authorization": {"secretRef": "authHeader"}}},
  "secrets": [
    {"name": "authHeader", "from": {"env": "CHATWRIGHT_TEST_NEVER_SET_XYZ"}},
    {"name": "credSecret", "from": {"credential": "never-looked-up"}}
  ],
  "cast": [
    {
      "id": "arena", "type": "ai-agent", "name": "Arena",
      "platformIdentity": {"userId": 7, "firstName": "Arena"},
      "provider": {"kind": "cassette", "mode": "replay", "cassette": "does/not/exist/on/disk.json"}
    }
  ],
  "parts": [
    {
      "id": "p1", "kind": "ai-goal", "chat": "main", "actorId": "arena",
      "goal": {"id": "g1", "title": "g", "tasks": [{"id": "t1", "successCriteria": "sc"}], "budgets": {"maxSteps": 5}}
    }
  ]
}`

	// Parse's signature takes only the document bytes — it has no
	// SecretResolver, no filesystem root and no network client to call
	// through, so there is no code path inside it that could read an env
	// var, consult a credential store, start a bot or read a cassette
	// file. This test proves that by construction rather than by spying:
	// CHATWRIGHT_TEST_NEVER_SET_XYZ is (deliberately) never set in this
	// process, "does/not/exist/on/disk.json" does not exist, and
	// "nonexistent.invalid.example" resolves nowhere — if Parse touched
	// any of them, it would surface as an error here; it does not.
	if v, ok := os.LookupEnv("CHATWRIGHT_TEST_NEVER_SET_XYZ"); ok {
		t.Fatalf("test precondition violated: CHATWRIGHT_TEST_NEVER_SET_XYZ is set to %q in this environment", v)
	}

	parsed, report, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v, report=%+v — parsing must succeed without resolving anything", err, report)
	}
	if parsed == nil {
		t.Fatal("Parse() returned a nil Document for a structurally valid document")
	}
	if len(report.Warnings()) == 0 {
		t.Error("report.Warnings() is empty, want at least noRunCeiling/no-independent-verification — sanity check that validate actually ran")
	}
	// The referenced cassette file does not exist on disk, the webhook
	// host does not resolve and the declared env var/credential are never
	// set/configured; Parse succeeding at all (rather than failing with a
	// file-not-found, DNS or credential-store error) is itself the proof
	// that Parse never attempted to resolve any of them. Build — a
	// deliberately separate, later step — is where that resolution
	// happens; see build_test.go.
}
