package scenario

import (
	"fmt"
	"regexp"
	"strings"

	"chatwright.dev/runtime/goal"
)

// DefaultSupportedExampleBots is the exampleBot ids this runtime (runtime-go)
// ships and Validate accepts a `requires: ["exampleBot:<id>"]` declaration
// for — see ExampleBotRegistry, which must register exactly this set (an
// id present here with no matching registry entry, or vice versa, is a
// packaging bug, not a document problem).
var DefaultSupportedExampleBots = map[string]bool{
	"greetbot": true,
}

// validate runs every structural rule the format's README describes over
// doc, appending an Issue to report for each violation. It performs no I/O:
// every check is over doc's already-decoded fields.
func validate(doc *Document, report *Report) {
	validateFormatAndVersion(doc, report)
	validateRequires(doc, report)
	validateFidelity(doc, report)
	validatePlatform(doc, report)
	chatIDs := validateChats(doc, report)
	validateBot(doc, report)
	castByID := validateCast(doc, report)
	validateParts(doc, report, chatIDs, castByID)
	validateCeiling(doc, report)
	validateVerify(doc, report, chatIDs)
}

func validateFormatAndVersion(doc *Document, report *Report) {
	if doc.Format != FormatV1 {
		report.add(errorIssue("unsupported-format", "/format",
			fmt.Sprintf("unsupported format %q; this runtime supports %q", doc.Format, FormatV1)))
	}
	if doc.SchemaVersion != SchemaVersion1 {
		report.add(errorIssue("unsupported-schema-version", "/schemaVersion",
			fmt.Sprintf("unsupported schemaVersion %d; this runtime supports %d", doc.SchemaVersion, SchemaVersion1)))
	}
	if doc.ID == "" {
		report.add(errorIssue("invalid-shape", "/id", "id is required"))
	}
	if doc.Version == "" {
		report.add(errorIssue("invalid-shape", "/version", "version is required"))
	}
	if doc.Title == "" {
		report.add(errorIssue("invalid-shape", "/title", "title is required"))
	}
}

// validateRequires checks every Document.Requires entry against this
// runtime's supported capability vocabulary — "ai-goal" and
// "exampleBot:<id>" for id in DefaultSupportedExampleBots — rejecting an
// unrecognised entry by naming it explicitly (the format's "unsupported
// capability ... is named, and the original document is retained" rule).
// Capabilities this format reserves but does not support in v1
// ("deterministic", "multi-chat", "hybrid", ...) are rejected here the same
// way as any other unrecognised string.
func validateRequires(doc *Document, report *Report) {
	for i, r := range doc.Requires {
		pointer := fmt.Sprintf("/requires/%d", i)
		if r == "ai-goal" {
			continue
		}
		if name, ok := strings.CutPrefix(r, "exampleBot:"); ok {
			if DefaultSupportedExampleBots[name] {
				continue
			}
			report.add(errorIssue("unsupported-capability", pointer, fmt.Sprintf("unsupported capability %q: this runtime ships no example bot named %q", r, name)))
			continue
		}
		report.add(errorIssue("unsupported-capability", pointer, fmt.Sprintf("unsupported capability %q", r)))
	}
}

func validatePlatform(doc *Document, report *Report) {
	if doc.Platform != "telegram" {
		report.add(errorIssue("unsupported-capability", "/platform", fmt.Sprintf("unsupported platform %q; this runtime supports \"telegram\"", doc.Platform)))
	}
}

// validateChats enforces "exactly one chat in v1" (more than one is
// rejected as unsupported-capability: multi-chat, per the README) and
// unique chat ids, returning the set of declared doc-local chat ids for
// later reference-resolution checks.
func validateChats(doc *Document, report *Report) map[string]bool {
	ids := map[string]bool{}
	switch {
	case len(doc.Chats) == 0:
		report.add(errorIssue("invalid-shape", "/chats", "at least one chat is required"))
	case len(doc.Chats) > 1:
		report.add(errorIssue("unsupported-capability", "/chats", "unsupported capability \"multi-chat\": exactly one chat is supported in v1"))
	}
	for i, c := range doc.Chats {
		pointer := fmt.Sprintf("/chats/%d", i)
		if c.ID == "" {
			report.add(errorIssue("invalid-shape", pointer+"/id", "chat id is required"))
			continue
		}
		if ids[c.ID] {
			report.add(errorIssue("duplicate-id", pointer+"/id", "duplicate chat id"))
		}
		ids[c.ID] = true
	}
	return ids
}

// validateBot enforces the README's "The bot endpoint" rules: exactly one
// of URL or ExampleBot, transport/delivery/URL combinations, the iframe
// transport being unsupported by this runtime, and that using an
// exampleBot is matched by a `requires: ["exampleBot:<id>"]` declaration.
func validateBot(doc *Document, report *Report) {
	b := doc.Bot
	if b.ID == "" {
		report.add(errorIssue("invalid-shape", "/bot/id", "bot id is required"))
	}
	if b.Name == "" {
		report.add(errorIssue("invalid-shape", "/bot/name", "bot name is required"))
	}

	hasURL := b.URL != ""
	hasExampleBot := b.ExampleBot != ""
	switch {
	case hasURL == hasExampleBot:
		report.add(errorIssue("invalid-shape", "/bot", "exactly one of \"url\" or \"exampleBot\" is required"))
		return
	case hasExampleBot:
		if b.Transport != "" {
			report.add(errorIssue("invalid-shape", "/bot/transport", "transport is forbidden with exampleBot"))
		}
		if b.Delivery != "" {
			report.add(errorIssue("invalid-shape", "/bot/delivery", "delivery is forbidden with exampleBot"))
		}
		required := "exampleBot:" + b.ExampleBot
		if !containsString(doc.Requires, required) {
			report.add(errorIssue("invalid-shape", "/requires", fmt.Sprintf("using exampleBot %q requires declaring %q in \"requires\"", b.ExampleBot, required)))
		}
		return
	}

	// hasURL: transport is required, and this runtime refuses "iframe" by
	// name (see the README's unsupported-transport-is-refused-by-name AC:
	// "the same refusal shape applies to runtime-go given transport:
	// iframe").
	switch b.Transport {
	case "":
		report.add(errorIssue("invalid-shape", "/bot/transport", "transport is required with url"))
	case TransportIframe:
		report.add(errorIssue("unsupported-transport", "/bot/transport", "unsupported transport \"iframe\": runtime-go has no iframe host (N/A by design — see docs/runtime-parity.md)"))
	case TransportHTTP:
		switch b.Delivery {
		case DeliveryWebhook:
			// url required, already true here (hasURL).
		case DeliveryPolling:
			report.add(errorIssue("invalid-shape", "/bot/url", "url is forbidden for http+polling delivery: a polling bot needs no inbound address"))
		default:
			report.add(errorIssue("invalid-shape", "/bot/delivery", "delivery is required with http transport (\"webhook\" or \"polling\")"))
		}
	default:
		report.add(errorIssue("invalid-shape", "/bot/transport", fmt.Sprintf("unrecognised transport %q", b.Transport)))
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// validateCast enforces cast-member id uniqueness, type/provider
// consistency and provider-kind shape (the README's "The cast and its
// providers" section), returning a lookup from cast id to its Type for
// validateParts' actorId/goal-actor checks.
func validateCast(doc *Document, report *Report) map[string]string {
	byID := map[string]string{}
	if len(doc.Cast) == 0 {
		report.add(errorIssue("invalid-shape", "/cast", "at least one cast member is required"))
	}
	for i, c := range doc.Cast {
		pointer := fmt.Sprintf("/cast/%d", i)
		if c.ID == "" {
			report.add(errorIssue("invalid-shape", pointer+"/id", "cast id is required"))
		} else {
			if _, dup := byID[c.ID]; dup {
				report.add(errorIssue("duplicate-id", pointer+"/id", "duplicate cast id"))
			}
			byID[c.ID] = c.Type
		}

		switch c.Type {
		case CastTypeAIAgent:
			if c.Provider == nil {
				report.add(errorIssue("invalid-shape", pointer+"/provider", "an ai-agent cast member requires a provider"))
			} else {
				validateProvider(*c.Provider, pointer+"/provider", report)
			}
		case CastTypeHuman:
			if c.Provider != nil {
				report.add(errorIssue("invalid-shape", pointer+"/provider", "a human cast member declares no provider"))
			}
		default:
			report.add(errorIssue("invalid-shape", pointer+"/type", fmt.Sprintf("unrecognised cast type %q", c.Type)))
		}
	}
	return byID
}

func validateProvider(p Provider, pointer string, report *Report) {
	switch p.Kind {
	case ProviderKindModel:
		if p.Model == "" {
			report.add(errorIssue("invalid-shape", pointer+"/model", "model is required for a model provider"))
		}
		if p.ProviderID == "" {
			report.add(errorIssue("invalid-shape", pointer+"/providerId", "providerId is required for a model provider"))
		}
	case ProviderKindCassette:
		if p.Cassette == "" {
			report.add(errorIssue("invalid-shape", pointer+"/cassette", "cassette is required for a cassette provider"))
		}
		switch p.Mode {
		case CassetteModeReplay:
			if p.Wraps != nil {
				report.add(errorIssue("invalid-shape", pointer+"/wraps", "wraps is forbidden in replay mode"))
			}
		case CassetteModeRecord:
			if p.Wraps == nil {
				report.add(errorIssue("invalid-shape", pointer+"/wraps", "record mode requires wraps (a model provider)"))
			} else if p.Wraps.Kind != ProviderKindModel {
				report.add(errorIssue("invalid-shape", pointer+"/wraps/kind", "wraps must be a model provider"))
			} else {
				validateProvider(*p.Wraps, pointer+"/wraps", report)
			}
		default:
			report.add(errorIssue("invalid-shape", pointer+"/mode", fmt.Sprintf("unrecognised cassette mode %q", p.Mode)))
		}
	default:
		report.add(errorIssue("invalid-shape", pointer+"/kind", fmt.Sprintf("unrecognised provider kind %q", p.Kind)))
	}
}

// validateParts enforces part-id uniqueness, chat/actorId reference
// resolution, the deterministic-part-is-reserved rule, and delegates
// ai-goal budget/goal validation to validateGoalPart.
func validateParts(doc *Document, report *Report, chatIDs map[string]bool, castByID map[string]string) {
	if len(doc.Parts) == 0 {
		report.add(errorIssue("invalid-shape", "/parts", "at least one part is required"))
	}
	seen := map[string]bool{}
	for i, p := range doc.Parts {
		pointer := fmt.Sprintf("/parts/%d", i)
		if p.ID == "" {
			report.add(errorIssue("invalid-shape", pointer+"/id", "part id is required"))
		} else {
			if seen[p.ID] {
				report.add(errorIssue("duplicate-id", pointer+"/id", "duplicate part id"))
			}
			seen[p.ID] = true
		}

		if p.Chat == "" {
			report.add(errorIssue("invalid-shape", pointer+"/chat", "chat is required"))
		} else if !chatIDs[p.Chat] {
			report.add(errorIssue("chat-ref-unresolved", pointer+"/chat", fmt.Sprintf("chat %q does not resolve to a declared chats[].id", p.Chat)))
		}

		switch p.FailurePolicy {
		case "", FailurePolicyAbort, FailurePolicyCoverageGap:
		default:
			report.add(errorIssue("invalid-shape", pointer+"/failurePolicy", fmt.Sprintf("unrecognised failurePolicy %q", p.FailurePolicy)))
		}

		switch p.Kind {
		case PartKindAIGoal:
			validateGoalPart(p, pointer, report, castByID)
		case PartKindDeterministic:
			report.add(errorIssue("unsupported-capability", pointer+"/kind", "unsupported capability \"deterministic-steps\": deterministic parts are reserved for action matchers and not built"))
		default:
			report.add(errorIssue("invalid-shape", pointer+"/kind", fmt.Sprintf("unrecognised part kind %q", p.Kind)))
		}
	}
}

func validateGoalPart(p Part, pointer string, report *Report, castByID map[string]string) {
	if p.ActorID == "" {
		report.add(errorIssue("invalid-shape", pointer+"/actorId", "actorId is required"))
	} else if actorType, ok := castByID[p.ActorID]; !ok {
		report.add(errorIssue("actor-ref-unresolved", pointer+"/actorId", fmt.Sprintf("actorId %q does not resolve to a declared cast[].id", p.ActorID)))
	} else if actorType != CastTypeAIAgent {
		report.add(errorIssue("invalid-shape", pointer+"/actorId", fmt.Sprintf("actorId %q resolves to a %q cast member, not an ai-agent", p.ActorID, actorType)))
	}

	if p.Goal == nil {
		report.add(errorIssue("invalid-shape", pointer+"/goal", "an ai-goal part requires goal"))
		return
	}

	if !hasPositiveBudget(p.Goal.Budgets) {
		report.add(errorIssue("ai-goal-budgets-required", pointer+"/goal/budgets",
			"an ai-goal part's budgets must declare at least one positive bound (maxSteps, maxDurationSeconds or maxCost)"))
	}
	if p.Goal.Budgets.MaxDurationSeconds < 0 {
		report.add(errorIssue("invalid-shape", pointer+"/goal/budgets/maxDurationSeconds", "must not be negative"))
	}
	if p.Goal.Budgets.MaxSteps < 0 {
		report.add(errorIssue("invalid-shape", pointer+"/goal/budgets/maxSteps", "must not be negative"))
	}
	if p.Goal.Budgets.MaxCost != nil && *p.Goal.Budgets.MaxCost <= 0 {
		report.add(errorIssue("invalid-shape", pointer+"/goal/budgets/maxCost", "must be positive when set"))
	}

	if g, err := toGoalGoal(*p.Goal); err != nil {
		report.add(errorIssue("invalid-shape", pointer+"/goal", err.Error()))
	} else if err := g.Validate(); err != nil {
		report.add(errorIssue("invalid-shape", pointer+"/goal", err.Error()))
	}
}

// hasPositiveBudget reports whether b declares at least one positive bound
// — the ai-goal-part-without-budgets-is-rejected acceptance criterion.
func hasPositiveBudget(b Budgets) bool {
	return b.MaxSteps > 0 || b.MaxDurationSeconds > 0 || (b.MaxCost != nil && *b.MaxCost > 0)
}

// toGoalGoal converts an authored Goal to goal.Goal, so this package's own
// Goal.Validate reuses goal.Goal.Validate (dependency-cycle detection,
// duplicate-task-id detection) rather than re-implementing it.
func toGoalGoal(g Goal) (goal.Goal, error) {
	tasks := make([]goal.Task, len(g.Tasks))
	for i, t := range g.Tasks {
		tasks[i] = goal.Task{
			ID: t.ID, Title: t.Title, DependsOn: t.DependsOn,
			SuccessCriteria: t.SuccessCriteria, Milestones: t.Milestones,
		}
	}
	return goal.Goal{
		ID: g.ID, Title: g.Title, Description: g.Description,
		Tasks: tasks, Constraints: g.Constraints,
		Budgets: goal.Budgets{
			MaxSteps:    g.Budgets.MaxSteps,
			MaxDuration: secondsToDuration(g.Budgets.MaxDurationSeconds),
			MaxCost:     g.Budgets.MaxCost,
		},
	}, nil
}

func validateCeiling(doc *Document, report *Report) {
	if doc.Ceiling == nil {
		report.add(warningIssue("no-run-ceiling", "/ceiling", "no run-level ceiling is declared; only each part's own budgets bind"))
		return
	}
	c := doc.Ceiling
	if c.MaxSteps < 0 {
		report.add(errorIssue("invalid-shape", "/ceiling/maxSteps", "must not be negative"))
	}
	if c.MaxDurationSeconds < 0 {
		report.add(errorIssue("invalid-shape", "/ceiling/maxDurationSeconds", "must not be negative"))
	}
	if c.MaxCost != nil && *c.MaxCost <= 0 {
		report.add(errorIssue("invalid-shape", "/ceiling/maxCost", "must be positive when set"))
	}
}

// validRegexSubset rejects the RE2 ∩ JavaScript subset's known escape
// hatches — backreferences and lookaround — that Go's own RE2-based
// regexp package never supports in the first place (so a successful
// regexp.Compile already rules out most of the subset violation on its
// own); this only needs to catch syntax RE2 accepts that a second runtime's
// engine would not restrict the same way. Go's regexp always rejects
// backreferences and lookaround with a compile error, so in practice this
// package's own compile check (see validateVerify) is the enforcement
// point; lookaroundPattern exists to produce a clearer message than RE2's
// own parse error for the common case.
var lookaroundOrBackreferencePattern = regexp.MustCompile(`\\[1-9]|\(\?[=!<]`)

// validateVerify enforces Verify's shape: chat resolution, expectation-id
// uniqueness, condition field/op vocabulary, contains/regex forbidden on a
// non-string field, and the RE2 ∩ JavaScript regex subset.
func validateVerify(doc *Document, report *Report, chatIDs map[string]bool) {
	if doc.Verify == nil {
		report.add(warningIssue("no-independent-verification", "/verify",
			"no independent journal verification is declared; a successful outcome is judged, never verified"))
		return
	}
	v := doc.Verify
	if v.Chat == "" {
		report.add(errorIssue("invalid-shape", "/verify/chat", "chat is required"))
	} else if !chatIDs[v.Chat] {
		report.add(errorIssue("chat-ref-unresolved", "/verify/chat", fmt.Sprintf("chat %q does not resolve to a declared chats[].id", v.Chat)))
	}

	seen := map[string]bool{}
	for i, exp := range v.Journal {
		pointer := fmt.Sprintf("/verify/journal/%d", i)
		if exp.ID == "" {
			report.add(errorIssue("invalid-shape", pointer+"/id", "expectation id is required"))
		} else {
			if seen[exp.ID] {
				report.add(errorIssue("duplicate-id", pointer+"/id", "duplicate expectation id"))
			}
			seen[exp.ID] = true
		}
		if len(exp.All) == 0 {
			report.add(errorIssue("invalid-shape", pointer+"/all", "at least one condition is required"))
		}
		for j, cond := range exp.All {
			validateCondition(cond, fmt.Sprintf("%s/all/%d", pointer, j), report)
		}
	}
}

func validateCondition(c Condition, pointer string, report *Report) {
	switch c.Field {
	case FieldKind, FieldDirection, FieldText, FieldEdited:
	default:
		report.add(errorIssue("invalid-shape", pointer+"/field", fmt.Sprintf("unrecognised field %q", c.Field)))
	}

	switch c.Op {
	case OpExact:
	case OpContains, OpRegex:
		if c.Field == FieldEdited {
			report.add(errorIssue("invalid-shape", pointer+"/op", fmt.Sprintf("op %q is not permitted on boolean field %q", c.Op, c.Field)))
		}
	default:
		report.add(errorIssue("invalid-shape", pointer+"/op", fmt.Sprintf("unrecognised op %q", c.Op)))
	}

	if c.Op == OpRegex {
		var pattern string
		if err := jsonUnmarshalString(c.Value, &pattern); err != nil {
			report.add(errorIssue("invalid-shape", pointer+"/value", "regex op requires a single string pattern"))
		} else if lookaroundOrBackreferencePattern.MatchString(pattern) {
			report.add(errorIssue("regex-unsupported-subset", pointer+"/value", "pattern uses backreferences or lookaround, outside the RE2 ∩ JavaScript subset this format supports"))
		} else if _, err := regexp.Compile(pattern); err != nil {
			report.add(errorIssue("regex-unsupported-subset", pointer+"/value", "pattern does not compile as RE2"))
		}
	}
}
