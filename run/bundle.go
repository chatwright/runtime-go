package run

import "github.com/chatwright/chatwright/bundle"

// AssembleBundleRunInput is everything AssembleBundleRun needs to build a
// bundle.Run from a completed Run's Result.
type AssembleBundleRunInput struct {
	// RunID, Platform and EndpointProfile become bundle.Run's own fields —
	// see bundle.Run.ID/Platform/EndpointProfile.
	RunID           string
	Platform        string
	EndpointProfile string
	// Actors is the run's roster — see bundle.Run.Actors.
	Actors []bundle.Actor
	// Chats is the run's full, final per-chat journal — the same shape
	// bundle.SingleAIGoalRunInput.Chats expects (every entry the whole Run
	// produced, in each chat, from the very start).
	Chats []bundle.ChatJournal
	// Result is a completed Run.Execute's own return value.
	Result Result
	// Bookmarks and Annotations become the Run's own fields verbatim — see
	// bundle.Run.Bookmarks/Annotations.
	Bookmarks   []bundle.Bookmark
	Annotations []bundle.Annotation
}

// AssembleBundleRun builds a bundle.Run from a completed Run's Result — the
// multi-part counterpart to bundle.SingleAIGoalRun's single-part mapping
// (see that function's own doc comment: "a future hybrid run assembles its
// Run directly ... once a runtime produces more than one Part"). Only Parts
// that actually executed (PartCompleted, PartFailed or PartCeilingStopped —
// see in.Result.Parts) become bundle.Part entries, in the order they ran; a
// Part the Run never reached (PartAborted, PartCoverageGap — see
// in.Result.Skipped) produced no journal entries to bound and so is not
// represented in the persisted evidence at all — a caller that wants to
// report on why the Run stopped short reads Result.Skipped/Result.
// CeilingTrip directly, which AssembleBundleRun deliberately leaves outside
// the bundle.Run shape rather than inventing a new bundle.Part field for it
// (see the package doc comment on not touching bundle's existing schema).
func AssembleBundleRun(in AssembleBundleRunInput) bundle.Run {
	parts := make([]bundle.Part, 0, len(in.Result.Parts))
	for _, outcome := range in.Result.Parts {
		parts = append(parts, bundle.Part{
			ID:              outcome.PartID,
			Title:           outcome.Title,
			Kind:            outcome.Kind,
			JournalBoundary: outcome.Boundary,
			AIGoal:          outcome.AIGoal,
		})
	}

	return bundle.Run{
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
