Send a message to a dispatched agent that is still running, steering it mid-task: the message lands as the agent's next input while it keeps working, and its response appears on the agent's dispatch block in the chat — not as this tool's result.

Address the agent by providing exactly one of `handle` (the "handle" from the dispatch handle) or `session_id` (the "session_id" from the dispatch handle). Use it to course-correct a running agent ("stop writing Rust and use Go"), answer a question it asked in its findings so far, or add constraints you forgot in the original prompt.

You can only message agents dispatched from your own session — another session's agents are invisible to this tool. A finished agent cannot receive messages — task sessions are never continuable. If the agent already finished, dispatch a new one with a prompt that includes the follow-up instead. The tool returns as soon as the message is queued; do not wait for the agent's reply.
