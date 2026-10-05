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
// in-process A2A server: [StartServer] binds a loopback JSON-RPC endpoint,
// wires the Executor behind a2asrv with the agent card at the well-known
// path, and [Resolve] reads a dispatch's card and endpoint back from its
// registry entry — in-memory discovery over the dispatch registry, no
// network hop. The DispatchAgent A2A client + SSE progress (#71) build on
// these: [ServerFactory.StreamDispatch] is the client half — prompt out
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
// one place, [extension registry in ext.go]: define the payload type next to
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
package a2a
