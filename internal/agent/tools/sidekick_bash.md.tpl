Execute read-only shell commands to inspect the system, the repository, and Crush itself.

<restrictions>
This is a READ-ONLY shell. You cannot write files or mutate system state.
- Only commands on the allowlist below may run; everything else is rejected before execution.
- Output redirection (`>`, `>>`, `&>`) is always blocked, as are write flags like `sed -i`, `find -delete`/`-exec`, and `sort -o`.
- Blocked commands fail with "Sidekick cannot run write commands" — do not retry them; explain the limitation instead.

Allowed commands: {{ .AllowedCommands }}
</restrictions>

<usage_notes>
- Command required, working_dir optional (defaults to current directory)
- Pipes (`|`) and chaining (`;`, `&&`, `||`) are fine as long as every command in the pipeline is on the allowlist
- Command names must be written literally — dynamic names ($CMD, $(...)) are rejected
- Each command runs in an independent shell (no state persistence between calls)
- Output is truncated after {{ .MaxOutputLength }} characters
</usage_notes>
