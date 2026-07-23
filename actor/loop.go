package actor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"chatwright.dev/runtime/goal"
	"chatwright.dev/runtime/observe"
	"chatwright.dev/runtime/platform"
)

// Actuator is the narrow seam the loop acts through: exactly the subset of
// platform.Emulator needed to submit a user action and read back the bot's
// raw reply. platform.Emulator satisfies it directly. This is deliberately
// the only place in this package that ever sees a platform-native message ID
// or callback datum — a Provider never does (see observe's doctrine that
// actors receive only the semantic Observation surface, never raw platform
// payloads).
type Actuator interface {
	SubmitText(chatID int64, user platform.User, text string) error
	SubmitClick(chatID int64, user platform.User, data string, targetMessageID int) error
	WaitForMessage(chatID int64, consumed int, timeout time.Duration) (*platform.Message, bool)
	WaitForEdit(chatID int64, messageID int, afterVersion int, timeout time.Duration) (*platform.Message, bool)
}

// ErrNilClock means Config.Now was nil.
var ErrNilClock = errors.New("actor: clock function is nil")

// Config configures a Loop.
type Config struct {
	// ChatID and User identify the conversation the loop drives, exactly as
	// passed to Actuator.SubmitText/SubmitClick.
	ChatID int64
	User   platform.User

	// HistoryWindow bounds how many recent LoopEvents are fed to each
	// Prompt.History. Defaults to 10 if <= 0.
	HistoryWindow int
	// NonProgressLimit is how many consecutive invalid-or-no-effect
	// proposals the loop tolerates for one task before stopping the
	// campaign itself (via goal.CampaignState.Abort) rather than looping
	// forever. Defaults to 3 if <= 0.
	NonProgressLimit int
	// ActWaitTimeout bounds how long the loop waits, after acting, for the
	// platform's raw reply (WaitForMessage/WaitForEdit) that the journal
	// read (observe.Engine.Observe) already showed exists. Defaults to 5s
	// if <= 0.
	ActWaitTimeout time.Duration

	// Now supplies the loop's notion of the current time, stamped onto
	// every LoopEvent.At. Must not be nil; pass the same clock given to the
	// Loop's goal.CampaignState so timestamps and budget decisions agree.
	Now func() time.Time

	// DisableObservationRetention turns off the Loop's retention of every
	// observe.Observation it produces (see Loop.Observations). Retention is
	// ON by default (the zero value is false) — a campaign's entire purpose
	// is producing evidence, and the retained observation bodies are what
	// let a run bundle (chatwright.dev/sdk's Bundle) stay self-contained: a
	// player can show exactly
	// what the actor saw at each step without re-deriving it from a
	// transcript. Set this true only when a campaign is long enough, or
	// memory-bounded enough, that holding every Observation body for its
	// whole run is not affordable; Loop.Events (and campaign.Report) are
	// unaffected either way, since neither depends on retention.
	DisableObservationRetention bool

	// DisableOvershootProbe turns off the Loop's overshoot probe (see
	// RunTask's evidence-defined-completion handling): the one extra
	// Provider.Propose call RunTask otherwise issues, records and never
	// executes the moment a task's goal.Task.Criteria are found to hold,
	// solely to measure whether the actor would have kept acting —
	// spec/ideas/evidence-defined-completion.md's "stops-when-done rate".
	// The probe is ON by default (the zero value is false), since the
	// idea's own MVP proof requires it; set this true to skip the extra
	// call's cost when that measurement is not wanted.
	DisableOvershootProbe bool

	// OnProgress, when non-nil, is called with a ProgressSnapshot once per
	// loop iteration and once at each task-start/task-end boundary — pure,
	// derived, in-process reporting (spec/ideas/campaign-progress-reporting.md):
	// nothing it receives is added to Loop.Events, a campaign.Report or any
	// run bundle. Called synchronously, on the same goroutine RunTask runs
	// on; a slow or blocking OnProgress delays the loop itself. Nil (the
	// zero value) means no progress reporting.
	OnProgress func(ProgressSnapshot)
}

// withDefaults returns cfg with zero-value tunables replaced by their
// defaults, after validating the required fields.
func (cfg Config) withDefaults() (Config, error) {
	if cfg.Now == nil {
		return Config{}, ErrNilClock
	}
	if cfg.HistoryWindow <= 0 {
		cfg.HistoryWindow = 10
	}
	if cfg.NonProgressLimit <= 0 {
		cfg.NonProgressLimit = 3
	}
	if cfg.ActWaitTimeout <= 0 {
		cfg.ActWaitTimeout = 5 * time.Second
	}
	return cfg, nil
}

// TaskResult is what one Loop.RunTask call produced.
type TaskResult struct {
	TaskID string
	// Status is the task's goal.TaskStatus when RunTask returned.
	Status goal.TaskStatus
	// Stopped is true if the whole campaign had stopped (any
	// goal.StopReason, including this task completing the goal) by the time
	// RunTask returned.
	Stopped bool
	// NonProgress is true if this task's run ended specifically via the
	// loop's own non-progress detection (Config.NonProgressLimit) rather
	// than a goal.CampaignState-native stop. When true, Stopped is also
	// true: the loop stops the whole campaign (via Abort) rather than
	// silently moving on to another task, since non-progress on one task is
	// evidence the actor itself is stuck, not that the task is unreachable.
	NonProgress bool
}

// Loop drives goal.CampaignState's tasks through the
// observe-plan-act-validate cycle: observe (observe.Engine.Observe), plan
// (Provider.Propose), validate (observe.Engine.Validate for clicks, plus
// CampaignState's own guarded transitions), act (Actuator), record (append a
// LoopEvent). Budgets and stop reasons are enforced entirely through the
// injected goal.CampaignState — see RunTask.
//
// A Loop is not safe for concurrent use: drive one campaign from one
// goroutine (see the "Out of scope: parallel actors" note in the slice-2
// plan).
type Loop struct {
	provider Provider
	engine   *observe.Engine
	actuator Actuator
	campaign *goal.CampaignState
	goalDef  goal.Goal
	cfg      Config

	events              []LoopEvent
	consumedBotMessages int // mirrors cw.Chat's own WaitForMessage cursor
	lastBotMessage      *platform.Message

	// observations retains every observe.Observation this Loop has produced
	// (see observeAndSync), keyed by its Sequence, when
	// Config.DisableObservationRetention is false. Always non-nil so
	// Observations can range over it unconditionally.
	observations map[int64]observe.Observation
}

// NewLoop constructs a Loop. campaign must have been created from goalDef
// (NewLoop does not itself validate that, but task lookups assume it).
func NewLoop(provider Provider, engine *observe.Engine, actuator Actuator, campaign *goal.CampaignState, goalDef goal.Goal, cfg Config) (*Loop, error) {
	if provider == nil {
		return nil, errors.New("actor: NewLoop: provider is nil")
	}
	if engine == nil {
		return nil, errors.New("actor: NewLoop: engine is nil")
	}
	if actuator == nil {
		return nil, errors.New("actor: NewLoop: actuator is nil")
	}
	if campaign == nil {
		return nil, errors.New("actor: NewLoop: campaign is nil")
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Loop{
		provider: provider, engine: engine, actuator: actuator, campaign: campaign, goalDef: goalDef, cfg: cfg,
		observations: make(map[int64]observe.Observation),
	}, nil
}

// Events returns a detached copy of every LoopEvent recorded so far, across
// every task this Loop has run.
func (l *Loop) Events() []LoopEvent { return append([]LoopEvent(nil), l.events...) }

// Observations returns a detached copy of every observe.Observation this
// Loop has produced so far (every observe.Engine.Observe call
// observeAndSync made — including the post-action re-observation
// observedEffect performs, not only the ones a LoopEvent.ObservationSequence
// points at), keyed by its Sequence. It is always empty, never nil, so a
// caller can range over it unconditionally — either because nothing has
// been observed yet, or because Config.DisableObservationRetention is true.
func (l *Loop) Observations() map[int64]observe.Observation {
	out := make(map[int64]observe.Observation, len(l.observations))
	for seq, obs := range l.observations {
		out[seq] = obs
	}
	return out
}

// taskByID finds goalDef's Task with the given id.
func (l *Loop) taskByID(id string) (goal.Task, bool) {
	for _, t := range l.goalDef.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return goal.Task{}, false
}

// RunTask activates taskID (if it is currently Pending and eligible; a
// resumed Active task is driven as-is) and runs the
// observe-plan-act-validate cycle until the task reaches a terminal status,
// the campaign stops for any reason (a budget, goal-complete from another
// path, cancellation, error), or the loop's own non-progress detection
// fires.
//
// Evidence-defined completion (spec/ideas/evidence-defined-completion.md):
// when task.Criteria is set, RunTask evaluates it after every EXECUTED
// action (ActionExecuted — a real, observed effect; never after a no-effect
// or invalid one) against the fresh post-action observation. The moment it
// holds, RunTask completes the task itself
// (goal.CampaignState.CompleteByEvidence, stop reason
// goal.StopGoalMetByEvidence when this is the campaign's last task) and
// returns — the actor cannot continue a task RunTask has already closed
// out this way. Unless Config.DisableOvershootProbe, it first issues one
// more Provider.Propose call for the same task (probeOvershoot), recorded
// as a LoopEvent with ActionOvershootProbe and never executed, so a
// Provider that would have kept proposing leaves that intent as evidence
// (campaign.FindingActorOvershoot at report assembly) without ever
// mutating platform state past the met moment.
func (l *Loop) RunTask(ctx context.Context, taskID string) (TaskResult, error) {
	task, ok := l.taskByID(taskID)
	if !ok {
		return TaskResult{}, fmt.Errorf("actor: RunTask: unknown task %q", taskID)
	}

	status, err := l.campaign.TaskStatus(taskID)
	if err != nil {
		return TaskResult{}, err
	}
	if status == goal.TaskPending {
		if err := l.campaign.Activate(taskID); err != nil {
			return TaskResult{}, err
		}
	} else if status.Terminal() {
		return TaskResult{TaskID: taskID, Status: status, Stopped: l.campaign.Stopped()}, nil
	}

	nonProgressStreak := 0
	iteration := 0
	retryCounts := make(map[ActionOutcomeKind]int)
	l.emitProgress(task, ProgressTaskStarted, iteration, nonProgressStreak, retryCounts)

	endTask := func(result TaskResult) (TaskResult, error) {
		l.emitProgress(task, ProgressTaskEnded, iteration, nonProgressStreak, retryCounts)
		return result, nil
	}

	for {
		if l.campaign.Stopped() {
			status, _ := l.campaign.TaskStatus(taskID)
			return endTask(TaskResult{TaskID: taskID, Status: status, Stopped: true})
		}

		obs, err := l.observeAndSync()
		if err != nil {
			return TaskResult{}, err
		}

		prompt := l.buildPrompt(task, obs)
		proposal, usage, err := l.provider.Propose(ctx, prompt)
		if err != nil {
			// Record what this iteration DID manage to do — observe — before
			// returning, so the failure leaves evidence in l.events (and so
			// a run bundle assembled from it) instead of vanishing with only
			// a returned Go error. No Proposal was ever produced, so Usage,
			// Validation and Action all stay at their zero value; see
			// LoopEvent.ProposeError's doc comment.
			l.events = append(l.events, LoopEvent{
				Index:               len(l.events),
				At:                  l.cfg.Now(),
				TaskID:              taskID,
				ObservationSequence: obs.Sequence,
				ProposeError:        err.Error(),
			})
			return TaskResult{}, fmt.Errorf("actor: propose: %w", err)
		}

		event := LoopEvent{
			Index:               len(l.events),
			At:                  l.cfg.Now(),
			TaskID:              taskID,
			ObservationSequence: obs.Sequence,
			Proposal:            proposal,
			Usage:               usage,
		}

		progressed, action, validation, postObs, err := l.actOn(ctx, task, obs, proposal)
		if err != nil {
			return TaskResult{}, err
		}
		event.Validation = validation
		event.Action = action
		l.events = append(l.events, event)
		iteration++
		retryCounts[action.Kind]++

		if err := l.campaign.RecordStep(); err != nil && !errors.Is(err, goal.ErrCampaignStopped) {
			return TaskResult{}, fmt.Errorf("actor: record step: %w", err)
		}
		if usage.Cost != nil {
			if err := l.campaign.RecordCost(*usage.Cost); err != nil && !errors.Is(err, goal.ErrCampaignStopped) {
				return TaskResult{}, fmt.Errorf("actor: record cost: %w", err)
			}
		}
		if action.Kind == ActionResolutionFailed {
			if err := l.campaign.RecordFailure(taskID); err != nil && !errors.Is(err, goal.ErrCampaignStopped) {
				return TaskResult{}, fmt.Errorf("actor: record failure: %w", err)
			}
		}

		if action.Kind == ActionTaskCompleted || action.Kind == ActionTaskGivenUp {
			status, _ := l.campaign.TaskStatus(taskID)
			l.emitProgress(task, ProgressIteration, iteration, nonProgressStreak, retryCounts)
			return endTask(TaskResult{TaskID: taskID, Status: status, Stopped: l.campaign.Stopped()})
		}

		if action.Kind == ActionExecuted && task.Criteria != nil && postObs != nil {
			met, cerr := task.Criteria(ctx, *postObs)
			if cerr != nil {
				return TaskResult{}, fmt.Errorf("actor: evaluate criteria: %w", cerr)
			}
			if met {
				if err := l.campaign.CompleteByEvidence(taskID); err != nil && !errors.Is(err, goal.ErrCampaignStopped) {
					return TaskResult{}, fmt.Errorf("actor: complete by evidence: %w", err)
				}
				if !l.cfg.DisableOvershootProbe {
					l.probeOvershoot(ctx, task, postObs)
				}
				status, _ := l.campaign.TaskStatus(taskID)
				l.emitProgress(task, ProgressIteration, iteration, nonProgressStreak, retryCounts)
				return endTask(TaskResult{TaskID: taskID, Status: status, Stopped: l.campaign.Stopped()})
			}
		}

		if progressed {
			nonProgressStreak = 0
		} else {
			nonProgressStreak++
		}
		l.emitProgress(task, ProgressIteration, iteration, nonProgressStreak, retryCounts)
		if !progressed && nonProgressStreak >= l.cfg.NonProgressLimit {
			_ = l.campaign.Abort() // already-stopped race is fine to ignore: Abort is a no-op then.
			status, _ := l.campaign.TaskStatus(taskID)
			return endTask(TaskResult{TaskID: taskID, Status: status, Stopped: true, NonProgress: true})
		}
	}
}

// probeOvershoot requests exactly one more Provider.Propose call for task
// after its Criteria were found to hold, records it as a LoopEvent with
// ActionOvershootProbe (or a normal ProposeError-carrying event if the
// probe call itself fails) and never acts on it — see RunTask's own doc
// comment and spec/ideas/evidence-defined-completion.md's "overshoot
// probe". A probe failure is swallowed as evidence, never returned: the
// task has already completed successfully, and a probe is best-effort
// measurement, never a condition of that success.
func (l *Loop) probeOvershoot(ctx context.Context, task goal.Task, obs *observe.Observation) {
	prompt := l.buildPrompt(task, obs)
	proposal, usage, err := l.provider.Propose(ctx, prompt)
	event := LoopEvent{
		Index:               len(l.events),
		At:                  l.cfg.Now(),
		TaskID:              task.ID,
		ObservationSequence: obs.Sequence,
		Usage:               usage,
	}
	if err != nil {
		event.ProposeError = err.Error()
	} else {
		event.Proposal = proposal
		event.Action = ActionOutcome{
			Kind:   ActionOvershootProbe,
			Detail: "requested after evidence-defined completion; recorded, never executed",
		}
	}
	l.events = append(l.events, event)
}

// emitProgress builds and delivers a ProgressSnapshot for task, when
// Config.OnProgress is set — a no-op otherwise. See ProgressSnapshot and
// spec/ideas/campaign-progress-reporting.md.
func (l *Loop) emitProgress(task goal.Task, phase ProgressPhase, iteration, nonProgressStreak int, retryCounts map[ActionOutcomeKind]int) {
	if l.cfg.OnProgress == nil {
		return
	}

	snapshot := l.campaign.Snapshot()
	taskIndex, taskCount := 0, len(l.goalDef.Tasks)
	tasksCompleted := 0
	for i, t := range l.goalDef.Tasks {
		if t.ID == task.ID {
			taskIndex = i + 1
		}
		if snapshot.Statuses[t.ID] == goal.TaskCompleted {
			tasksCompleted++
		}
	}

	budgets := l.goalDef.Budgets
	burn := BudgetBurn{
		Steps:            burnFraction(float64(snapshot.Steps), float64(budgets.MaxSteps)),
		Duration:         burnFraction(float64(snapshot.Elapsed), float64(budgets.MaxDuration)),
		RepeatedFailures: burnFraction(float64(snapshot.Failures[task.ID]), float64(budgets.MaxRepeatedFailures)),
	}
	if budgets.MaxCost != nil {
		burn.Cost = burnFraction(snapshot.Cost, *budgets.MaxCost)
	}

	retryCountsCopy := make(map[ActionOutcomeKind]int, len(retryCounts))
	for k, v := range retryCounts {
		retryCountsCopy[k] = v
	}

	l.cfg.OnProgress(ProgressSnapshot{
		Phase:             phase,
		GoalID:            l.goalDef.ID,
		TaskID:            task.ID,
		TaskIndex:         taskIndex,
		TaskCount:         taskCount,
		TasksCompleted:    tasksCompleted,
		Iteration:         iteration,
		Budgets:           budgets,
		Burn:              burn,
		NonProgressStreak: nonProgressStreak,
		RetryCounts:       retryCountsCopy,
		Stopped:           snapshot.Stopped,
		StopReason:        snapshot.StopReason,
	})
}

// burnFraction returns consumed/max, or 0 when max is <= 0 (goal.Budgets'
// own "zero means unlimited" convention — an unbudgeted dimension is never
// "burned").
func burnFraction(consumed, max float64) float64 {
	if max <= 0 {
		return 0
	}
	return consumed / max
}

// RunCampaign repeatedly runs RunTask for every eligible task (in goalDef's
// declared order — see Loop's own non-progress/budget guards for why this is
// safe to do unattended) until no task is eligible or the campaign has
// stopped.
func (l *Loop) RunCampaign(ctx context.Context) ([]TaskResult, error) {
	var results []TaskResult
	for {
		if l.campaign.Stopped() {
			return results, nil
		}
		taskID, ok := l.nextEligibleTask()
		if !ok {
			return results, nil
		}
		result, err := l.RunTask(ctx, taskID)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
}

// nextEligibleTask returns the first task (in goalDef.Tasks order) that is
// currently eligible to be activated.
func (l *Loop) nextEligibleTask() (string, bool) {
	for _, t := range l.goalDef.Tasks {
		if eligible, err := l.campaign.Eligible(t.ID); err == nil && eligible {
			return t.ID, true
		}
	}
	return "", false
}

// buildPrompt assembles task's Prompt from obs and the loop's bounded recent
// history.
func (l *Loop) buildPrompt(task goal.Task, obs *observe.Observation) Prompt {
	start := len(l.events) - l.cfg.HistoryWindow
	if start < 0 {
		start = 0
	}
	history := append([]LoopEvent(nil), l.events[start:]...)
	return Prompt{
		GoalID:              l.goalDef.ID,
		GoalTitle:           l.goalDef.Title,
		GoalDescription:     l.goalDef.Description,
		Constraints:         l.goalDef.Constraints,
		TaskID:              task.ID,
		TaskTitle:           task.Title,
		TaskSuccessCriteria: task.SuccessCriteria,
		Observation:         *obs,
		History:             history,
	}
}

// observeAndSync calls observe.Engine.Observe and keeps the loop's raw
// (platform-native) view of the latest bot message in sync with it — see
// refreshRawBotMessage. It is the loop's single choke point for every
// observe.Engine.Observe call (the initial per-iteration observation and
// observedEffect's post-action re-observation both go through it), which is
// why retention (Config.DisableObservationRetention, Observations) is
// implemented here rather than at each call site.
func (l *Loop) observeAndSync() (*observe.Observation, error) {
	obs, err := l.engine.Observe()
	if err != nil {
		return nil, fmt.Errorf("actor: observe: %w", err)
	}
	if !l.cfg.DisableObservationRetention {
		l.observations[obs.Sequence] = *obs
	}
	if err := l.refreshRawBotMessage(obs); err != nil {
		return nil, err
	}
	return obs, nil
}

// refreshRawBotMessage keeps l.lastBotMessage — the raw platform.Message
// backing the most recent bot-authored entry in obs.Messages — in sync,
// using exactly the primitives cw.Chat/BotMessage use internally
// (WaitForMessage for a new message, WaitForEdit for an in-place edit of the
// one already held). obs's journal read already proved the message/edit
// exists, so these calls are a synchronisation formality, not a real wait,
// except as a defensive timeout.
//
// Scoping note: this tracks only the single most recently observed
// bot-authored message. A click proposal targeting an action on an *older*
// still-visible bot message (multiple simultaneously live action surfaces)
// cannot be resolved to a platform-native click by this slice's loop — see
// resolveClickTarget — which is consistent with the plan's "parallel
// actors"/single-surface MVP scope.
func (l *Loop) refreshRawBotMessage(obs *observe.Observation) error {
	var latestBot *observe.VisibleMessage
	botCount := 0
	for i := range obs.Messages {
		if obs.Messages[i].Actor == observe.ActorBot {
			botCount++
			latestBot = &obs.Messages[i]
		}
	}

	for l.consumedBotMessages < botCount {
		msg, ok := l.actuator.WaitForMessage(l.cfg.ChatID, l.consumedBotMessages, l.cfg.ActWaitTimeout)
		if !ok {
			return fmt.Errorf("actor: expected bot message #%d to already be journaled, but WaitForMessage timed out", l.consumedBotMessages+1)
		}
		l.consumedBotMessages++
		l.lastBotMessage = msg
	}

	if l.lastBotMessage != nil && latestBot != nil && latestBot.Version > l.lastBotMessage.Version {
		msg, ok := l.actuator.WaitForEdit(l.cfg.ChatID, l.lastBotMessage.MessageID, l.lastBotMessage.Version, l.cfg.ActWaitTimeout)
		if !ok {
			return fmt.Errorf("actor: expected message %d to have been edited to version %d, but WaitForEdit timed out",
				l.lastBotMessage.MessageID, latestBot.Version)
		}
		l.lastBotMessage = msg
	}
	return nil
}

// resolveClickTarget finds the (row, col) of the action labelled label on
// l.lastBotMessage's raw action grid — the loop's only way to recover a
// platform-native click target from an observe.AvailableAction, since
// AvailableAction's own ID is opaque by design. See refreshRawBotMessage's
// scoping note for what this cannot resolve.
func (l *Loop) resolveClickTarget(label string) (row, col int, found bool) {
	if l.lastBotMessage == nil {
		return 0, 0, false
	}
	for r, cols := range l.lastBotMessage.Actions {
		for c, a := range cols {
			if a.Label == label {
				return r, c, true
			}
		}
	}
	return 0, 0, false
}

// actOn validates (for ProposeClick, ProposeSendText's content rules, and
// ProposeTaskDone's evidence-defined criteria) and, if valid, acts on
// proposal. It returns whether the iteration progressed (acted with an
// observable effect, or concluded the task) — the signal Loop.RunTask's
// non-progress detection uses — plus the recorded ActionOutcome and
// ValidationOutcome, and, when a new observation was actually produced
// (ProposeSendText/ProposeClick that reached the platform), the post-action
// observation RunTask's own evidence-defined-completion check evaluates
// task.Criteria against. The returned error is reserved for genuinely
// unexpected failures (an Actuator I/O error, an internal CampaignState
// guard violation, a Criteria/content-rule Predicate error); an invalid or
// unresolvable proposal is reported through ActionOutcome, never as an
// error, per the plan's "invalid proposals are recorded and re-prompted,
// never mutated".
func (l *Loop) actOn(ctx context.Context, task goal.Task, obs *observe.Observation, p Proposal) (progressed bool, action ActionOutcome, validation ValidationOutcome, postObs *observe.Observation, err error) {
	taskID := task.ID
	switch p.Kind {
	case ProposeTaskDone:
		// Evidence-defined completion (spec/ideas/evidence-defined-completion.md):
		// when the task declares Criteria, the actor's own task-done claim
		// is checked against them before being trusted. A premature claim
		// (criteria not yet met) is recorded exactly like any other invalid
		// proposal — never accepted, never silently dropped — so the task
		// continues and the existing ai-navigation-failure classification
		// (campaign.Assemble's navigationFailureEvidence) applies unchanged
		// if the task later ends up Failed/Blocked. A nil Criteria leaves
		// this proposal kind's pre-existing, unconditional-accept behaviour
		// untouched.
		if task.Criteria != nil {
			met, cerr := task.Criteria(ctx, *obs)
			if cerr != nil {
				return false, ActionOutcome{}, ValidationOutcome{}, nil, fmt.Errorf("actor: evaluate criteria: %w", cerr)
			}
			if !met {
				return false, ActionOutcome{Kind: ActionSkippedInvalid, Detail: "task-done proposed before evidence-defined criteria are met"}, ValidationOutcome{}, nil, nil
			}
		}
		if err := l.campaign.Complete(taskID); err != nil {
			return false, ActionOutcome{Kind: ActionResolutionFailed, Detail: err.Error()}, ValidationOutcome{}, nil, nil
		}
		return true, ActionOutcome{Kind: ActionTaskCompleted}, ValidationOutcome{}, nil, nil

	case ProposeGiveUp:
		if err := l.campaign.Fail(taskID); err != nil {
			return false, ActionOutcome{Kind: ActionResolutionFailed, Detail: err.Error()}, ValidationOutcome{}, nil, nil
		}
		return true, ActionOutcome{Kind: ActionTaskGivenUp}, ValidationOutcome{}, nil, nil

	case ProposeSendText:
		if strings.TrimSpace(p.Text) == "" {
			return false, ActionOutcome{Kind: ActionSkippedInvalid, Detail: "empty text proposal"}, ValidationOutcome{}, nil, nil
		}
		// Proposal content constraints (spec/ideas/proposal-content-constraints.md):
		// a violating text proposal is blocked before it ever reaches the
		// bot — recorded, never submitted, and re-prompted exactly like any
		// other invalid proposal.
		if rules := goal.EffectiveContentRules(l.goalDef, task); !rules.Empty() {
			ok, reason, cerr := rules.Check(ctx, p.Text)
			if cerr != nil {
				return false, ActionOutcome{}, ValidationOutcome{}, nil, fmt.Errorf("actor: check content rules: %w", cerr)
			}
			if !ok {
				return false, ActionOutcome{Kind: ActionBlockedConstraintViolation, Detail: reason}, ValidationOutcome{}, nil, nil
			}
		}
		if err := l.actuator.SubmitText(l.cfg.ChatID, l.cfg.User, p.Text); err != nil {
			return false, ActionOutcome{}, ValidationOutcome{}, nil, fmt.Errorf("actor: submit text: %w", err)
		}
		effect, freshObs, err := l.observedEffect(obs)
		if err != nil {
			return false, ActionOutcome{}, ValidationOutcome{}, nil, err
		}
		if !effect {
			return false, ActionOutcome{Kind: ActionExecutedNoEffect}, ValidationOutcome{}, freshObs, nil
		}
		return true, ActionOutcome{Kind: ActionExecuted}, ValidationOutcome{}, freshObs, nil

	case ProposeClick:
		result, verr := l.engine.Validate(observe.ActionProposal{ObservationSequence: p.ObservationSequence, ActionID: p.ActionID})
		if verr != nil {
			return false, ActionOutcome{}, ValidationOutcome{}, nil, fmt.Errorf("actor: validate: %w", verr)
		}
		validation = ValidationOutcome{Checked: true, Verdict: result.Verdict, Reason: result.Reason}
		if result.Verdict != observe.VerdictFresh {
			return false, ActionOutcome{Kind: ActionSkippedInvalid, Detail: result.Reason}, validation, nil, nil
		}

		row, col, found := l.resolveClickTarget(result.Current.Label)
		if !found {
			detail := fmt.Sprintf("no action labelled %q found on the current message", result.Current.Label)
			return false, ActionOutcome{Kind: ActionResolutionFailed, Detail: detail}, validation, nil, nil
		}
		targetMessageID := l.lastBotMessage.MessageID
		data := l.lastBotMessage.Actions[row][col].ID
		if err := l.actuator.SubmitClick(l.cfg.ChatID, l.cfg.User, data, targetMessageID); err != nil {
			return false, ActionOutcome{}, validation, nil, fmt.Errorf("actor: submit click: %w", err)
		}
		effect, freshObs, err := l.observedEffect(obs)
		if err != nil {
			return false, ActionOutcome{}, validation, nil, err
		}
		if !effect {
			return false, ActionOutcome{Kind: ActionExecutedNoEffect}, validation, freshObs, nil
		}
		return true, ActionOutcome{Kind: ActionExecuted}, validation, freshObs, nil

	default:
		detail := fmt.Sprintf("unknown proposal kind %v", p.Kind)
		return false, ActionOutcome{Kind: ActionSkippedInvalid, Detail: detail}, ValidationOutcome{}, nil, nil
	}
}

// observedEffect re-observes after acting and reports whether the bot
// reacted with a semantic effect: a new bot message, or an existing bot
// message whose Text or available-action labels actually differ from
// preAction (the Observation the loop acted from). It deliberately ignores
// a change whose Actor is the user — submitting text or a click always
// adds the actor's own message/action to the journal, which would
// otherwise always look like "an effect" even when the bot never responded
// at all, defeating non-progress detection.
//
// It deliberately does NOT stop at "some observe.Change exists for a bot
// message", the way an earlier version of this method did. observe.Engine's
// diff keys ChangeMessageEdited off Version alone (see observe/engine.go's
// diff): a bot that re-edits a message in place with byte-identical text and
// the same action labels — the arena trap behind
// https://github.com/chatwright/runtime-go/issues/2, where a model re-clicks
// an already-activated button and the bot's handler harmlessly re-renders
// the same screen — still bumps Version, so it still produces a
// ChangeMessageEdited. That Change is correct, and stays exactly as-is on
// the Observation: observe's Changes feed is a truthful record of what
// moved, never a judgement about whether it mattered (see observe.Change's
// doc comment). But treating "a Change exists" as "progress" here let a
// model click the same button ten extra times without ever tripping
// NonProgressLimit, because every idempotent re-edit looked identical to a
// real one. semanticallyEqualMessage is what tells the two apart: a real
// content change is progress, a content-identical re-render is not — so
// the two concerns (observe's truthful Changes vs. the loop's own PROGRESS
// judgement) stay distinct instead of collapsing into "any Change means
// progress".
//
// It also refreshes l.lastBotMessage, so the loop's very next iteration (or
// the next click resolution within this same act) sees up-to-date raw
// message state.
//
// It returns the freshly observed Observation alongside the effect
// verdict — the same one RunTask's evidence-defined-completion check
// evaluates goal.Task.Criteria against, so criteria are judged from
// exactly the state this method already re-observed, never a redundant
// extra Observe call.
func (l *Loop) observedEffect(preAction *observe.Observation) (bool, *observe.Observation, error) {
	obs, err := l.observeAndSync()
	if err != nil {
		return false, nil, err
	}

	preByID := make(map[string]observe.VisibleMessage, len(preAction.Messages))
	for _, m := range preAction.Messages {
		preByID[m.ID] = m
	}

	for _, ch := range obs.Changes {
		if ch.Actor != observe.ActorBot {
			continue
		}
		curMsg, found := findMessageByID(obs.Messages, ch.MessageID)
		if !found {
			// Defensive: a bot Change naming a message no longer present is not
			// one this loop can dismiss as a no-op re-render.
			return true, obs, nil
		}
		prevMsg, hadPrev := preByID[ch.MessageID]
		if !hadPrev || !semanticallyEqualMessage(prevMsg, curMsg) {
			return true, obs, nil
		}
	}
	return false, obs, nil
}

// findMessageByID returns the message with the given id from messages, and
// whether it was found.
func findMessageByID(messages []observe.VisibleMessage, id string) (observe.VisibleMessage, bool) {
	for _, m := range messages {
		if m.ID == id {
			return m, true
		}
	}
	return observe.VisibleMessage{}, false
}

// semanticallyEqualMessage reports whether a and b — two observations of
// the same logical message, at possibly different Versions — carry the
// same user-visible content: identical text and the same action labels in
// the same layout. It deliberately compares Actions by Label, never by ID:
// an AvailableAction.ID encodes the owning message's version (see
// observe/engine.go's availableActionID), so it changes on every edit by
// design — comparing IDs would report every re-render as a change, which
// is exactly the false "progress" signal this function exists to avoid.
func semanticallyEqualMessage(a, b observe.VisibleMessage) bool {
	if a.Text != b.Text {
		return false
	}
	if len(a.Actions) != len(b.Actions) {
		return false
	}
	for i := range a.Actions {
		if a.Actions[i].Label != b.Actions[i].Label {
			return false
		}
	}
	return true
}
