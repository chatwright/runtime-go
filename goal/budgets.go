package goal

import "time"

// Budgets bounds one campaign run. Every numeric field's zero value means
// "no limit"; a negative value is invalid. MaxCost is the one genuinely
// optional field: nil means cost is not budgeted at all.
type Budgets struct {
	// MaxSteps caps the number of steps CampaignState.RecordStep counts.
	// Zero means unlimited.
	MaxSteps int

	// MaxDuration caps wall-clock time elapsed since the campaign started,
	// measured by the CampaignState's injected clock. Zero means unlimited.
	MaxDuration time.Duration

	// MaxRepeatedFailures caps how many times CampaignState.RecordFailure
	// may be called for a single task before the campaign stops. Zero means
	// unlimited.
	MaxRepeatedFailures int

	// MaxCost optionally caps spend against the campaign (tokens, currency
	// or another caller-defined unit — whatever unit the caller accrues via
	// CampaignState.RecordCost). Nil means cost is not budgeted.
	MaxCost *float64
}

// validate rejects a negative MaxSteps, MaxDuration or MaxRepeatedFailures,
// and a MaxCost that is set but not positive.
func (b Budgets) validate() error {
	if b.MaxSteps < 0 {
		return fmtBudgetErr(ErrNegativeBudget, "max steps", b.MaxSteps)
	}
	if b.MaxDuration < 0 {
		return fmtBudgetErr(ErrNegativeBudget, "max duration", b.MaxDuration)
	}
	if b.MaxRepeatedFailures < 0 {
		return fmtBudgetErr(ErrNegativeBudget, "max repeated failures", b.MaxRepeatedFailures)
	}
	if b.MaxCost != nil && *b.MaxCost <= 0 {
		return fmtBudgetErr(ErrNonPositiveCostBudget, "max cost", *b.MaxCost)
	}
	return nil
}
