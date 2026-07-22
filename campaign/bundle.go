package campaign

import (
	"runtime/debug"
	"sort"
	"time"

	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/datastate"
	"github.com/chatwright/chatwright/goal"
	"github.com/chatwright/chatwright/observe"
	"github.com/chatwright/chatwright/platform"
)

// BundleSchemaVersion is the current version of Bundle's JSON shape. Bump it
// whenever Bundle changes in a way a consumer must branch on — see
// ReadBundle, which rejects any other value rather than silently
// reinterpreting an old or newer shape under today's field meanings. It is
// independent of ReportSchemaVersion: Report and Bundle are two separately
// versioned contracts, and Bundle simply carries whichever Report a caller
// gives it.
const BundleSchemaVersion = 1

// EndpointProfilePlatformEmulated is the Metadata.EndpointProfile label for
// a Bundle assembled from a run driven against a platform.Emulator — the
// only endpoint profile this module currently produces (see decision 0008
// and docs/glossary.md's "endpoint profile" entry: "platform-emulated
// (strongest), headless engine, or future profiles"). Metadata.EndpointProfile
// is a plain string, not restricted to this constant, so a future profile
// never requires a Bundle schema change — only a new label.
const EndpointProfilePlatformEmulated = "platform-emulated"

// chatwrightModulePath is this module's own import path — see ModuleVersion.
const chatwrightModulePath = "github.com/chatwright/chatwright"

// Bundle is a campaign run's complete, persisted, self-contained artifact: a
// Web UI player (Chatwright Studio) needs nothing else — no live emulator,
// no database, no network access — to replay what happened and show why the
// campaign.Report concluded what it did. It is the machine-readable run
// bundle campaign.Report's own doc comment names as its eventual seed.
//
// "Self-contained" is deliberate and load-bearing (see docs/glossary.md:
// "evidence is portable and never requires a hosted service to interpret"):
// Bundle carries the Goal a campaign ran, the per-chat platform.JournalEntry
// history a player renders as transcript, the observe.Observation bodies the
// actor actually saw (so a player can show "here is what the actor was
// looking at" without re-deriving it), every actor.LoopEvent the loop
// recorded, the resulting Report, any datastate.Evidence gathered along the
// way, and a Metadata block that names — never implies — the run's fidelity
// (AGENTS.md's "fidelity is declared" principle): its endpoint profile, the
// platform it ran against, and which provider/model ids actually proposed
// actions.
//
// Cassettes (actor.Cassette) are deliberately NOT embedded. A cassette is a
// provider-layer artifact: a reusable, provider-config-keyed record/replay
// log of Provider.Propose calls that can outlive any one campaign (the same
// cassette may back many runs, and a run may use a Provider that was never
// wrapped in a cassette at all). A Bundle is the opposite — one run's actual
// outcome — and everything a player needs to show that outcome (what was
// observed, what was proposed, what happened, what it cost) already lives
// in Events and Observations verbatim. Folding a cassette in would tie two
// independently-lived artifacts together and duplicate content Events
// already carries, for no player-facing benefit.
//
// Field order below is Bundle's stable JSON shape (Go's encoding/json
// preserves struct field declaration order for objects, and sorts map keys
// deterministically for anything encoded as a JSON object) — see WriteBundle
// for the ordering guarantees this gives a round-tripped Bundle, and each
// slice field's own doc comment for the order its elements are stored in.
type Bundle struct {
	// SchemaVersion is always BundleSchemaVersion for a Bundle this package
	// produced.
	SchemaVersion int `json:"schemaVersion"`

	// Goal is the goal definition the campaign ran — verbatim, not
	// converted to plain strings the way Report's fields are, since a
	// player needs the full Goal (task dependencies, constraints, budgets)
	// to render it, not just the outcome Report already summarises.
	Goal goal.Goal `json:"goal"`

	// Chats is every chat the campaign drove, one entry per distinct chat
	// ID, in the order the caller assembled them. A Bundle from this
	// module's current single-chat Loop always has exactly one entry;
	// the shape accommodates a future multi-chat campaign without a schema
	// change.
	Chats []ChatJournal `json:"chats"`

	// Observations is every observe.Observation the campaign's actor.Loop
	// retained (see actor.Config.DisableObservationRetention and
	// actor.Loop.Observations), ordered ascending by Sequence — not the raw
	// map[int64]observe.Observation Loop.Observations returns, so a
	// Bundle's JSON stays chronologically readable regardless of
	// encoding/json's own (string-lexicographic, not numeric) map-key
	// ordering for an integer-keyed map.
	Observations []RetainedObservation `json:"observations"`

	// Events is every actor.LoopEvent the campaign's Loop recorded, in
	// Loop.Events' own order (Index-ascending, across every task the Loop
	// ran).
	Events []actor.LoopEvent `json:"events"`

	// Report is this run's assembled campaign.Report (see Assemble).
	Report Report `json:"report"`

	// Evidence is the datastate.Evidence any data-state assertions produced
	// during the run, in the order they were run. Optional: a campaign with
	// no data-state assertions attached carries none.
	Evidence []datastate.Evidence `json:"evidence,omitempty"`

	// Metadata names this Bundle's fidelity and provenance — see Metadata.
	Metadata Metadata `json:"metadata"`
}

// ChatJournal is one chat's complete structured journal — the same
// platform.JournalEntry records platform.Emulator.Journal returns, carried
// verbatim (including platform-native identifiers) because a Bundle is the
// developer/trace-level artifact platform.JournalEntry's own doc comment
// describes, not the actor-facing observe surface.
type ChatJournal struct {
	ChatID  int64                   `json:"chatId"`
	Entries []platform.JournalEntry `json:"entries"`
}

// RetainedObservation pairs one retained observe.Observation with its own
// Sequence, so Bundle.Observations reads as an ordered list rather than a
// JSON object keyed by a stringified int64 (see Bundle.Observations).
type RetainedObservation struct {
	Sequence    int64               `json:"sequence"`
	Observation observe.Observation `json:"observation"`
}

// Metadata declares a Bundle's provenance and fidelity — AGENTS.md's
// "fidelity is declared" principle applied to the run-bundle artifact: a
// player must be able to say what it is looking at without guessing.
type Metadata struct {
	// CreatedAt is when this Bundle was assembled, supplied by the caller
	// (never time.Now internally — see chatwright's broader injected-clock
	// convention, e.g. goal.NewCampaignState, actor.Config.Now) so
	// assembling a Bundle is itself deterministic and testable.
	CreatedAt time.Time `json:"createdAt"`

	// ChatwrightVersion is the chatwright module's own resolved version —
	// see ModuleVersion — left empty when it cannot be determined (e.g. a
	// `go test` run inside this repository itself, which always reports
	// "(devel)"; see ModuleVersion's doc comment). "If available" is
	// load-bearing: a Bundle is still valid and complete without it.
	ChatwrightVersion string `json:"chatwrightVersion,omitempty"`

	// Platform is the platform name the run drove (e.g. "telegram") — see
	// platform.Platform.Name.
	Platform string `json:"platform"`

	// EndpointProfile is this run's declared endpoint profile (decision
	// 0008; docs/glossary.md's "endpoint profile" entry) — e.g.
	// EndpointProfilePlatformEmulated. Evidence is never interchangeable
	// across profiles, so a player must always have this label, never infer
	// it.
	EndpointProfile string `json:"endpointProfile"`

	// ModelIDs is the aggregated set of actor.Usage.Model ids that actually
	// proposed an action during the run — see AggregateModelIDs, the
	// canonical way to compute it. Empty for a campaign with no AI actor
	// usage (e.g. a fully scripted run whose actor.Usage.Model is always
	// empty).
	ModelIDs []string `json:"modelIds,omitempty"`
}

// SortObservations converts observations — as returned by
// actor.Loop.Observations — into a Bundle.Observations-ready slice, ordered
// ascending by Sequence. It is the canonical way a caller turns a Loop's
// retained observations into a Bundle field: see Bundle.Observations for why
// the slice form, not the map encoding/json would otherwise produce, is
// what Bundle stores.
func SortObservations(observations map[int64]observe.Observation) []RetainedObservation {
	out := make([]RetainedObservation, 0, len(observations))
	for seq, obs := range observations {
		out = append(out, RetainedObservation{Sequence: seq, Observation: obs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// AggregateModelIDs returns the sorted, deduplicated set of every non-empty
// actor.Usage.Model value across events. It is the canonical way
// Metadata.ModelIDs is computed, so two callers assembling a Bundle from the
// same events always produce the same aggregated identity list, regardless
// of how many times — or in what order — any one model was actually used.
func AggregateModelIDs(events []actor.LoopEvent) []string {
	seen := make(map[string]struct{})
	for _, e := range events {
		if e.Usage.Model == "" {
			continue
		}
		seen[e.Usage.Model] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ModuleVersion returns the chatwright module's own resolved version, read
// from the currently running binary's runtime/debug build info, or "" when
// it cannot be determined.
//
// A Bundle is normally produced by a bot's own test binary, which imports
// chatwright as a dependency rather than being chatwright itself. In that
// (expected) case it is chatwright's entry in the binary's
// debug.BuildInfo.Deps that carries the meaningful resolved version (a git
// tag or pseudo-version), so Deps is searched for chatwrightModulePath. The
// less common case — the running binary IS chatwright's own module, e.g. a
// test run inside this repository — is also covered, via
// debug.BuildInfo.Main, checked first.
//
// Either way, "(devel)" (Go's placeholder for "no resolvable version") and
// "" are both treated as "not available" — mirroring
// cmd/chatwright/main.go's own cliVersion fallback for the same reason: a
// plain `go build`/`go test` inside this repository, or inside a consumer
// module that has not pinned a chatwright version, never has one.
func ModuleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if bi.Main.Path == chatwrightModulePath {
		return resolvedVersion(bi.Main.Version)
	}
	for _, dep := range bi.Deps {
		if dep.Path == chatwrightModulePath {
			return resolvedVersion(dep.Version)
		}
	}
	return ""
}

// resolvedVersion returns v, or "" when v is Go's own placeholder for "no
// real version" ("(devel)") or already empty.
func resolvedVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	return v
}
