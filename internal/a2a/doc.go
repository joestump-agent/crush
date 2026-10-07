// Package a2a is Crush's coordination layer for dispatching agents over the
// A2A (Agent-to-Agent) protocol, using the official Go SDK
// (github.com/a2aproject/a2a-go/v2).
//
// It replaces the in-process Go call between the main agent and a dispatched
// worktree agent with a protocol boundary: each dispatched agent is described
// by an [AgentCard] and served as an A2A server; the coordinator is an A2A
// client. This unlocks process isolation, remote workers, and third-party
// agents without changing the model-facing DispatchAgent tool.
//
// This package currently implements Phase 1 foundations:
//
//   - BuildAgentCard derives a spec-valid A2A AgentCard from a dispatched
//     agent's config (issue #68).
//   - Executor adapts a Crush SessionAgent to the a2asrv.AgentExecutor
//     interface, mapping the agent run lifecycle onto A2A task states
//     (issue #69).
//
// On top of those foundations, #70 serves each dispatched agent as an
// in-process A2A server and #71 drives it over the protocol:
// [ServerFactory.StartServer] registers the dispatch on a process-wide
// unix-socket host (#346), wires the Executor behind a2asrv, and
// [Resolve] reads a dispatch's card and endpoint back from its registry
// entry — in-memory discovery over the dispatch registry, no network
// hop. [ServerFactory.StreamDispatch] is the client half — prompt out
// as a streaming message, the SSE event stream back to its terminal
// state, the artifact (diff) and the agent's final text assembled into
// the transport outcome the coordinator maps onto its DispatchResult.
// See the A2A Coordination epic (#67) for the full plan.
//
// The SDK's core type package is imported as a2aspec throughout to avoid
// colliding with this package's own name.
//
// # Adding an extension
//
// Declared, statically typed metadata extensions (#359) are registered in
// one place, ext.go's registry ([Register]): define the payload type next to
// [agent.DispatchTransportOutcome] in internal/agent (the a2a package
// imports agent, not the other way around), then add an [Extension] entry
// with mustRegister in ext.go's init, carrying the payload's Go type and
// its JSON Schema. Registration advertises the extension — URI, description,
// schema in the card params — on every built agent card automatically; the
// server emits values with [Encode], the client activates the card-declared
// extensions it has registered and decodes their metadata with
// [Decode]/[DecodeValue] on each TaskStatusUpdateEvent, consuming decoded
// payloads onto the transport outcome. Unknown or undeclared metadata keys
// are logged and dropped, never fatal.
//
// # A2A TCK deviations
//
// The a2a TCK (#363) runs against the host through internal/a2a/tck's
// harness, which fronts the unix socket with a loopback TCP proxy. The
// intentional deviations from full spec conformance, and the TCK suites
// they explain:
//
//   - JSON-RPC only. Only the JSON-RPC binding is served; the TCK's
//     gRPC and HTTP+JSON suites (tests/compatibility/grpc,
//     tests/compatibility/http_json) are out of scope, run with
//     --transport jsonrpc. Remote transports are #358.
//   - Bearer-authenticated. The card declares the crush-bearer HTTP
//     security scheme as required (#357), and the host rejects every
//     call without the token before the handler runs. The TCK knows
//     nothing of the credential, so the harness's proxy injects the
//     host's per-process token on every forwarded call; a
//     token-missing call is only observable in-process
//     (TestHostRejectsUnauthenticated). Affects the TCK's
//     authentication-related expectations in tests/compatibility/core_operations.
//   - One session per host. Every task a host serves shares the
//     dispatch's ephemeral session (#350); a second message to a busy
//     task fails rather than forking (#351). TCK tests that create
//     independent task contexts on one endpoint
//     (tests/compatibility/core_operations task-history cases) see the
//     shared context instead.
//   - One context per dispatch. The A2A context is the dispatch's
//     session (#350): the executor runs only messages on the
//     dispatch's own bound context and rejects the task otherwise.
//     Crush's dispatch client always sends that context; the TCK's
//     new-task messages send none, and the context the SDK mints for
//     them would be rejected. The harness's proxy stamps the
//     dispatch's context on a message that starts a task without one,
//     the way it injects the bearer token; a message naming a task or
//     its own context is forwarded untouched, so the TCK's
//     context-inference and mismatch checks (CORE-MULTI-002a,
//     CORE-MULTI-005, CORE-MULTI-006) see the host's own answers.
//
// # TCK results and known deviations
//
// The must-level JSON-RPC suite runs green except for the clusters
// below (TCK commit 263b9cfa, a2a-go v2.5.0); each names the TCK test
// IDs it explains.
//
//   - Artifact content (tests/compatibility/core_operations/
//     test_artifacts.py, DM-ART-001). The executor's completion carries
//     Crush's real dispatch work product — the chunked text/x-diff
//     "diff" artifact and the typed "dispatch-result" artifact
//     (#361) — with the agent's answer as the terminal status text.
//     The TCK's artifact tests send sentinel prompts and assert
//     canonical echo-agent content (a "Generated text content" text
//     part, an output.txt file part, a {key,value} data part), which a
//     dispatched-agent surface neither should nor can synthesize. The
//     structural requirements these tests exist for — every artifact
//     carries an artifactId and typed parts under oneof semantics —
//     pass (DM-PART-001, DM-TASK-001).
//   - Message responses (tests/compatibility/core_operations/
//     test_artifacts.py::TestMessageResponse, DM-MSG-001). A served
//     dispatch always answers with a Task: a dispatched run is a
//     long-running unit with status transitions and artifacts, never a
//     bare message reply, so the TCK's "respond with a Message"
//     sentinel is answered with a completed Task carrying the same
//     text.
//   - Subscribe error framing (tests/compatibility/jsonrpc/
//     test_sse_streaming.py::TestSseSubscribeToTask::
//     test_subscribe_nonexistent_task_returns_error,
//     tests/compatibility/core_operations/test_requirements.py
//     STREAM-SUB-004). Subscribing to a nonexistent task returns
//     TaskNotFoundError (-32001) correctly, but the SDK's JSON-RPC
//     handler writes the SSE headers before resolving the
//     subscription, so the error arrives as an in-stream error event
//     rather than a plain JSON-RPC error body — which the TCK's
//     streaming client reads as a successfully opened stream.
//
// Three of the TCK's findings were fixed rather than accepted, and their
// upstream shapes are worth reporting to the a2a-go project (filing
// there needs Joe's approval, so they are tracked here):
//
//   - SecurityRequirement marshaling (CARD-STRUCT-001): the SDK
//     marshals each security requirement's per-scheme scope list as a
//     bare JSON array, but the spec's Security Requirement object
//     wants StringList objects ({"list": [...]}). The TCK proxy
//     normalizes the served card on the way out
//     (internal/a2a/tck/proxy.go's normalizeCardJSON); the SDK type
//     round-trips fine internally, so only spec-facing consumers see
//     it.
//   - Status timestamps (DM-SERIAL-003): the SDK stamps
//     NewStatusUpdateEvent with a local-zone time.Now(); the wire
//     format wants ISO 8601 with a Z suffix. Every status event the
//     executor emits — the run's lifecycle and the steer path's
//     working/rejected/completed/failed progression alike — is built
//     through the statusEvent helper, which normalizes the timestamp
//     to UTC.
//   - Subscribe on a terminal task (STREAM-SUB-003): the SDK
//     resubscribes to a finished task by replaying its recorded
//     terminal events, which the TCK rejects. Since #528 the suite's
//     second-and-later messages on the shared context are served as
//     steers, and a steer task never opens a stream of its own, so the
//     scenario's task has no recorded stream and SubscribeToTask on it
//     errors — which the TCK accepts. Resubscribing to a task that did
//     stream still replays (SDK behavior, unchanged).
package a2a
