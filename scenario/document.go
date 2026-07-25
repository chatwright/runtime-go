// Package scenario is the parser, validator and run.Run mapper for
// Chatwright's self-contained scenario document format
// (https://chatwright.dev/formats/scenario-document/v1) — the format
// spec/features/chatwright/scenario-authoring/portable-scenario-documents/
// self-contained-scenario-documents/README.md in the standard repository
// (chatwright/chatwright) defines. A document declares a bot endpoint, an
// AI goal with its tasks and budgets, the cast, the declared fidelity and
// an independent journal verification — everything arena.GreetbotScenario()
// carries compiled into Go, expressed instead as one committed,
// language-neutral JSON file that both chatwright runtimes execute to the
// same verdict with no Go or TypeScript written.
//
// Three stages, kept deliberately separate:
//
//  1. Parse — pure, in-memory, no I/O of any kind. It never starts a bot,
//     reads an environment variable, resolves a credential, opens a file
//     beyond the document bytes it was given, or makes a network call (the
//     spec's "parsing is inert, and refusal is explicit"). Parse returns a
//     Report naming every structural problem by JSON pointer and rule id,
//     and rejects (returns a non-nil error) the moment any Report issue is
//     an error-severity one — never echoing the offending value.
//  2. Resolve — a ScenarioProvider turns a reference (a file path today; a
//     store id or a Cloud reference tomorrow) into document bytes, then
//     Parse validates them. The runner (Build, run.Run) never learns where
//     a Document came from.
//  3. Build — the separate, explicit step that actually starts anything: it
//     resolves declared secrets (env/credential lookup), boots an example
//     bot or wires an HTTP bot transport, and produces a ready-to-execute
//     run.Run plus the roster and doc-local chat-id mapping a caller needs
//     to assemble a run bundle afterwards. A document that failed
//     validation never reaches Build.
package scenario

import "encoding/json"

// FormatV1 is the scenario-document format identifier this package parses.
// Parse rejects any other value for Document.Format — see the format's own
// "unsupported format ... is named" validation rule.
const FormatV1 = "https://chatwright.dev/formats/scenario-document/v1"

// SchemaVersion1 is the only schemaVersion this package accepts. A document
// declaring any other value is rejected naming it explicitly, never
// silently downgraded or upgraded.
const SchemaVersion1 = 1

// Document is the parsed, validated shape of one scenario-document/v1 file
// — the Go embodiment of the format the standard repository's
// self-contained-scenario-documents README defines. Field order mirrors
// that README's "Shape" table.
//
// SourceURL and BaseDir are not part of the wire shape (no json tag): they
// are provenance a ScenarioProvider stamps on the Document after Parse
// succeeds, so Build can resolve document-relative references (e.g. a
// cassette path) without Parse itself ever touching the filesystem.
type Document struct {
	Format        string `json:"format"`
	SchemaVersion int    `json:"schemaVersion"`

	ID      string `json:"id"`
	Version string `json:"version"`
	Title   string `json:"title"`

	Description string   `json:"description,omitempty"`
	Requires    []string `json:"requires,omitempty"`

	Fidelity Fidelity `json:"fidelity"`
	Platform string   `json:"platform"`
	Chats    []Chat   `json:"chats"`
	Bot      Bot      `json:"bot"`
	Cast     []Cast   `json:"cast"`

	Secrets []Secret `json:"secrets,omitempty"`

	// Inputs and Cases are declared, but their shape beyond "a named input"
	// and "a named binding of inputs" is not pinned down by the format's
	// worked example or any acceptance criterion — see InputDecl and
	// CaseDecl's own doc comments. Neither is consulted by Build: a
	// document executed directly (no manifest) uses no case and no input
	// binding.
	Inputs []InputDecl `json:"inputs,omitempty"`
	Cases  []CaseDecl  `json:"cases,omitempty"`

	Parts []Part `json:"parts"`

	Ceiling *Ceiling `json:"ceiling,omitempty"`
	Verify  *Verify  `json:"verify,omitempty"`

	Verifies []string `json:"verifies,omitempty"`

	// SourceURL is this Document's canonical source identity — e.g.
	// "file:///abs/path/to/doc.json" for a FileScenarioProvider. Empty for
	// a Document built directly from Parse without going through a
	// ScenarioProvider (e.g. in a unit test).
	SourceURL string `json:"-"`
	// BaseDir is the directory document-relative references (a cassette
	// path today) resolve against — the same directory SourceURL names,
	// for a file-backed Document.
	BaseDir string `json:"-"`
}

// Fidelity declares a document's endpoint profile, environment and data
// sensitivity — see the README's "Declared fidelity and environment"
// section. RedactionPolicy is this package's minimal, provisional answer to
// that section's open question ("where does a document's redaction policy
// live"): a non-empty policy name, required exactly when DataSensitivity is
// "real-subject" (see Validate) and otherwise unused. It is not a redaction
// policy body — the sensitive-data-redaction idea owns that; this field
// only records that a document declares one applies.
type Fidelity struct {
	EndpointProfile string `json:"endpointProfile"`
	Environment     string `json:"environment,omitempty"`
	DataSensitivity string `json:"dataSensitivity,omitempty"`
	RedactionPolicy string `json:"redactionPolicy,omitempty"`
}

// Endpoint profiles — see Fidelity.EndpointProfile.
const (
	EndpointProfilePlatformEmulated = "platform-emulated"
	EndpointProfileHeadlessEngine   = "headless-engine"
)

// Environments — see Fidelity.Environment.
const (
	EnvironmentDev        = "dev"
	EnvironmentTest       = "test"
	EnvironmentProduction = "production"
	EnvironmentUnknown    = "unknown"
)

// Data sensitivities — see Fidelity.DataSensitivity.
const (
	DataSensitivitySynthetic   = "synthetic"
	DataSensitivityRealSubject = "real-subject"
)

// Chat is one declared chat — exactly one in v1 (see Validate). ID is
// document-local: a Part's Chat member, and Verify.Chat, reference it, so
// no part hard-codes a platform chat number.
type Chat struct {
	ID             string `json:"id"`
	PlatformChatID int64  `json:"platformChatId"`
}

// Bot declares how the runtime reaches the bot under test and, at the same
// time, its roster identity — see the README's "The bot endpoint" section.
// Exactly one of URL or ExampleBot is set (see Validate).
type Bot struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Transport is "http" or "iframe" — required with URL, forbidden with
	// ExampleBot (the runtime wires its own example however it likes, and
	// declares no transport in the document).
	Transport string `json:"transport,omitempty"`
	// Delivery is "webhook" or "polling" — http transport only.
	Delivery string `json:"delivery,omitempty"`
	// URL is the bot's webhook URL (http+webhook) or iframe src. Required
	// for iframe and for http+webhook; forbidden for http+polling.
	URL string `json:"url,omitempty"`
	// Headers optionally carries request headers for webhook delivery; each
	// value MUST be a secret reference — see SecretRef.
	Headers map[string]SecretRef `json:"headers,omitempty"`
	// ExampleBot names a bot the runtime itself ships (e.g. "greetbot").
	// Not an extension point — see ExampleBotRegistry.
	ExampleBot string `json:"exampleBot,omitempty"`
}

// Bot transports — see Bot.Transport.
const (
	TransportHTTP   = "http"
	TransportIframe = "iframe"
)

// Bot deliveries — see Bot.Delivery.
const (
	DeliveryWebhook = "webhook"
	DeliveryPolling = "polling"
)

// Cast is one non-bot participant — see the README's "The cast and its
// providers" section.
type Cast struct {
	ID               string           `json:"id"`
	Type             string           `json:"type"`
	Name             string           `json:"name"`
	PlatformIdentity PlatformIdentity `json:"platformIdentity"`
	// Provider is set for an "ai-agent" cast member; nil for a "human" or
	// "scripted"/"replay" one (v1's document format has no way to author a
	// scripted actor — see the README's "scripted is deliberately not
	// expressible").
	Provider *Provider `json:"provider,omitempty"`
}

// Cast types — see Cast.Type. Reused verbatim from the sdk wire vocabulary.
const (
	CastTypeAIAgent = "ai-agent"
	CastTypeHuman   = "human"
)

// PlatformIdentity is one cast member's platform-native identity, serving
// both the roster entry and the user the loop acts as.
type PlatformIdentity struct {
	UserID    int64  `json:"userId"`
	FirstName string `json:"firstName,omitempty"`
}

// Provider is an ai-agent cast member's provider declaration — a
// discriminated union on Kind, encoded as one flat JSON object because
// Go's encoding/json has no native sum-type support. Exactly the fields
// belonging to Kind are meaningful; see Validate for which combinations are
// legal.
type Provider struct {
	Kind string `json:"kind"`

	// Model-kind fields.
	ProviderID string     `json:"providerId,omitempty"`
	Model      string     `json:"model,omitempty"`
	BaseURL    string     `json:"baseUrl,omitempty"`
	APIKey     *SecretRef `json:"apiKey,omitempty"`

	// Cassette-kind fields.
	Mode     string    `json:"mode,omitempty"`
	Cassette string    `json:"cassette,omitempty"`
	Wraps    *Provider `json:"wraps,omitempty"` // mode:"record" only, a model provider
}

// Provider kinds — see Provider.Kind.
const (
	ProviderKindModel    = "model"
	ProviderKindCassette = "cassette"
)

// Cassette provider modes — see Provider.Mode.
const (
	CassetteModeReplay = "replay"
	CassetteModeRecord = "record"
)

// SecretRef is the only legal shape for a secret-bearing field: a JSON
// object carrying exactly one member, "secretRef", naming a Secret declared
// in Document.Secrets. Its UnmarshalJSON (see secrets.go) is deliberately
// strict — a JSON string, or an object with any sibling member, fails to
// decode — so a literal secret in one of these fields is a parse-time
// rejection, not something Validate discovers later.
type SecretRef struct {
	Name string `json:"secretRef"`
}

// Secret declares a name and where the runner resolves it from — never a
// value. See SecretSource.
type Secret struct {
	Name string       `json:"name"`
	From SecretSource `json:"from"`
}

// SecretSource names exactly one of an environment variable or a named
// credential-store entry a Secret resolves from.
type SecretSource struct {
	Env        string `json:"env,omitempty"`
	Credential string `json:"credential,omitempty"`
}

// InputDecl is a document-declared input — see Document.Inputs' own doc
// comment on why this shape is provisional.
type InputDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
}

// CaseDecl is a document-declared named input binding — see Document.Cases'
// own doc comment on why this shape is provisional.
type CaseDecl struct {
	Name   string                     `json:"name"`
	Inputs map[string]json.RawMessage `json:"inputs,omitempty"`
}

// Part is one ordered passage of the run — see the README's "Parts,
// budgets and ceilings" section. Maps onto run.Part/run.AIGoalPartInput
// member for member.
type Part struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Title         string `json:"title,omitempty"`
	Chat          string `json:"chat"`
	ActorID       string `json:"actorId"`
	FailurePolicy string `json:"failurePolicy,omitempty"`

	// Goal is set for kind:"ai-goal".
	Goal *Goal `json:"goal,omitempty"`
	// Loop optionally overrides the actor.Config tunables — see LoopConfig.
	Loop *LoopConfig `json:"loop,omitempty"`

	// Steps is reserved for kind:"deterministic" (action matchers — not
	// built; see the README's "Reserved for action matchers" section). Its
	// mere presence is never inspected beyond "this document declares a
	// deterministic part", which Validate always rejects in v1.
	Steps json.RawMessage `json:"steps,omitempty"`
}

// Part kinds — see Part.Kind.
const (
	PartKindAIGoal        = "ai-goal"
	PartKindDeterministic = "deterministic"
)

// Failure policies — see Part.FailurePolicy. Reused verbatim from
// run.FailurePolicy's own wire vocabulary.
const (
	FailurePolicyAbort        = "abort"
	FailurePolicyCoverageGap  = "coverage-gap"
	defaultFailurePolicyValue = "" // omitted means abort — see run.FailurePolicy.effective.
)

// Goal is goal.Goal, authored — see the README's "Parts, budgets and
// ceilings" section.
type Goal struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Tasks       []Task   `json:"tasks"`
	Constraints []string `json:"constraints,omitempty"`
	Budgets     Budgets  `json:"budgets"`
}

// Task is goal.Task, authored.
type Task struct {
	ID              string   `json:"id"`
	Title           string   `json:"title,omitempty"`
	DependsOn       []string `json:"dependsOn,omitempty"`
	SuccessCriteria string   `json:"successCriteria"`
	Milestones      []string `json:"milestones,omitempty"`
}

// Budgets is goal.Budgets, authored with integer seconds instead of a
// nanosecond duration — see the README's "Durations are integer seconds"
// rule. At least one of MaxSteps, MaxDurationSeconds or MaxCost must be a
// positive value (see Validate's ai-goal-part-without-budgets rule);
// goal.Budgets' own "zero means unlimited" convention is deliberately not
// inherited here.
type Budgets struct {
	MaxSteps           int      `json:"maxSteps,omitempty"`
	MaxDurationSeconds int      `json:"maxDurationSeconds,omitempty"`
	MaxCost            *float64 `json:"maxCost,omitempty"`
}

// LoopConfig is actor.Config's authorable tunables — see the README's
// "loop" paragraph for each field's default, identical to actor.Config's
// own zero-value defaults. RetainObservations and OvershootProbe are
// pointers so "omitted" (nil, defaults to true) is distinguishable from an
// explicit false — actor.Config itself inverts both into
// DisableObservationRetention/DisableOvershootProbe (see toActorConfig in
// build.go).
type LoopConfig struct {
	HistoryWindow         int   `json:"historyWindow,omitempty"`
	NonProgressLimit      int   `json:"nonProgressLimit,omitempty"`
	ActWaitTimeoutSeconds int   `json:"actWaitTimeoutSeconds,omitempty"`
	RetainObservations    *bool `json:"retainObservations,omitempty"`
	OvershootProbe        *bool `json:"overshootProbe,omitempty"`
}

// Ceiling is run.RunCeiling, authored with integer seconds — see Budgets'
// own doc comment on why. Absence is reported by Validate as
// noRunCeiling, never silently accepted as "no limit intended".
type Ceiling struct {
	MaxSteps           int      `json:"maxSteps,omitempty"`
	MaxCost            *float64 `json:"maxCost,omitempty"`
	MaxDurationSeconds int      `json:"maxDurationSeconds,omitempty"`
}

// Verify is the declarative form of arena.Scenario.Verify — see the
// README's "Independent verification of what happened" section, and
// verify.go for its compiled evaluation.
type Verify struct {
	Chat      string               `json:"chat"`
	MetDetail string               `json:"metDetail"`
	Journal   []JournalExpectation `json:"journal"`
}

// JournalExpectation is one ordered journal expectation — see Verify.
type JournalExpectation struct {
	ID          string      `json:"id"`
	UnmetDetail string      `json:"unmetDetail"`
	All         []Condition `json:"all"`
}

// Condition is a {field, op, value} triple with an optional negate — the
// same shape the exploration-to-regression idea fixed for action matchers.
type Condition struct {
	Field  string          `json:"field"`
	Op     string          `json:"op"`
	Value  json.RawMessage `json:"value"`
	Negate bool            `json:"negate,omitempty"`
}

// Condition fields — see Condition.Field.
const (
	FieldKind      = "kind"
	FieldDirection = "direction"
	FieldText      = "text"
	FieldEdited    = "edited"
)

// Condition operators — see Condition.Op.
const (
	OpExact    = "exact"
	OpContains = "contains"
	OpRegex    = "regex"
)
