package campaign_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/chatwright/chatwright/actor"
	"github.com/chatwright/chatwright/campaign"
	"github.com/chatwright/chatwright/goal"
)

// goldenReport builds a small, fully deterministic Report exercising every
// field the schema currently has, for TestReportSchemaIsVersioned's
// round-trip and golden-file comparison.
func goldenReport() campaign.Report {
	cost := 0.3
	events := []actor.LoopEvent{
		{
			Index: 0, TaskID: "onboarding", ObservationSequence: 1,
			Proposal: actor.Proposal{Kind: actor.ProposeSendText, Text: "Hi", Rationale: "start the conversation"},
			Usage:    actor.Usage{Model: "scripted-v1", InputTokens: 5, OutputTokens: 2, Cost: &cost},
			Action:   actor.ActionOutcome{Kind: actor.ActionExecuted},
		},
		{
			Index: 1, TaskID: "onboarding", ObservationSequence: 2,
			Proposal: actor.Proposal{Kind: actor.ProposeTaskDone, Rationale: "onboarding confirmed"},
			Action:   actor.ActionOutcome{Kind: actor.ActionTaskCompleted},
		},
	}
	snapshot := goal.CampaignSnapshot{
		GoalID:     "listus",
		Statuses:   map[string]goal.TaskStatus{"onboarding": goal.TaskCompleted},
		Steps:      2,
		Cost:       0.3,
		Stopped:    true,
		StopReason: goal.StopGoalComplete,
	}
	g := goal.Goal{ID: "listus", Title: "Exercise onboarding", Tasks: []goal.Task{
		{ID: "onboarding", Title: "Complete onboarding", SuccessCriteria: "user completes language selection"},
	}}
	return campaign.Assemble(campaign.AssembleInput{Goal: g, Campaign: snapshot, Events: events})
}

// TestReportSchemaIsVersioned proves Report carries a stable SchemaVersion,
// round-trips through JSON without loss, and matches a checked-in golden
// file — so an accidental, undeclared change to the schema's shape is
// caught by a test diff rather than discovered by a downstream consumer.
func TestReportSchemaIsVersioned(t *testing.T) {
	report := goldenReport()

	if report.SchemaVersion != campaign.ReportSchemaVersion {
		t.Fatalf("report.SchemaVersion = %d, want %d", report.SchemaVersion, campaign.ReportSchemaVersion)
	}
	if campaign.ReportSchemaVersion != 1 {
		t.Fatalf("ReportSchemaVersion = %d, want 1 (bump this test deliberately alongside any schema change)", campaign.ReportSchemaVersion)
	}

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal into map error = %v", err)
	}
	if _, ok := fields["schemaVersion"]; !ok {
		t.Fatal(`encoded report has no "schemaVersion" field`)
	}

	var roundTripped campaign.Report
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(report, roundTripped) {
		t.Fatalf("round-tripped report differs from the original:\ngot:  %+v\nwant: %+v", roundTripped, report)
	}

	const goldenPath = "testdata/report_golden.json"
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", goldenPath, err)
	}
	encoded = append(encoded, '\n')
	if string(encoded) != string(golden) {
		t.Fatalf("report JSON no longer matches %s — if this schema change is deliberate, update the golden file; got:\n%s", goldenPath, encoded)
	}
}
