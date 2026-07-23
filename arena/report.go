package arena

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// WriteReport writes results as a markdown comparison report to w: an
// environment block (hardware, context lengths, load-hook outcomes, date —
// spec/ideas/actor-model-arena.md), a headline table (completion,
// cold-start, latency p50/p95, wall, tokens, steps, stop reasons), the
// required retry breakdown, the structured-output-mode split, a per-cell
// detail table naming every cell's bundle, and a short per-model
// narrative. Every declared provider in results.Models gets a row in every
// table, however its block went (a ProviderErr or a per-cell Err renders
// as an explicit error, never a silently dropped row) — the spec's
// exclusion policy: a model leaves a report only with a recorded,
// evidence-linked reason, never silently.
func WriteReport(w io.Writer, results Results) error {
	var b strings.Builder
	writeHeader(&b, results)
	writeHeadline(&b, results.Models)
	writeRetryBreakdown(&b, results.Models)
	writeStructuredMode(&b, results.Models)
	writePerCellTable(&b, results.Models)
	writeNarratives(&b, results.Models)

	_, err := io.WriteString(w, b.String())
	return err
}

func writeHeader(b *strings.Builder, results Results) {
	fmt.Fprintf(b, "# Actor model arena — %s\n\n", results.Scenario.Title)
	fmt.Fprintf(b, "Generated: %s\n\n", results.Environment.Date.Format(time.RFC3339))
	fmt.Fprintf(b, "## Environment\n\n")
	env := results.Environment
	if env.Hardware != "" {
		fmt.Fprintf(b, "- Hardware: %s\n", env.Hardware)
	}
	fmt.Fprintf(b, "- OS: %s/%s\n", env.OS, env.Arch)
	fmt.Fprintf(b, "- Go: %s\n", env.GoVersion)
	fmt.Fprintf(b, "- Scenario: %s@%s — %s\n", results.Scenario.ID, results.Scenario.Version, results.Scenario.Title)
	for _, pe := range env.Providers {
		ctxNote := "server default"
		if pe.Spec.ContextLength > 0 {
			ctxNote = fmt.Sprintf("%d tokens", pe.Spec.ContextLength)
		}
		fmt.Fprintf(b, "- %s: %s (model=%s, context=%s, load=%s)\n",
			pe.Spec.label(), pe.Spec.BaseURL, pe.Spec.Model, ctxNote, pe.LoadResult.Note)
	}
	fmt.Fprintf(b, "\n")
}

func writeHeadline(b *strings.Builder, models []ModelResult) {
	fmt.Fprintf(b, "## Headline comparison\n\n")
	fmt.Fprintf(b, "\"Completed\" is the model's own self-declared claim (Report TaskOutcome.Status); \"Verified\" is the scenario's independent, deterministic re-check of the journal — evidence, not a claim (see the per-model narrative for any mismatch between the two). \"n/a\" means this scenario declares no Verify step.\n\n")
	fmt.Fprintf(b, "| Model | Attempted | Completed | Verified | Cold-start (s) | Latency p50/p95 (s) | Wall p50 (s) | Tokens in/out (avg) | Steps (avg) | Stop reasons |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|---|---|---|---|\n")

	for _, m := range models {
		if m.ProviderErr != nil {
			fmt.Fprintf(b, "| %s | ERROR | — | — | — | — | — | — | — | provider error: %s |\n", m.Spec.label(), firstLine(m.ProviderErr.Error()))
			continue
		}

		attempted := len(m.Cells)
		var completed, verified, verifiable int
		var allLat []time.Duration
		var walls []time.Duration
		var inTok, outTok, steps, tokN int
		stopReasons := map[string]int{}
		for _, c := range m.Cells {
			if c.Err != nil {
				stopReasons["(error)"]++
				continue
			}
			if c.TaskStatus == "completed" {
				completed++
			}
			verifiable++
			if c.Verified {
				verified++
			}
			allLat = append(allLat, c.Latencies...)
			walls = append(walls, c.Wall)
			inTok += c.InputTokens
			outTok += c.OutputTokens
			steps += c.Steps
			tokN++
			sr := c.StopReason
			if sr == "" {
				sr = "(unknown)"
			}
			stopReasons[sr]++
		}

		p50, p95 := percentile(allLat, 50), percentile(allLat, 95)
		wallP50 := percentile(walls, 50)
		avgIn, avgOut, avgSteps := 0.0, 0.0, 0.0
		if tokN > 0 {
			avgIn = float64(inTok) / float64(tokN)
			avgOut = float64(outTok) / float64(tokN)
			avgSteps = float64(steps) / float64(tokN)
		}

		coldStart := "n/a"
		if m.Warmup != nil {
			coldStart = fmt.Sprintf("%.1f", m.Warmup.ColdStart.Seconds())
			if m.Warmup.Err != nil {
				coldStart += " (err)"
			}
		}

		verifiedCell := "n/a"
		if !hasNoVerifyDetail(m.Cells) {
			verifiedCell = fmt.Sprintf("%d/%d", verified, verifiable)
		}

		var srParts []string
		for sr, n := range stopReasons {
			srParts = append(srParts, fmt.Sprintf("%s×%d", sr, n))
		}
		sort.Strings(srParts)

		fmt.Fprintf(b, "| %s | %d/%d | %d/%d | %s | %s | %.1f / %.1f | %.1f | %.0f / %.0f | %.1f | %s |\n",
			m.Spec.label(), attempted, attempted, completed, attempted, verifiedCell, coldStart,
			p50.Seconds(), p95.Seconds(), wallP50.Seconds(), avgIn, avgOut, avgSteps, strings.Join(srParts, ", "))
	}
	fmt.Fprintf(b, "\n")
}

// hasNoVerifyDetail reports whether none of cells carries a VerifyDetail —
// i.e. the scenario declared no Scenario.Verify step at all, as opposed to
// one that ran and found nothing verified.
func hasNoVerifyDetail(cells []CellResult) bool {
	for _, c := range cells {
		if c.VerifyDetail != "" {
			return false
		}
	}
	return true
}

func writeRetryBreakdown(b *strings.Builder, models []ModelResult) {
	fmt.Fprintf(b, "## Retry breakdown\n\n")
	fmt.Fprintf(b, "Counts aggregated across every timed repeat. skipped-invalid/executed-no-effect/resolution-failed come from bundle LoopEvents (ActionOutcomeKind); transport-errors comes from CallRecord (a Propose call that errored before it could ever become a LoopEvent).\n\n")
	fmt.Fprintf(b, "| Model | skipped-invalid | executed-no-effect | resolution-failed | transport-errors |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|\n")
	for _, m := range models {
		if m.ProviderErr != nil {
			fmt.Fprintf(b, "| %s | — | — | — | — |\n", m.Spec.label())
			continue
		}
		skipped, noEffect, resFailed, transportErr := retryBreakdown(m.Cells)
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d |\n", m.Spec.label(), skipped, noEffect, resFailed, transportErr)
	}
	fmt.Fprintf(b, "\n")
}

// retryBreakdown tallies the four retry-breakdown counters
// spec/ideas/actor-model-arena.md requires, across every cell.
func retryBreakdown(cells []CellResult) (skipped, noEffect, resolutionFailed, transportErr int) {
	for _, c := range cells {
		skipped += c.ActionCounts["skipped-invalid"]
		noEffect += c.ActionCounts["executed-no-effect"]
		resolutionFailed += c.ActionCounts["resolution-failed"]
		for _, call := range c.Calls {
			if call.Error != "" {
				transportErr++
			}
		}
	}
	return
}

func writeStructuredMode(b *strings.Builder, models []ModelResult) {
	fmt.Fprintf(b, "## Structured-output mode\n\n")
	fmt.Fprintf(b, "Share of Propose calls (across every timed repeat) served by each response_format mode — see actor/openai's json_schema-first, json_object-fallback contract.\n\n")
	fmt.Fprintf(b, "| Model | json_schema | json_object_fallback | other/empty | calls |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|\n")
	for _, m := range models {
		if m.ProviderErr != nil {
			fmt.Fprintf(b, "| %s | — | — | — | 0 |\n", m.Spec.label())
			continue
		}
		schemaN, fallbackN, otherN, total := modeShares(m.Cells)
		pct := func(n int) string {
			if total == 0 {
				return "n/a"
			}
			return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(total))
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %d |\n", m.Spec.label(), pct(schemaN), pct(fallbackN), pct(otherN), total)
	}
	fmt.Fprintf(b, "\n")
}

// modeShares tallies every cell's call modes into the three buckets the
// report shows, plus the total call count actually carrying a mode
// (transport errors with no mode are excluded, matching total's own
// definition as "calls served by SOME response_format mode").
func modeShares(cells []CellResult) (schemaN, fallbackN, otherN, total int) {
	for _, c := range cells {
		for mode, n := range c.ModeCounts {
			total += n
			switch mode {
			case "json_schema":
				schemaN += n
			case "json_object_fallback":
				fallbackN += n
			default:
				otherN += n
			}
		}
	}
	return
}

func writePerCellTable(b *strings.Builder, models []ModelResult) {
	fmt.Fprintf(b, "## Per-cell detail\n\n")
	fmt.Fprintf(b, "Every cell names its bundle file, replayable in the Studio player.\n\n")
	fmt.Fprintf(b, "| Model | Repeat | Bundle | Part status | Stop reason | Task status | Steps | Verified | Wall (s) |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range models {
		if m.ProviderErr != nil {
			fmt.Fprintf(b, "| %s | — | (no cells: provider error) | — | — | — | — | — | — |\n", m.Spec.label())
			continue
		}
		for _, c := range m.Cells {
			if c.Err != nil {
				fmt.Fprintf(b, "| %s | %d | (none) | ERROR | %s | — | — | — | %.1f |\n", m.Spec.label(), c.Repeat, firstLine(c.Err.Error()), c.Wall.Seconds())
				continue
			}
			verified := "n/a"
			if c.VerifyDetail != "" {
				verified = fmt.Sprintf("%v", c.Verified)
			}
			fmt.Fprintf(b, "| %s | %d | `%s` | %s | %s | %s | %d | %s | %.1f |\n",
				m.Spec.label(), c.Repeat, c.BundleName, c.PartStatus, c.StopReason, c.TaskStatus, c.Steps, verified, c.Wall.Seconds())
		}
	}
	fmt.Fprintf(b, "\n")
}

func writeNarratives(b *strings.Builder, models []ModelResult) {
	fmt.Fprintf(b, "## Per-model narrative\n\n")
	for _, m := range models {
		fmt.Fprintf(b, "### %s\n\n", m.Spec.label())
		if m.ProviderErr != nil {
			fmt.Fprintf(b, "- Excluded: could not construct a provider for this spec — %s\n\n", m.ProviderErr.Error())
			continue
		}
		if m.Warmup == nil {
			fmt.Fprintf(b, "- No warm-up recorded.\n")
		} else {
			errNote := ""
			if m.Warmup.Err != nil {
				errNote = fmt.Sprintf(" — warm-up call itself errored: %s", firstLine(m.Warmup.Err.Error()))
			}
			fmt.Fprintf(b, "- Cold-start: %.1fs (mode=%s)%s\n", m.Warmup.ColdStart.Seconds(), m.Warmup.Call.Mode, errNote)
		}
		if len(m.Cells) == 0 {
			fmt.Fprintf(b, "- No cells recorded for this model.\n\n")
			continue
		}
		for _, c := range m.Cells {
			fmt.Fprintf(b, "- repeat %d: %s\n", c.Repeat, narrateCell(c))
		}
		fmt.Fprintf(b, "\n")
	}
}

func narrateCell(c CellResult) string {
	if c.Err != nil {
		return "did not produce a bundle — arena-level error: " + c.Err.Error()
	}

	var parts []string
	switch {
	case c.VerifyDetail != "" && c.Verified && c.TaskStatus == "completed":
		parts = append(parts, "completed the goal: "+c.VerifyDetail)
	case c.VerifyDetail != "" && c.TaskStatus == "completed" && !c.Verified:
		parts = append(parts, "declared done WITHOUT completing the journal evidence: "+c.VerifyDetail)
	case c.VerifyDetail != "" && c.Verified && c.TaskStatus != "completed":
		parts = append(parts, fmt.Sprintf("journal evidence complete but task status=%q (stopped before declaring done): %s", c.TaskStatus, c.VerifyDetail))
	case c.TaskStatus == "completed":
		parts = append(parts, "declared done (scenario declares no independent verification)")
	case c.TaskStatus == "failed":
		parts = append(parts, "gave up (task-done never proposed successfully)")
	default:
		detail := c.VerifyDetail
		if detail == "" {
			detail = "no independent verification available"
		}
		parts = append(parts, fmt.Sprintf("did not finish (task status=%q): %s", c.TaskStatus, detail))
	}

	if c.StopReason != "" && c.StopReason != "goal-complete" {
		parts = append(parts, fmt.Sprintf("stopped on %s", c.StopReason))
	}
	if n := c.ActionCounts["resolution-failed"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d resolution-failed action(s)", n))
	}
	if n := c.ActionCounts["skipped-invalid"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped-invalid proposal(s)", n))
	}
	if n := c.ActionCounts["executed-no-effect"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d no-effect action(s)", n))
	}
	if _, _, _, transportErr := retryBreakdown([]CellResult{c}); transportErr > 0 {
		parts = append(parts, fmt.Sprintf("%d transport/parse error(s)", transportErr))
	}
	parts = append(parts, fmt.Sprintf("%d steps", c.Steps))
	return strings.Join(parts, "; ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// percentile returns the p-th percentile of durs (nearest-rank method,
// matching the scratchpad harness's own percentile/percentileF). Returns 0
// for an empty input.
func percentile(durs []time.Duration, p float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
