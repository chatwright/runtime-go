package scenario

import (
	"strings"
	"testing"
)

// --- AC: unsupported-capability-is-named-not-dropped ---

func TestParse_UnsupportedCapabilityIsNamedNotDropped(t *testing.T) {
	t.Run("deterministic part", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument, `"requires": ["ai-goal", "exampleBot:greetbot"],`, `"requires": ["ai-goal", "exampleBot:greetbot", "deterministic"],`, 1)
		doc = strings.Replace(doc, `"id": "p1", "kind": "ai-goal", "chat": "main", "actorId": "arena",`,
			`"id": "p1", "kind": "deterministic", "chat": "main", "actorId": "arena", "steps": [],`, 1)

		parsed, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a deterministic part")
		}
		if parsed != nil {
			t.Fatal("Parse() returned a non-nil Document for a rejected document")
		}
		var named bool
		for _, issue := range report.Errors() {
			if issue.Code == "unsupported-capability" && strings.Contains(issue.Message, "deterministic-steps") {
				named = true
			}
		}
		if !named {
			t.Errorf("no unsupported-capability issue names \"deterministic-steps\"; errors = %+v", report.Errors())
		}
	})

	t.Run("two chats", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument, `"chats": [{"id": "main", "platformChatId": 42}],`,
			`"chats": [{"id": "main", "platformChatId": 42}, {"id": "second", "platformChatId": 43}],`, 1)

		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for two declared chats")
		}
		issue := mustErrorCode(t, report, "unsupported-capability")
		if !strings.Contains(issue.Message, "multi-chat") {
			t.Errorf("issue.Message = %q, want it to name \"multi-chat\"", issue.Message)
		}
	})

	t.Run("unrecognised requires entry", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument, `"requires": ["ai-goal", "exampleBot:greetbot"],`, `"requires": ["ai-goal", "exampleBot:greetbot", "hybrid"],`, 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for an unrecognised capability")
		}
		issue := mustErrorCode(t, report, "unsupported-capability")
		if !strings.Contains(issue.Message, `"hybrid"`) {
			t.Errorf("issue.Message = %q, want it to name \"hybrid\"", issue.Message)
		}
	})

	t.Run("unrecognised schema version", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument, `"schemaVersion": 1,`, `"schemaVersion": 2,`, 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for schemaVersion 2")
		}
		mustErrorCode(t, report, "unsupported-schema-version")
	})

	t.Run("unrecognised format", func(t *testing.T) {
		doc := strings.Replace(validBaseDocument, `"format": "https://chatwright.dev/formats/scenario-document/v1",`, `"format": "https://chatwright.dev/formats/scenario-document/v99",`, 1)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for an unrecognised format")
		}
		mustErrorCode(t, report, "unsupported-format")
	})
}

// --- AC: unsupported-transport-is-refused-by-name (runtime-go half: iframe) ---

func TestParse_IframeTransportIsRefusedByName(t *testing.T) {
	doc := strings.Replace(validBaseDocument, `"exampleBot": "greetbot"`, `"transport": "iframe", "url": "https://bot.example.com/embed"`, 1)
	doc = strings.Replace(doc, `"requires": ["ai-goal", "exampleBot:greetbot"],`, `"requires": ["ai-goal"],`, 1)

	_, report, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() error = nil, want rejection for transport:\"iframe\"")
	}
	issue := mustErrorCode(t, report, "unsupported-transport")
	if !strings.Contains(issue.Message, "iframe") {
		t.Errorf("issue.Message = %q, want it to name \"iframe\"", issue.Message)
	}
}

// A valid http+webhook document (the transport runtime-go DOES support) is
// accepted, proving the iframe rejection above is transport-specific, not a
// blanket refusal of every non-exampleBot bot.
func TestParse_HTTPWebhookTransportIsAccepted(t *testing.T) {
	doc := strings.Replace(validBaseDocument, `"exampleBot": "greetbot"`, `"transport": "http", "delivery": "webhook", "url": "https://bot.example.com/webhook"`, 1)
	doc = strings.Replace(doc, `"requires": ["ai-goal", "exampleBot:greetbot"],`, `"requires": ["ai-goal"],`, 1)
	_, report, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v, want acceptance for a valid http+webhook document; report=%+v", err, report)
	}
}

// --- AC: environment-is-declared-and-never-guessed ---

func TestResolveFidelity_EnvironmentNeverGuessed(t *testing.T) {
	t.Run("unrecognised host resolves to unknown, never dev or production", func(t *testing.T) {
		doc := &Document{Bot: Bot{URL: "https://real-bot.example.com/webhook"}, Fidelity: Fidelity{}}
		got := ResolveFidelity(doc)
		if got.Environment != EnvironmentUnknown {
			t.Errorf("Environment = %q, want %q", got.Environment, EnvironmentUnknown)
		}
	})

	t.Run("localhost resolves to dev", func(t *testing.T) {
		doc := &Document{Bot: Bot{URL: "http://localhost:8080/webhook"}, Fidelity: Fidelity{}}
		got := ResolveFidelity(doc)
		if got.Environment != EnvironmentDev {
			t.Errorf("Environment = %q, want %q", got.Environment, EnvironmentDev)
		}
	})

	t.Run("declared environment always wins over the heuristic", func(t *testing.T) {
		doc := &Document{Bot: Bot{URL: "http://localhost:8080/webhook"}, Fidelity: Fidelity{Environment: EnvironmentProduction}}
		got := ResolveFidelity(doc)
		if got.Environment != EnvironmentProduction {
			t.Errorf("Environment = %q, want %q (declared value must win)", got.Environment, EnvironmentProduction)
		}
	})

	t.Run("production defaults sensitivity to real-subject", func(t *testing.T) {
		doc := &Document{Fidelity: Fidelity{Environment: EnvironmentProduction}}
		got := ResolveFidelity(doc)
		if got.DataSensitivity != DataSensitivityRealSubject {
			t.Errorf("DataSensitivity = %q, want %q", got.DataSensitivity, DataSensitivityRealSubject)
		}
	})

	t.Run("real-subject declared on a test endpoint is kept, not downgraded", func(t *testing.T) {
		doc := &Document{Fidelity: Fidelity{Environment: EnvironmentTest, DataSensitivity: DataSensitivityRealSubject}}
		got := ResolveFidelity(doc)
		if got.DataSensitivity != DataSensitivityRealSubject {
			t.Errorf("DataSensitivity = %q, want %q (must not be silently downgraded)", got.DataSensitivity, DataSensitivityRealSubject)
		}
	})

	t.Run("non-production defaults sensitivity to synthetic", func(t *testing.T) {
		doc := &Document{Fidelity: Fidelity{Environment: EnvironmentDev}}
		got := ResolveFidelity(doc)
		if got.DataSensitivity != DataSensitivitySynthetic {
			t.Errorf("DataSensitivity = %q, want %q", got.DataSensitivity, DataSensitivitySynthetic)
		}
	})
}

func TestParse_RealSubjectWithoutRedactionPolicyIsRejected(t *testing.T) {
	doc := strings.Replace(validBaseDocument,
		`"fidelity": {"endpointProfile": "platform-emulated", "environment": "dev", "dataSensitivity": "synthetic"},`,
		`"fidelity": {"endpointProfile": "platform-emulated", "environment": "test", "dataSensitivity": "real-subject"},`, 1)

	_, report, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() error = nil, want rejection for real-subject with no redactionPolicy declared")
	}
	mustErrorCode(t, report, "redaction-policy-required")

	// Declaring a redactionPolicy alongside it is accepted.
	docWithPolicy := strings.Replace(doc, `"dataSensitivity": "real-subject"},`, `"dataSensitivity": "real-subject", "redactionPolicy": "standard-pii-v1"},`, 1)
	_, report2, err2 := Parse([]byte(docWithPolicy))
	if err2 != nil {
		t.Fatalf("Parse() error = %v, want acceptance once redactionPolicy is declared; report=%+v", err2, report2)
	}
}

// --- noRunCeiling / noIndependentVerification are reported, never rejected ---

func TestParse_AbsentCeilingAndVerifyAreWarningsNotErrors(t *testing.T) {
	_, report, err := Parse([]byte(validBaseDocument))
	if err != nil {
		t.Fatalf("Parse() error = %v, want acceptance (no ceiling/verify is valid, just reported); report=%+v", err, report)
	}
	var sawCeiling, sawVerify bool
	for _, w := range report.Warnings() {
		if w.Code == "no-run-ceiling" {
			sawCeiling = true
		}
		if w.Code == "no-independent-verification" {
			sawVerify = true
		}
	}
	if !sawCeiling {
		t.Error("report.Warnings() missing no-run-ceiling")
	}
	if !sawVerify {
		t.Error("report.Warnings() missing no-independent-verification")
	}
}

// TestParse_UnknownMemberIsRejected guards against a typo'd or made-up
// member silently decoding to nothing — the format's own "no member is
// silently ignored" rule applies to an unrecognised field, not only to a
// whole unsupported capability (found in review of #12).
func TestParse_UnknownMemberIsRejected(t *testing.T) {
	doc := strings.Replace(validBaseDocument, `"title": "t",`, `"title": "t", "titel": "typo",`, 1)
	_, report, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("Parse() error = nil, want rejection for an unrecognised member")
	}
	issue := mustErrorCode(t, report, "unknown-member")
	if !strings.Contains(issue.Message, `"titel"`) {
		t.Errorf("issue.Message = %q, want it to name the unrecognised member %q", issue.Message, "titel")
	}
}

// TestParse_VerifyConditionValueShapeIsChecked guards Condition.Value shape
// checking beyond the regex op — an exact/contains value that is neither a
// string nor a string array, and a non-boolean value on the "edited"
// field, are both rejected at Parse time rather than surfacing later as a
// plain Go error out of CompileVerify (found in review of #12).
func TestParse_VerifyConditionValueShapeIsChecked(t *testing.T) {
	withVerify := func(condition string) string {
		doc := strings.TrimSuffix(validBaseDocument, "\n}")
		return doc + `,
  "verify": {"chat": "main", "metDetail": "ok", "journal": [
    {"id": "e1", "unmetDetail": "no", "all": [` + condition + `]}
  ]}
}`
	}

	t.Run("exact value must be string or string array", func(t *testing.T) {
		doc := withVerify(`{"field": "text", "op": "exact", "value": 42}`)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a numeric exact value")
		}
		mustErrorCode(t, report, "invalid-shape")
	})

	t.Run("edited value must be boolean", func(t *testing.T) {
		doc := withVerify(`{"field": "edited", "op": "exact", "value": "true"}`)
		_, report, err := Parse([]byte(doc))
		if err == nil {
			t.Fatal("Parse() error = nil, want rejection for a string value on the boolean \"edited\" field")
		}
		mustErrorCode(t, report, "invalid-shape")
	})

	t.Run("valid conditions are accepted", func(t *testing.T) {
		doc := withVerify(`{"field": "text", "op": "exact", "value": ["a", "b"]}`)
		_, report, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("Parse() error = %v, want acceptance for a string-array exact value; report=%+v", err, report)
		}
	})
}
