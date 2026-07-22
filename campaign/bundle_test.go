package campaign_test

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/campaign"
	"github.com/chatwright/chatwright/datastate"
	"github.com/chatwright/chatwright/goal"
	"github.com/chatwright/chatwright/observe"
	"github.com/chatwright/chatwright/platform"
)

// goldenBundle builds a small, fully deterministic Bundle exercising every
// field the schema currently has, for TestBundleRoundTripIsDeterministic's
// round-trip and golden-file comparison. Every timestamp is built from
// time.Date, never time.Now, so it carries no monotonic reading and
// round-trips through JSON (which discards monotonic readings anyway) with
// full reflect.DeepEqual fidelity, not just byte-identical re-encoding.
func goldenBundle() campaign.Bundle {
	fixedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	cost := 0.3

	events := []actor.LoopEvent{
		{
			Index: 0, At: fixedAt, TaskID: "onboarding", ObservationSequence: 1,
			Proposal: actor.Proposal{Kind: actor.ProposeSendText, Text: "Hi", Rationale: "start the conversation"},
			Usage:    actor.Usage{Model: "scripted-v1", InputTokens: 5, OutputTokens: 2, Cost: &cost},
			Action:   actor.ActionOutcome{Kind: actor.ActionExecuted},
		},
		{
			Index: 1, At: fixedAt.Add(time.Second), TaskID: "onboarding", ObservationSequence: 2,
			Proposal: actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "onboarding confirmed"},
			Action:   actor.ActionOutcome{Kind: actor.ActionTaskCompleted},
		},
	}
	g := goal.Goal{ID: "listus", Title: "Exercise onboarding", Tasks: []goal.Task{
		{ID: "onboarding", Title: "Complete onboarding", SuccessCriteria: "user completes language selection"},
	}}
	snapshot := goal.CampaignSnapshot{
		GoalID:     "listus",
		Statuses:   map[string]goal.TaskStatus{"onboarding": goal.TaskCompleted},
		Steps:      2,
		Cost:       0.3,
		Stopped:    true,
		StopReason: goal.StopGoalComplete,
	}
	report := campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: snapshot, Events: events})

	return campaign.Bundle{
		SchemaVersion: campaign.BundleSchemaVersion,
		Goal:          g,
		Chats: []campaign.ChatJournal{
			{
				ChatID: 42,
				Entries: []platform.JournalEntry{
					{Direction: platform.DirectionUser, Kind: platform.JournalEntryMessage, MessageID: 1, Text: "Hi", At: fixedAt},
					{
						Direction: platform.DirectionBot, Kind: platform.JournalEntryMessage, MessageID: 2, Text: "Choose your language:",
						Actions: [][]platform.Action{{{Label: "English", ID: "act1"}}}, At: fixedAt.Add(time.Second),
					},
					{Direction: platform.DirectionUser, Kind: platform.JournalEntryAction, RefMessageID: 2, Text: "act1", At: fixedAt.Add(2 * time.Second)},
					{Direction: platform.DirectionBot, Kind: platform.JournalEntryMessage, MessageID: 2, Version: 1, Text: "Howdy stranger", At: fixedAt.Add(3 * time.Second)},
				},
			},
		},
		Observations: []campaign.RetainedObservation{
			{
				Sequence: 1,
				Observation: observe.Observation{
					Sequence: 1, Chat: observe.ChatRef{ChatID: 42},
					Messages: []observe.VisibleMessage{{ID: "msg1", Actor: observe.ActorUser, Text: "Hi"}},
				},
			},
			{
				Sequence: 2,
				Observation: observe.Observation{
					Sequence: 2, PreviousSequence: 1, Chat: observe.ChatRef{ChatID: 42},
					Messages: []observe.VisibleMessage{
						{ID: "msg1", Actor: observe.ActorUser, Text: "Hi"},
						{
							ID: "msg2", Actor: observe.ActorBot, Text: "Choose your language:",
							Actions: []observe.AvailableAction{{ID: "act1", Label: "English", SeenAt: 2}},
						},
					},
					Changes: []observe.Change{{Kind: observe.ChangeNewMessage, MessageID: "msg2", Actor: observe.ActorBot}},
				},
			},
		},
		Events: events,
		Report: report,
		Evidence: []datastate.Evidence{
			{
				Name: "onboarding-language", AttachmentPoint: datastate.AttachmentAfterMessage,
				Holder: "listusdb", Query: "SELECT language FROM users WHERE id = @userId",
				Params:  map[string]any{"userId": "u1"},
				Outcome: datastate.OutcomePassed, TotalRows: 1, ReturnedRows: 1,
				Preview: []datastate.Row{{"language": "en"}},
			},
		},
		Metadata: campaign.Metadata{
			CreatedAt:       fixedAt,
			Platform:        "telegram",
			EndpointProfile: campaign.EndpointProfilePlatformEmulated,
			ModelIDs:        campaign.AggregateModelIDs(events),
		},
	}
}

// TestBundleRoundTripIsDeterministic proves WriteBundle/ReadBundle round-trip
// a Bundle without loss, that writing the same Bundle twice (directly, or
// after reading it back) produces byte-identical output, and that the
// output matches a checked-in golden file — so an accidental, undeclared
// change to the schema's shape or field order is caught by a test diff
// rather than discovered by a downstream player.
func TestBundleRoundTripIsDeterministic(t *testing.T) {
	bundle := goldenBundle()

	var first bytes.Buffer
	if err := campaign.WriteBundle(&first, bundle); err != nil {
		t.Fatalf("WriteBundle() error = %v", err)
	}

	roundTripped, err := campaign.ReadBundle(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatalf("ReadBundle() error = %v", err)
	}
	if !reflect.DeepEqual(bundle, roundTripped) {
		t.Fatalf("round-tripped bundle differs from the original:\ngot:  %+v\nwant: %+v", roundTripped, bundle)
	}

	var second bytes.Buffer
	if err := campaign.WriteBundle(&second, roundTripped); err != nil {
		t.Fatalf("WriteBundle(roundTripped) error = %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("WriteBundle is not deterministic across a read/write cycle:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}

	const goldenPath = "testdata/bundle_golden.json"
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", goldenPath, err)
	}
	if first.String() != string(golden) {
		t.Fatalf("bundle JSON no longer matches %s — if this schema change is deliberate, update the golden file; got:\n%s", goldenPath, first.String())
	}
}

// TestBundleReadRejectsUnknownSchemaVersion proves ReadBundle rejects a
// schemaVersion it does not recognise — older, newer, or otherwise unknown —
// with a typed error naming the version found, rather than silently
// unmarshalling the rest of the payload under today's field meanings.
func TestBundleReadRejectsUnknownSchemaVersion(t *testing.T) {
	tests := map[string]string{
		"newer":   `{"schemaVersion": 2}`,
		"older":   `{"schemaVersion": 0}`,
		"garbage": `{"schemaVersion": -7}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := campaign.ReadBundle(strings.NewReader(payload))
			if err == nil {
				t.Fatal("ReadBundle() error = nil, want an unknown-schema-version error")
			}
			if !errors.Is(err, campaign.ErrUnknownBundleSchemaVersion) {
				t.Fatalf("ReadBundle() error = %v, want it to wrap ErrUnknownBundleSchemaVersion", err)
			}
		})
	}
}

// TestBundleContainsProfileAndPlatformLabels proves a Bundle always names —
// never implies — its endpoint profile and platform (AGENTS.md's "fidelity
// is declared" principle applied to the run-bundle artifact), both as
// struct fields and as readable keys in the encoded JSON a player parses.
func TestBundleContainsProfileAndPlatformLabels(t *testing.T) {
	bundle := goldenBundle()

	if bundle.Metadata.Platform != "telegram" {
		t.Fatalf("bundle.Metadata.Platform = %q, want %q", bundle.Metadata.Platform, "telegram")
	}
	if bundle.Metadata.EndpointProfile != campaign.EndpointProfilePlatformEmulated {
		t.Fatalf("bundle.Metadata.EndpointProfile = %q, want %q", bundle.Metadata.EndpointProfile, campaign.EndpointProfilePlatformEmulated)
	}

	var buf bytes.Buffer
	if err := campaign.WriteBundle(&buf, bundle); err != nil {
		t.Fatalf("WriteBundle() error = %v", err)
	}
	encoded := buf.String()
	if !strings.Contains(encoded, `"platform": "telegram"`) {
		t.Fatalf("encoded bundle does not carry a readable platform label: %s", encoded)
	}
	if !strings.Contains(encoded, `"endpointProfile": "platform-emulated"`) {
		t.Fatalf("encoded bundle does not carry a readable endpointProfile label: %s", encoded)
	}
}
