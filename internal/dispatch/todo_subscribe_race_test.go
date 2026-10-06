package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

// Subscribing and unsubscribing concurrently with snapshots being reduced
// must not panic: emit's listener delivery and the unsubscribe goroutine's
// channel close race unless both hold the collector mutex (#174).
func TestTodoCollectorSubscribeUnsubscribeRace(t *testing.T) {
	reg := NewAgentRegistry()

	sessions := pubsub.NewBroker[session.Session]()
	collector := NewTodoCollector(reg, sessions)
	collector.Start(t.Context())

	entry := Entry{ID: "dispatch-1", Status: StatusProvisioned}
	reg.Register(entry)
	reg.SetSession(entry.ID, "msg$$call")

	stop := make(chan struct{})
	var emitDone sync.WaitGroup
	emitDone.Add(1)
	go func() {
		defer emitDone.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			sessions.Publish(pubsub.UpdatedEvent, session.Session{
				ID:    "msg$$call",
				Todos: testTodos(),
			})
			time.Sleep(time.Millisecond)
		}
	}()

	var churnDone sync.WaitGroup
	churnDone.Add(1)
	go func() {
		defer churnDone.Done()
		for i := 0; i < 500; i++ {
			ctx, cancel := context.WithCancel(context.Background())
			_ = collector.SubscribeSessionTodos(ctx, "msg$$call")
			cancel()
		}
	}()
	churnDone.Wait()
	close(stop)
	emitDone.Wait()
}
