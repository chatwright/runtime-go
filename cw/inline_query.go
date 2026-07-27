package cw

import (
	"time"

	"chatwright.dev/runtime/platform"
)

// InlineQuery is a lazy expectation for Telegram-style inline-mode results.
// It is obtained through Chat.SendInlineQuery and resolves on Snapshot or
// ResultCount. Inline answers are intentionally separate from chat messages:
// answering a query does not itself post the selected result to a chat.
type InlineQuery struct {
	chat     *Chat
	queryID  string
	resolved bool
	answer   *platform.InlineQueryAnswer
}

type SelectedInlineResult struct {
	chat            *Chat
	inlineMessageID string
	version         int
}

// SendInlineQuery delivers the user's inline query to a platform that supports
// it and returns a lazy answer handle.
func (c *Chat) SendInlineQuery(query string) *InlineQuery {
	c.cw.t.Helper()
	emulator, ok := c.cw.emu.(platform.InlineQueryEmulator)
	if !ok {
		c.cw.t.Fatalf("chatwright: platform %q does not support inline queries", c.cw.platform.Name())
		return &InlineQuery{chat: c}
	}
	c.lastSent = time.Now()
	queryID, err := emulator.SubmitInlineQuery(c.user, query, "")
	if err != nil {
		c.cw.t.Fatalf("chatwright: submit inline query: %v", err)
	}
	result := &InlineQuery{chat: c, queryID: queryID}
	c.cw.t.Cleanup(func() {
		if !result.resolved {
			c.cw.t.Errorf("chatwright: an InlineQuery expectation was never resolved (call Snapshot or ResultCount)")
		}
	})
	return result
}

func (q *InlineQuery) resolve() {
	if q.resolved {
		return
	}
	q.chat.cw.t.Helper()
	emulator, ok := q.chat.cw.emu.(platform.InlineQueryEmulator)
	if !ok {
		q.chat.cw.t.Fatalf("chatwright: platform %q does not support inline queries", q.chat.cw.platform.Name())
		return
	}
	answer, ok := emulator.WaitForInlineQueryAnswer(q.queryID, q.chat.cw.safetyTimeout)
	if !ok {
		q.chat.cw.t.Fatalf(
			"chatwright: expected an answer to inline query %q within %s (safety timeout), but none arrived",
			q.queryID,
			q.chat.cw.safetyTimeout,
		)
		return
	}
	q.answer = answer
	q.resolved = true
}

// ResultCount asserts the number of inline results.
func (q *InlineQuery) ResultCount(want int) *InlineQuery {
	q.chat.cw.t.Helper()
	q.resolve()
	if q.answer != nil && len(q.answer.Results) != want {
		q.chat.cw.t.Errorf("chatwright: inline result count = %d, want %d", len(q.answer.Results), want)
	}
	return q
}

// Snapshot returns a detached copy of the normalized inline answer.
func (q *InlineQuery) Snapshot() platform.InlineQueryAnswer {
	q.chat.cw.t.Helper()
	q.resolve()
	if q.answer == nil {
		return platform.InlineQueryAnswer{}
	}
	snapshot := *q.answer
	snapshot.Results = append([]platform.InlineQueryResult(nil), q.answer.Results...)
	for i := range snapshot.Results {
		snapshot.Results[i].Actions = make([][]platform.Action, len(q.answer.Results[i].Actions))
		for row := range q.answer.Results[i].Actions {
			snapshot.Results[i].Actions[row] = append(
				[]platform.Action(nil),
				q.answer.Results[i].Actions[row]...,
			)
		}
	}
	return snapshot
}

// Select chooses one answered inline result and delivers the platform's
// chosen-result update back to the bot. The selected result remains
// chat-independent and is addressed only by its opaque inline-message ID.
func (q *InlineQuery) Select(index int) *SelectedInlineResult {
	q.chat.cw.t.Helper()
	q.resolve()
	if q.answer == nil || index < 0 || index >= len(q.answer.Results) {
		q.chat.cw.t.Fatalf("chatwright: inline result index %d is out of range", index)
		return &SelectedInlineResult{chat: q.chat}
	}
	emulator, ok := q.chat.cw.emu.(platform.ChosenInlineResultEmulator)
	if !ok {
		q.chat.cw.t.Fatalf(
			"chatwright: platform %q does not support chosen inline results",
			q.chat.cw.platform.Name(),
		)
		return &SelectedInlineResult{chat: q.chat}
	}
	inlineMessageID, err := emulator.SelectInlineQueryResult(
		q.chat.user,
		q.queryID,
		q.answer.Results[index].ID,
	)
	if err != nil {
		q.chat.cw.t.Fatalf("chatwright: select inline result: %v", err)
	}
	return &SelectedInlineResult{
		chat:            q.chat,
		inlineMessageID: inlineMessageID,
	}
}

// WaitForEdit waits for the selected inline message's next in-place edit and
// returns its normalized current content.
func (s *SelectedInlineResult) WaitForEdit() platform.InlineQueryResult {
	s.chat.cw.t.Helper()
	emulator, ok := s.chat.cw.emu.(platform.ChosenInlineResultEmulator)
	if !ok {
		s.chat.cw.t.Fatalf(
			"chatwright: platform %q does not support inline-result edits",
			s.chat.cw.platform.Name(),
		)
		return platform.InlineQueryResult{}
	}
	result, version, ok := emulator.WaitForInlineResultEdit(
		s.inlineMessageID,
		s.version,
		s.chat.cw.safetyTimeout,
	)
	if !ok {
		s.chat.cw.t.Fatalf(
			"chatwright: selected inline result %q was not edited within %s",
			s.inlineMessageID,
			s.chat.cw.safetyTimeout,
		)
		return platform.InlineQueryResult{}
	}
	s.version = version
	return *result
}

func (s *SelectedInlineResult) InlineMessageID() string {
	return s.inlineMessageID
}
