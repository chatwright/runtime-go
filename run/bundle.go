package run

import (
	"chatwright.dev/runtime/actor"
	"chatwright.dev/runtime/campaign"
	"chatwright.dev/runtime/datastate"
	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/sdk"
)

// SingleAIGoalRunInput is everything SingleAIGoalRun needs to assemble a
// single-part, single-run sdk.Run from a plain campaign's own pieces. The
// campaign-produced fields (Goal, Events, Observations, Report, Evidence)
// take this runtime's own types — SingleAIGoalRun converts them to the
// sdk's wire shapes internally (see wire.go); the roster fields (Actors,
// Chats, Bookmarks, Annotations) are wire-typed already, since a roster is
// a bundle-only concept with no runtime counterpart — callers build each
// Chats entry with WireJournal.
type SingleAIGoalRunInput struct {
	// RunID is caller-supplied — see sdk.Run.ID.
	RunID string
	// Platform is the platform name the run drove — see sdk.Run.Platform.
	Platform string
	// EndpointProfile is the run's declared endpoint profile — see
	// sdk.Run.EndpointProfile, sdk.EndpointProfilePlatformEmulated.
	EndpointProfile string
	// Actors is the run's roster — see sdk.Run.Actors.
	Actors []sdk.Actor
	// Chats is the run's continuous per-chat journal — see sdk.Run.Chats;
	// build each entry with WireJournal from the emulator's own Journal.
	Chats []sdk.ChatJournal

	// PartID and PartTitle name the single ai-goal Part this run gets — see
	// sdk.Part.ID, sdk.Part.Title.
	PartID    string
	PartTitle string

	// ActorID references the Actors entry that ran the loop — see
	// sdk.AIGoalSection.ActorID.
	ActorID string

	// Goal is the goal.Goal the part's actor loop ran; it becomes the
	// part's sdk.AIGoalSection.Goal.
	Goal goal.Goal
	// Events is every actor.LoopEvent the loop recorded (actor.Loop.Events),
	// in the loop's own order; it becomes sdk.AIGoalSection.Events.
	Events []actor.LoopEvent
	// Observations is the raw retained-observation map exactly as
	// actor.Loop.Observations returns it. SingleAIGoalRun sorts it ascending
	// by Sequence and converts it internally — absorbing the pre-split
	// bundle package's SortObservations — so a caller never orders (or
	// wire-converts) observations by hand. See sdk.AIGoalSection.Observations
	// for why the stored form is an ordered slice, not this map.
	Observations map[int64]observe.Observation
	// Report is the part's assembled campaign.Report (campaign.Assemble); it
	// becomes sdk.AIGoalSection.Report.
	Report campaign.Report
	// Evidence is the datastate.Evidence any data-state assertions produced,
	// in the order they were run; it becomes sdk.AIGoalSection.Evidence.
	Evidence []datastate.Evidence

	// Bookmarks and Annotations become the Run's own fields verbatim — see
	// sdk.Run.Bookmarks, sdk.Run.Annotations. Both are optional: a caller
	// with nothing to attach leaves them nil, and the resulting Run carries
	// none (SingleAIGoalRun never invents one).
	Bookmarks   []sdk.Bookmark
	Annotations []sdk.Annotation
}

// SingleAIGoalRun builds an sdk.Run containing exactly one ai-goal Part
// whose JournalBoundary spans each chat's entire journal — the "plain
// campaign is a single ai-goal part" path the standard repository's
// spec/ideas/hybrid-runs.md MVP scope describes. It moved here from the old
// bundle package when the wire model split out to chatwright.dev/sdk: the
// sdk owns the shapes, this runtime owns the conversion from its own
// internal types (see wire.go). A future hybrid run assembles its Run via
// AssembleBundleRun instead, once more than one Part actually ran; this
// helper only ever emits one.
func SingleAIGoalRun(in SingleAIGoalRunInput) sdk.Run {
	boundary := sdk.JournalBoundary{Chats: make([]sdk.ChatBoundary, 0, len(in.Chats))}
	for _, chat := range in.Chats {
		boundary.Chats = append(boundary.Chats, sdk.ChatBoundary{
			ChatID:     chat.ChatID,
			FirstEntry: 0,
			EntryCount: len(chat.Entries),
		})
	}

	part := sdk.Part{
		ID:              in.PartID,
		Title:           in.PartTitle,
		Kind:            sdk.PartKindAIGoal,
		JournalBoundary: boundary,
		AIGoal: &sdk.AIGoalSection{
			Goal:         wireGoal(in.Goal),
			ActorID:      in.ActorID,
			Events:       wireLoopEvents(in.Events),
			Observations: wireRetainedObservations(in.Observations),
			Report:       wireReport(in.Report),
			Evidence:     wireDataStateEvidences(in.Evidence),
		},
	}

	return sdk.Run{
		ID:              in.RunID,
		Platform:        in.Platform,
		EndpointProfile: in.EndpointProfile,
		Actors:          in.Actors,
		Chats:           in.Chats,
		Parts:           []sdk.Part{part},
		Bookmarks:       in.Bookmarks,
		Annotations:     in.Annotations,
	}
}

// AssembleBundleRunInput is everything AssembleBundleRun needs to build an
// sdk.Run from a completed Run's Result.
type AssembleBundleRunInput struct {
	// RunID, Platform and EndpointProfile become sdk.Run's own fields —
	// see sdk.Run.ID/Platform/EndpointProfile.
	RunID           string
	Platform        string
	EndpointProfile string
	// Actors is the run's roster — see sdk.Run.Actors.
	Actors []sdk.Actor
	// Chats is the run's full, final per-chat journal — the same shape
	// SingleAIGoalRunInput.Chats expects (every entry the whole Run
	// produced, in each chat, from the very start); build each entry with
	// WireJournal.
	Chats []sdk.ChatJournal
	// Result is a completed Run.Execute's own return value.
	Result Result
	// Bookmarks and Annotations become the Run's own fields verbatim — see
	// sdk.Run.Bookmarks/Annotations.
	Bookmarks   []sdk.Bookmark
	Annotations []sdk.Annotation
}

// AssembleBundleRun builds an sdk.Run from a completed Run's Result — the
// multi-part counterpart to SingleAIGoalRun's single-part mapping. Only
// Parts that actually executed (PartCompleted, PartFailed or
// PartCeilingStopped — see in.Result.Parts) become sdk.Part entries, in the
// order they ran; a Part the Run never reached (PartAborted,
// PartCoverageGap — see in.Result.Skipped) produced no journal entries to
// bound and so is not represented in the persisted evidence at all — a
// caller that wants to report on why the Run stopped short reads
// Result.Skipped/Result.CeilingTrip directly, which AssembleBundleRun
// deliberately leaves outside the sdk.Run shape rather than inventing a new
// sdk.Part field for it (the wire schema is the sdk's to change, never this
// package's).
func AssembleBundleRun(in AssembleBundleRunInput) sdk.Run {
	parts := make([]sdk.Part, 0, len(in.Result.Parts))
	for _, outcome := range in.Result.Parts {
		parts = append(parts, sdk.Part{
			ID:              outcome.PartID,
			Title:           outcome.Title,
			Kind:            outcome.Kind,
			JournalBoundary: outcome.Boundary,
			AIGoal:          outcome.AIGoal,
		})
	}

	return sdk.Run{
		ID:              in.RunID,
		Platform:        in.Platform,
		EndpointProfile: in.EndpointProfile,
		Actors:          in.Actors,
		Chats:           in.Chats,
		Parts:           parts,
		Bookmarks:       in.Bookmarks,
		Annotations:     in.Annotations,
	}
}
