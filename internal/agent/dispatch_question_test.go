package agent

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
)

// fakeQuestionService is the parent's question service as the
// coordinator sees it (#352): Ask records each request and answers from
// the answers channel — or returns err at once when set — and gives up
// when its context ends.
type fakeQuestionService struct {
	answers chan []question.Answer
	err     error

	mu    sync.Mutex
	asked []question.Request
}

func newFakeQuestionService() *fakeQuestionService {
	return &fakeQuestionService{answers: make(chan []question.Answer)}
}

func (f *fakeQuestionService) Subscribe(context.Context) <-chan pubsub.Event[question.Request] {
	return nil
}

func (f *fakeQuestionService) SubscribeNotifications(context.Context) <-chan pubsub.Event[question.Notification] {
	return nil
}

func (f *fakeQuestionService) Ask(ctx context.Context, req question.Request) ([]question.Answer, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	select {
	case answers := <-f.answers:
		return answers, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeQuestionService) Answer([]question.Answer) bool { return false }

func (f *fakeQuestionService) Cancel() bool { return false }

// requests returns a copy of the requests Ask received.
func (f *fakeQuestionService) requests() []question.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]question.Request(nil), f.asked...)
}

var _ question.Service = (*fakeQuestionService)(nil)

// dispatchedQuestion is the question the tests' dispatched agent asks.
func dispatchedQuestion() QuestionRequest {
	return QuestionRequest{
		ID: "batch-1",
		Questions: []question.Question{{
			ID:          "q-1",
			Type:        question.TypeFreeText,
			Text:        "Which database?",
			Description: "The schema differs per engine.",
		}},
	}
}

// The question tool reaches a dispatched agent only while its parent is
// interactive (#352), through the toolchain's own question service, and
// a question tool the user denied stays denied.
func TestDispatchToolchainQuestionToolFollowsInteractive(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		disabled    []string
		wantTool    bool
	}{
		{name: "interactive parent", interactive: true, wantTool: true},
		{name: "non-interactive parent", interactive: false},
		{name: "question tool denied", interactive: true, disabled: []string{tools.QuestionToolName}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newDispatchTestCoordinator(t, testEnv(t))
			c.interactive = tt.interactive
			c.cfg.Config().Options.DisabledTools = tt.disabled

			tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: t.TempDir()})
			require.NoError(t, err)
			defer tc.Close(t.Context())

			names := make(map[string]bool, len(tc.Tools()))
			for _, tool := range tc.Tools() {
				names[tool.Info().Name] = true
			}
			require.Equal(t, tt.wantTool, names[tools.QuestionToolName])
			require.Equal(t, tt.interactive, tc.Questions() != nil,
				"the toolchain's own question service exists exactly while the parent is interactive")
		})
	}
}

// The coordinator's OnInputRequired (#352): an interactive parent puts
// the dispatched agent's question in front of its user, labeled with the
// dispatch's @handle, and returns the user's answers; a parent with no
// interactive user — or no question service — answers at once with the
// refusal, without asking anyone; a dismissed question is answered as
// declined, so the agent carries on.
func TestAnswerDispatchQuestion(t *testing.T) {
	t.Parallel()

	answered := []question.Answer{{QuestionID: "q-1", FillInText: "postgres"}}
	tests := []struct {
		name        string
		interactive bool
		noService   bool
		askErr      error
		want        QuestionAnswer
		wantAsked   bool
	}{
		{
			name:        "interactive parent asks its user",
			interactive: true,
			want:        QuestionAnswer{Answers: answered},
			wantAsked:   true,
		},
		{
			name: "non-interactive parent refuses",
			want: QuestionAnswer{Answers: []question.Answer{{QuestionID: "q-1", FillInText: NoInteractiveUserAnswer}}},
		},
		{
			name:        "no question service refuses",
			interactive: true,
			noService:   true,
			want:        QuestionAnswer{Answers: []question.Answer{{QuestionID: "q-1", FillInText: NoInteractiveUserAnswer}}},
		},
		{
			name:        "dismissed question is declined",
			interactive: true,
			askErr:      question.ErrCancelled,
			want:        QuestionAnswer{Answers: []question.Answer{{QuestionID: "q-1", FillInText: dispatchQuestionDeclinedAnswer}}},
			wantAsked:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeQuestionService()
			fake.err = tt.askErr
			c := &coordinator{interactive: tt.interactive, questions: fake}
			if tt.noService {
				c.questions = nil
			}
			req := dispatchedQuestion()

			type result struct {
				answer QuestionAnswer
				err    error
			}
			done := make(chan result, 1)
			go func() {
				answer, err := c.answerDispatchQuestion(t.Context(), dispatchRun{kill: &dispatchKill{}}, "tester", req)
				done <- result{answer, err}
			}()
			if tt.wantAsked && tt.askErr == nil {
				fake.answers <- answered
			}
			res := <-done
			require.NoError(t, res.err)
			require.Equal(t, tt.want, res.answer)

			asked := fake.requests()
			if !tt.wantAsked {
				require.Empty(t, asked, "no one is asked when no one can answer")
				return
			}
			require.Len(t, asked, 1)
			require.Equal(t, "@tester: Which database?", asked[0].Questions[0].Text,
				"the parent's user sees which dispatch is asking")
			require.Equal(t, "q-1", asked[0].Questions[0].ID, "the answer must name the agent's own question")
			require.Equal(t, "Which database?", req.Questions[0].Text, "labeling must not touch the caller's request")
		})
	}
}

// A kill ends the wait on the parent's user (#352): hard_timeout keeps
// running while a question is parked, and when it fires the coordinator
// returns the kill reason, which the transport cancels the parked task
// with.
func TestAnswerDispatchQuestionKillEndsWait(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fake := newFakeQuestionService()
		c := &coordinator{interactive: true, questions: fake}
		run := dispatchRun{kill: &dispatchKill{}}

		done := make(chan error, 1)
		go func() {
			_, err := c.answerDispatchQuestion(t.Context(), run, "tester", dispatchedQuestion())
			done <- err
		}()

		// The question is on screen and the wait is parked on the user.
		synctest.Wait()
		require.Len(t, fake.requests(), 1)

		run.kill.kill(dispatch.ReasonHardTimeout)
		require.EqualError(t, <-done, dispatch.ReasonHardTimeout)
	})
}

// Dispatched questions take turns (#352): the parent's question service
// holds one pending question at a time, so a second dispatch's question
// waits until the first is answered instead of stranding it.
func TestAnswerDispatchQuestionTakesTurns(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fake := newFakeQuestionService()
		c := &coordinator{interactive: true, questions: fake}

		errs := make(chan error, 2)
		for _, handle := range []string{"first", "second"} {
			go func() {
				_, err := c.answerDispatchQuestion(t.Context(), dispatchRun{kill: &dispatchKill{}}, handle, dispatchedQuestion())
				errs <- err
			}()
		}

		synctest.Wait()
		require.Len(t, fake.requests(), 1, "one dispatched question on screen at a time")

		fake.answers <- []question.Answer{{QuestionID: "q-1", FillInText: "one"}}
		require.NoError(t, <-errs)
		synctest.Wait()
		require.Len(t, fake.requests(), 2, "the next question goes up once the first is answered")

		fake.answers <- []question.Answer{{QuestionID: "q-1", FillInText: "two"}}
		require.NoError(t, <-errs)
	})
}

// questionAskingHost is a dispatch host whose served agent asks one
// question (#352): StreamDispatch hands it to the coordinator's
// OnInputRequired, reports the answer, and completes.
type questionAskingHost struct {
	fakeDispatchHost

	answered chan QuestionAnswer
}

func (h *questionAskingHost) StreamDispatch(ctx context.Context, p DispatchTransportParams) (DispatchTransportOutcome, error) {
	answer, err := p.OnInputRequired(ctx, dispatchedQuestion())
	if err != nil {
		return DispatchTransportOutcome{Status: transportStatusCanceled, Text: err.Error()}, nil
	}
	h.answered <- answer
	return DispatchTransportOutcome{Status: transportStatusCompleted, Text: "done"}, nil
}

// The dispatch path wires questions end to end on the coordinator's side
// (#352): the served dispatch gets the toolchain's question service while
// the parent is interactive, and a question the served agent parks on
// reaches the coordinator through the transport's OnInputRequired —
// asked of the user with the dispatch's handle, or refused with the
// no-interactive-user answer so the agent carries on.
func TestDispatchQuestionReachesParent(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		want        string
	}{
		{name: "interactive parent", interactive: true, want: "postgres"},
		{name: "non-interactive parent", want: NoInteractiveUserAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newGatedDispatchAgent()
			c, _ := newInjectionEnv(t, agent)
			fake := newFakeQuestionService()
			c.interactive = tt.interactive
			c.questions = fake
			host := &questionAskingHost{answered: make(chan QuestionAnswer, 1)}
			c.SetDispatchHost(host)

			if tt.interactive {
				go func() { fake.answers <- []question.Answer{{QuestionID: "q-1", FillInText: "postgres"}} }()
			}
			resp := runDispatchToolCall(t, c.dispatchTool(), DispatchAgentParams{
				Prompt: "set up the schema",
				Branch: "main",
				Handle: "tester",
			})
			require.False(t, resp.IsError, "dispatch failed: %s", resp.Content)

			answer := <-host.answered
			require.Len(t, answer.Answers, 1)
			require.Equal(t, "q-1", answer.Answers[0].QuestionID)
			require.Equal(t, tt.want, answer.Answers[0].FillInText)
			require.Equal(t, tt.interactive, host.lastParams().Questions != nil,
				"the served dispatch watches the toolchain's question service only while the parent is interactive")

			asked := fake.requests()
			if !tt.interactive {
				require.Empty(t, asked)
				return
			}
			require.Len(t, asked, 1)
			require.Equal(t, "@tester: Which database?", asked[0].Questions[0].Text)
		})
	}
}

// labelDispatchQuestion keeps a labeled question inside the question
// service's length limit, cutting on a rune boundary.
func TestLabelDispatchQuestionFitsLimit(t *testing.T) {
	t.Parallel()

	long := ""
	for len(long) < question.MaxQuestionLength {
		long += "é"
	}
	labeled := labelDispatchQuestion(question.Request{Questions: []question.Question{{Text: long}}}, "tester")
	text := labeled.Questions[0].Text
	require.LessOrEqual(t, len(text), question.MaxQuestionLength)
	require.True(t, len(text) > len("@tester: "))
	require.Equal(t, "@tester: ", text[:len("@tester: ")])
	require.Equal(t, "…", text[len(text)-len("…"):])
	require.NoError(t, question.Question{ID: "q", Type: question.TypeFreeText, Text: text, Description: "d"}.Validate())

	unlabeled := labelDispatchQuestion(question.Request{Questions: []question.Question{{Text: "Which?"}}}, "")
	require.Equal(t, "Which?", unlabeled.Questions[0].Text)
}
