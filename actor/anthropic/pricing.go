package anthropic

// PricingSnapshotDate is when PricingUSDPerMillionTokens was last checked
// against Anthropic's published pricing. It is a point-in-time snapshot,
// not a live price feed — Anthropic can change list prices at any time.
// Update both together when they drift from the source below.
const PricingSnapshotDate = "2026-06-24"

// PricingUSDPerMillionTokens was read from
// https://platform.claude.com/docs/en/pricing.

// modelPrice is one model's per-token list price, in US dollars per
// 1,000,000 tokens.
type modelPrice struct{ Input, Output float64 }

// PricingUSDPerMillionTokens is a snapshot of Anthropic's per-model list
// pricing (US dollars per 1,000,000 tokens) as of PricingSnapshotDate,
// sourced from pricingSourceURL. Propose uses it to fill actor.Usage.Cost
// automatically (see Config.DisableCostEstimate) for every model it has an
// entry for; a model with no entry leaves Usage.Cost nil rather than guess.
//
// Every entry here is the model's standard, non-promotional rate.
// claude-sonnet-5 in particular carries a temporary lower "intro" rate
// ($2/$10 per MTok) through 2026-08-31 that is deliberately NOT used here,
// so a campaign's estimated spend against goal.Budgets.MaxCost never
// understates the model's steady-state cost.
//
// Treat Usage.Cost as an estimate for campaign budgeting, not an invoice —
// see AGENTS.md's "fidelity is declared" principle. Refresh this table (and
// PricingSnapshotDate) when it drifts from pricingSourceURL.
var PricingUSDPerMillionTokens = map[string]modelPrice{
	DefaultModel:        {Input: 1.00, Output: 5.00},
	"claude-sonnet-5":   {Input: 3.00, Output: 15.00}, // standard rate; see the intro-rate note above
	"claude-sonnet-4-6": {Input: 3.00, Output: 15.00},
	"claude-opus-4-8":   {Input: 5.00, Output: 25.00},
	"claude-opus-4-7":   {Input: 5.00, Output: 25.00},
	"claude-fable-5":    {Input: 10.00, Output: 50.00},
	"claude-mythos-5":   {Input: 10.00, Output: 50.00},
}

// estimateCost returns model's estimated USD cost for inputTokens and
// outputTokens per PricingUSDPerMillionTokens, or nil if model has no entry
// there.
func estimateCost(model string, inputTokens, outputTokens int64) *float64 {
	price, ok := PricingUSDPerMillionTokens[model]
	if !ok {
		return nil
	}
	cost := float64(inputTokens)/1_000_000*price.Input + float64(outputTokens)/1_000_000*price.Output
	return &cost
}
