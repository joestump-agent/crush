// Command tck runs Crush's A2A host against the a2a Technology
// Compatibility Kit (#363).
//
// It starts one dispatched agent served through the production
// ServerFactory — the same executor, middleware and unix-socket host a
// real dispatch gets — backed by a scripted runner (a fixed completion,
// one todo snapshot, a small diff), and fronts the host with a loopback
// TCP proxy the TCK can reach. The proxy serves the agent card at the
// well-known path with its endpoint rewritten to itself, injects the
// host's bearer token on every forwarded call, and stamps the
// dispatch's A2A context on messages that start a task without one.
//
// Run the TCK against it with make tck (scripts/run_tck.sh), or point
// the TCK's runner at the printed base URL by hand:
//
//	go run ./internal/a2a/tck --port 9999
//	./run_tck.py --sut-host http://127.0.0.1:9999 --transport jsonrpc
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
)

// dispatchID is the registry id the harness's scripted dispatch serves
// under; the TCK never sees it, but the card's endpoint path carries
// it.
const dispatchID = "tck"

// definitionID is the agent definition the harness publishes beside the
// dispatch (#580), so the per-route card path has a definition to probe
// by hand: GET /agents/coder/.well-known/agent-card.json. The TCK never
// touches it.
const definitionID = "coder"

// sessionID is the ephemeral session the scripted runs answer within.
const sessionID = "tck-session"

func main() {
	port := flag.Int("port", 9999, "loopback TCP port the TCK-facing proxy listens on")
	dataDir := flag.String("data-dir", "", "data directory for the host's session database (defaults to a temp dir)")
	delay := flag.Duration("run-delay", 0, "how long each scripted run takes, for in-flight state polls")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("a2a-tck ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := *dataDir
	if dir == "" {
		temp, err := os.MkdirTemp("", "crush-tck-*")
		if err != nil {
			log.Fatalf("create temp data dir: %v", err)
		}
		defer os.RemoveAll(temp)
		dir = temp
	}

	factory := a2a.NewServerFactory(dir)
	server, err := factory.StartServer(ctx, a2a.ServerParams{
		DispatchID:  dispatchID,
		SessionID:   sessionID,
		ContextID:   sessionID,
		Runner:      &ScriptedRunner{Text: "TCK scripted answer", Delay: *delay},
		Todos:       &ScriptedTodos{},
		Diff:        ScriptedDiff,
		Name:        "Crush TCK Agent",
		Description: "Crush's dispatched-agent surface, exercised by the a2a TCK",
	})
	if err != nil {
		log.Fatalf("start host: %v", err)
	}
	if err := publishDefinition(ctx, factory); err != nil {
		log.Fatalf("publish definition: %v", err)
	}

	proxy, err := NewProxy(ctx, factory, server, sessionID, *port)
	if err != nil {
		log.Fatalf("start proxy: %v", err)
	}
	slog.Info("TCK harness ready",
		"card", proxy.BaseURL()+wellKnownCardPath,
		"sut_host", proxy.BaseURL(),
		"definition_card", proxy.BaseURL()+a2a.AgentPath(definitionID)+wellKnownCardPath,
		"socket", factory.SocketPath())

	<-ctx.Done()
	_ = proxy.Close()
	_ = factory.Close(context.Background())
	_ = server.Stop(context.Background())
	fmt.Println("a2a-tck harness stopped")
}

// publishDefinition publishes the harness's one agent definition on the
// host (#580): a stable card at /agents/coder, served the way the
// production host serves every definition.
func publishDefinition(ctx context.Context, factory *a2a.ServerFactory) error {
	return factory.PublishAgentDefinition(ctx, agent.AgentDefinitionCard{
		ID:          definitionID,
		Name:        "Coder",
		Description: "An agent that helps with executing coding tasks.",
	})
}
