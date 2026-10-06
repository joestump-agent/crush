#!/bin/sh
# Run the a2a Technology Compatibility Kit against a locally served
# Crush harness (#363).
#
# Usage: scripts/run_tck.sh [path-to-a2a-tck-clone]
#
# The TCK clone defaults to TCK_PATH or ~/src/a2a-tck, and must be a
# checkout of a2aproject/a2a-tck; a checkout at any other commit is
# allowed but warned about, since the expected-failure list below is
# pinned to the recorded commit. The TCK's virtualenv is created with
# `uv sync` on first run — never `curl | sh`.
#
# The harness (internal/a2a/tck) serves the production host through the
# loopback proxy; the script starts it, waits for the well-known card,
# runs the JSON-RPC suite at the must level and in full, and fails when
# a test fails (or passes) outside the expected-deviation list recorded
# in internal/a2a/doc.go.

set -eu

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_dir=$(CDPATH= cd "$script_dir/.." && pwd)

tck_commit="263b9cfaf16a554bdfb166a7ba5b67716e946349"
tck_dir="${1:-${TCK_PATH:-$HOME/src/a2a-tck}}"
port="${TCK_PORT:-9911}"
base_url="http://127.0.0.1:$port"
reports="$tck_dir/reports"

if [ ! -d "$tck_dir" ]; then
	echo "error: no a2a-tck checkout at $tck_dir" >&2
	echo "       clone it: git clone https://github.com/a2aproject/a2a-tck $tck_dir" >&2
	exit 1
fi

if [ "$(git -C "$tck_dir" rev-parse HEAD)" != "$tck_commit" ]; then
	echo "warning: $tck_dir is not at the recorded TCK commit $tck_commit;" >&2
	echo "         the expected-failure list may not match this checkout" >&2
fi

if [ ! -x "$tck_dir/.venv/bin/python" ]; then
	echo "== creating the TCK virtualenv (uv sync)"
	(cd "$tck_dir" && uv sync)
fi

echo "== building the TCK harness"
harness=$(mktemp -d)/crush-a2a-tck
(cd "$repo_dir" && go build -o "$harness" ./internal/a2a/tck)

cleanup() {
	[ -n "${harness_pid:-}" ] && kill "$harness_pid" 2>/dev/null || true
	[ -n "${expected_file:-}" ] && rm -f "$expected_file" || true
}
trap cleanup EXIT
trap cleanup INT
trap cleanup TERM

echo "== starting the harness on $base_url"
"$harness" --port "$port" &
harness_pid=$!
card="$base_url/.well-known/agent-card.json"
i=0
until curl -sf -o /dev/null "$card"; do
	i=$((i + 1))
	if [ "$i" -ge 30 ]; then
		echo "error: the harness card never answered at $card" >&2
		exit 1
	fi
	sleep 1
done

# The tests the must-level suite is allowed to fail — the documented
# deviations from internal/a2a/doc.go: the artifact-content and
# message-response sentinels presume an echo agent (DM-ART-001,
# DM-MSG-001), and the SDK's subscribe framing differs from the TCK's
# expectations (STREAM-SUB-003, STREAM-SUB-004). A failure outside this
# list, or an expected one that now passes, fails the run.
expected='tests/compatibility/core_operations/test_artifacts.py::TestTextArtifact::test_task_has_text_artifact
tests/compatibility/core_operations/test_artifacts.py::TestFileArtifact::test_task_has_file_artifact
tests/compatibility/core_operations/test_artifacts.py::TestFileUrlArtifact::test_task_has_file_url_artifact
tests/compatibility/core_operations/test_artifacts.py::TestDataArtifact::test_task_has_data_artifact
tests/compatibility/core_operations/test_artifacts.py::TestMessageResponse::test_returns_message_with_text_part
tests/compatibility/core_operations/test_requirements.py::test_must_requirement[STREAM-SUB-004-jsonrpc]
tests/compatibility/core_operations/test_task_lifecycle.py::TestSubscribeLifecycle::test_subscribe_rejects_terminal_task
tests/compatibility/jsonrpc/test_sse_streaming.py::TestSseSubscribeToTask::test_subscribe_nonexistent_task_returns_error'

check_report() {
	(cd "$tck_dir" && .venv/bin/python - "$reports/junitreport.xml" "$1" <<'PYEOF'
import sys
import xml.etree.ElementTree as ET

_, report, expected_path = sys.argv

def key(nodeid):
	# Canonical form shared by pytest nodeids (tests/.../mod.py::Class::
	# name[param]) and junit ids (tests...mod.Class::name[param]):
	# separators collapsed to dots, the .py dropped, params stripped.
	s = nodeid.replace("::", ".").replace("/", ".")
	s = s.replace(".py", "")
	return s[: s.index("[")] if s.endswith("]") else s

expected = {key(line) for line in open(expected_path).read().splitlines() if line}
failed = set()
for case in ET.parse(report).iter("testcase"):
	node = list(case)
	if any(child.tag == "failure" for child in node):
		failed.add(key(case.get("classname") + "::" + case.get("name")))

unexpected = sorted(failed - expected)
resolved = sorted(expected - failed)
for name in unexpected:
	print("UNEXPECTED FAILURE:", name)
for name in resolved:
	print("EXPECTED FAILURE NOW PASSES (update the deviation list):", name)
sys.exit(1 if (unexpected or resolved) else 0)
PYEOF
	)
}

run_suite() {
	echo "== running the TCK ($1)"
	shift
	# The suite's own exit status counts its failures; the expected-
	# deviation comparison below is what gates this script. run_tck.py
	# has no positional suite argument — its trailing pytest_args are
	# pytest paths, so only real flags go on this line.
	(cd "$tck_dir" && ./.venv/bin/python run_tck.py --sut-host "$base_url" --transport jsonrpc "$@") || true
}

expected_file=$(mktemp)
printf '%s\n' "$expected" > "$expected_file"

run_suite "must level" --level must
check_report "$expected_file"

run_suite full
check_report "$expected_file"

echo "== TCK run matches the documented deviations"
