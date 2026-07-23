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
func (l *Loop) RunTask(ctx context.Context, taskID string) (TaskResult, error) {
	if _, ok := l.taskByID(taskID); !ok {
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
	for {
		if l.campaign.Stopped() {
			status, _ := l.campaign.TaskStatus(taskID)
			return TaskResult{TaskID: taskID, Status: status, Stopped: true}, nil
		}

		obs, err := l.observeAndSync()
		if err != nil {
			return TaskResult{}, err
		}

		task, _ := l.taskByID(taskID)
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

		progressed, action, validation, err := l.actOn(taskID, obs, proposal)
		if err != nil {
			return TaskResult{}, err
		}
		event.Validation = validation
		event.Action = action
		l.events = append(l.events, event)

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
			return TaskResult{TaskID: taskID, Status: status, Stopped: l.campaign.Stopped()}, nil
		}

		if progressed {
			nonProgressStreak = 0
		} else {
			nonProgressStreak++
			if nonProgressStreak >= l.cfg.NonProgressLimit {
				_ = l.campaign.Abort() // already-stopped race is fine to ignore: Abort is a no-op then.
				status, _ := l.campaign.TaskStatus(taskID)
				return TaskResult{TaskID: taskID, Status: status, Stopped: true, NonProgress: true}, nil
			}
		}
	}
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

// actOn validates (for ProposeClick) and, if valid, acts on proposal. It
// returns whether the iteration progressed (acted with an observable
// effect, or concluded the task) — the signal Loop.RunTask's non-progress
// detection uses — plus the recorded ActionOutcome and ValidationOutcome.
// The returned error is reserved for genuinely unexpected failures (an
// Actuator I/O error, an internal CampaignState guard violation); an
// invalid or unresolvable proposal is reported through ActionOutcome, never
// as an error, per the plan's "invalid proposals are recorded and
// re-prompted, never mutated".
func (l *Loop) actOn(taskID string, obs *observe.Observation, p Proposal) (progressed bool, action ActionOutcome, validation ValidationOutcome, err error) {
	switch p.Kind {
	case ProposeTaskDone:
		if err := l.campaign.Complete(taskID); err != nil {
			return false, ActionOutcome{Kind: ActionResolutionFailed, Detail: err.Error()}, ValidationOutcome{}, nil
		}
		return true, ActionOutcome{Kind: ActionTaskCompleted}, ValidationOutcome{}, nil

	case ProposeGiveUp:
		if err := l.campaign.Fail(taskID); err != nil {
			return false, ActionOutcome{Kind: ActionResolutionFailed, Detail: err.Error()}, ValidationOutcome{}, nil
		}
		return true, ActionOutcome{Kind: ActionTaskGivenUp}, ValidationOutcome{}, nil

	case ProposeSendText:
		if strings.TrimSpace(p.Text) == "" {
			return false, ActionOutcome{Kind: ActionSkippedInvalid, Detail: "empty text proposal"}, ValidationOutcome{}, nil
		}
		if err := l.actuator.SubmitText(l.cfg.ChatID, l.cfg.User, p.Text); err != nil {
			return false, ActionOutcome{}, ValidationOutcome{}, fmt.Errorf("actor: submit text: %w", err)
		}
		effect, err := l.observedEffect(obs)
		if err != nil {
			return false, ActionOutcome{}, ValidationOutcome{}, err
		}
		if !effect {
			return false, ActionOutcome{Kind: ActionExecutedNoEffect}, ValidationOutcome{}, nil
		}
		return true, ActionOutcome{Kind: ActionExecuted}, ValidationOutcome{}, nil

	case ProposeClick:
		result, verr := l.engine.Validate(observe.ActionProposal{ObservationSequence: p.ObservationSequence, ActionID: p.ActionID})
		if verr != nil {
			return false, ActionOutcome{}, ValidationOutcome{}, fmt.Errorf("actor: validate: %w", verr)
		}
		validation = ValidationOutcome{Checked: true, Verdict: result.Verdict, Reason: result.Reason}
		if result.Verdict != observe.VerdictFresh {
			return false, ActionOutcome{Kind: ActionSkippedInvalid, Detail: result.Reason}, validation, nil
		}

		row, col, found := l.resolveClickTarget(result.Current.Label)
		if !found {
			detail := fmt.Sprintf("no action labelled %q found on the current message", result.Current.Label)
			return false, ActionOutcome{Kind: ActionResolutionFailed, Detail: detail}, validation, nil
		}
		targetMessageID := l.lastBotMessage.MessageID
		data := l.lastBotMessage.Actions[row][col].ID
		if err := l.actuator.SubmitClick(l.cfg.ChatID, l.cfg.User, data, targetMessageID); err != nil {
			return false, ActionOutcome{}, validation, fmt.Errorf("actor: submit click: %w", err)
		}
		effect, err := l.observedEffect(obs)
		if err != nil {
			return false, ActionOutcome{}, validation, err
		}
		if !effect {
			return false, ActionOutcome{Kind: ActionExecutedNoEffect}, validation, nil
		}
		return true, ActionOutcome{Kind: ActionExecuted}, validation, nil

	default:
		detail := fmt.Sprintf("unknown proposal kind %v", p.Kind)
		return false, ActionOutcome{Kind: ActionSkippedInvalid, Detail: detail}, ValidationOutcome{}, nil
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
func (l *Loop) observedEffect(preAction *observe.Observation) (bool, error) {
	obs, err := l.observeAndSync()
	if err != nil {
		return false, err
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
			return true, nil
		}
		prevMsg, hadPrev := preByID[ch.MessageID]
		if !hadPrev || !semanticallyEqualMessage(prevMsg, curMsg) {
			return true, nil
		}
	}
	return false, nil
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
