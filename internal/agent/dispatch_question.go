package agent

// Questions from dispatched agents (#352): a dispatched agent's question
// tool asks through its own question service, the served executor parks
// the run in input-required, and the parent's transport hands the
// question to the coordinator here. The coordinator puts it in front of
// the parent's user, labeled with the dispatch's @handle, and the answer
// travels back on the same A2A task.

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/question"
)

// dispatchQuestionDeclinedAnswer answers a dispatched agent's question
// the user dismissed instead of answering: the agent carries on rather
// than the dispatch being killed over a closed form.
const dispatchQuestionDeclinedAnswer = "the user declined to answer; proceed with your best judgment"

// answerDispatchQuestion is the coordinator's OnInputRequired (#352). A
// parent with no interactive user answers at once with
// NoInteractiveUserAnswer. Otherwise the question goes to the parent's
// question service — one dispatched question on screen at a time — with
// every question labeled with the dispatch's handle, and the user's
// answers go back. A dismissed question is answered with
// dispatchQuestionDeclinedAnswer. The wait ends with the run: a kill
// (hard timeout, cancel, shutdown) returns an error carrying the kill
// reason, which the transport cancels the parked task with.
func (c *coordinator) answerDispatchQuestion(ctx context.Context, run dispatchRun, handle string, req QuestionRequest) (QuestionAnswer, error) {
	if c.questions == nil || !c.isInteractive() {
		return UnattendedQuestionAnswer(req), nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-run.kill.killed():
			cancel()
		case <-ctx.Done():
		}
	}()

	// The question service holds one pending question at a time, so two
	// dispatches asking together would strand the first; take turns.
	slot := c.dispatchQuestionSlot()
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return QuestionAnswer{}, dispatchQuestionAbort(run, ctx.Err())
	}
	defer func() { <-slot }()

	answers, err := c.questions.Ask(ctx, labelDispatchQuestion(question.Request(req), handle))
	switch {
	case errors.Is(err, question.ErrCancelled):
		return freeTextAnswer(req, dispatchQuestionDeclinedAnswer), nil
	case err != nil:
		return QuestionAnswer{}, dispatchQuestionAbort(run, err)
	}
	return QuestionAnswer{Answers: answers}, nil
}

// dispatchQuestionAbort is the error an unanswered dispatched question
// returns: the run's kill reason when a kill ended the wait — the parked
// task is canceled with it — and err otherwise.
func dispatchQuestionAbort(run dispatchRun, err error) error {
	if reason := run.kill.current(); reason != "" {
		return errors.New(reason)
	}
	return err
}

// dispatchQuestionSlot returns the one-slot semaphore dispatched
// questions take turns on, made on first use: tests build the
// coordinator struct directly.
func (c *coordinator) dispatchQuestionSlot() chan struct{} {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if c.dispatchQuestions == nil {
		c.dispatchQuestions = make(chan struct{}, 1)
	}
	return c.dispatchQuestions
}

// labelDispatchQuestion prefixes every question's text with the asking
// dispatch's @handle, so the parent's user sees who is asking. A text
// the prefix would push past question.MaxQuestionLength is shortened to
// fit, ending in an ellipsis. The questions are copied; req's slice is
// untouched.
func labelDispatchQuestion(req question.Request, handle string) question.Request {
	if handle == "" {
		return req
	}
	prefix := "@" + handle + ": "
	labeled := make([]question.Question, len(req.Questions))
	for i, q := range req.Questions {
		q.Text = prefix + fitQuestionText(q.Text, question.MaxQuestionLength-len(prefix))
		labeled[i] = q
	}
	req.Questions = labeled
	return req
}

// fitQuestionText shortens s to at most limit bytes on a rune boundary,
// ending in an ellipsis when anything was cut. The question service
// measures text in bytes.
func fitQuestionText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	cut := max(limit-len(ellipsis), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
