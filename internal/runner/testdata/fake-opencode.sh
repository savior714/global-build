#!/bin/sh
# Fake OpenCode CLI for global-build runner tests.
#
# Behavior is selected via GB_FAKE_SCENARIO. Controlled NDJSON goes to stdout;
# the task body received on stdin is copied to GB_FAKE_STDIN_COPY; every
# invocation appends one line to GB_FAKE_CALLS so tests can count attempts.
# Optional GB_FAKE_ARGS_LOG records the exact argv of each worker/finalizer call.
#
# Like real OpenCode, the fake accepts `--dir <path>` and performs its
# repository-relative work (git lookups, mutations, commits, dirty state)
# inside that directory. Call-count/stdin bookkeeping stays in the caller's
# CWD because those fixture files live outside the disposable worktree.
set -u

# BUILD CLI compatibility preflight runs `<bin> --version` before execution.
# Version probes are not worker attempts and therefore must not touch the
# attempt-count/stdin fixtures.
if [ "${1:-}" = '--version' ]; then
  printf '%s\n' "${GB_FAKE_VERSION:-1.18.25}"
  exit 0
fi

echo run >> "$GB_FAKE_CALLS"
count=$(grep -c . "$GB_FAKE_CALLS")
if [ -n "${GB_FAKE_ARGS_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$GB_FAKE_ARGS_LOG"
fi
if [ -n "${GB_FAKE_STDIN_COPY:-}" ] && [ "$count" -eq 1 ]; then
  cat > "$GB_FAKE_STDIN_COPY"
else
  cat > /dev/null
fi

dir=''
agent=''
session=''
prev=''
for arg in "$@"; do
  case "$prev" in
    --dir) dir=$arg; prev='' ;;
    --agent) agent=$arg; prev='' ;;
    --session) session=$arg; prev='' ;;
    *)
      case "$arg" in
        --dir|--agent|--session) prev=$arg ;;
        *) prev='' ;;
      esac
      ;;
  esac
done

emit() { printf '%s\n' "$1"; }

step_start='{"type":"step_start","timestamp":1,"sessionID":"ses_1","part":{"id":"p1","type":"step-start"}}'
text_progress='{"type":"text","timestamp":2,"sessionID":"ses_1","part":{"id":"p2","type":"text","text":"Working on the prepared task.","time":{"start":1,"end":2}}}'
tool_use='{"type":"tool_use","timestamp":3,"sessionID":"ses_1","part":{"id":"p3","type":"tool","tool":"bash","state":{"status":"completed","input":{},"output":"ok"}}}'
err_transient='{"type":"error","timestamp":8,"sessionID":"ses_1","error":{"name":"UnknownError","data":{"message":"502 Bad Gateway upstream connect error"}}}'
err_auth='{"type":"error","timestamp":8,"sessionID":"ses_1","error":{"name":"ProviderAuthError","data":{"message":"authentication required"}}}'

commit_candidate_in() {
  printf 'mutation\n' >> "$1"
  git add -A >/dev/null 2>&1
  git commit -qm "candidate change" >/dev/null 2>&1
}

scenario="${GB_FAKE_SCENARIO:-}"

# Scenarios that touch the repository must run inside the disposable worktree
# real OpenCode would have been pointed at via --dir.
case "$scenario" in
  complete|dirty_complete|outside_surface|continuable|identity_mismatch|transient_then_success|malformed_then_finalized|malformed_then_malformed|malformed_no_session|non_git_proof|failing_test_blocked)
    if [ -z "$dir" ]; then
      echo "fake opencode: scenario '$scenario' requires --dir <worktree>" >&2
      exit 3
    fi
    cd -- "$dir" || { echo "fake opencode: cannot cd into '$dir'" >&2; exit 3; }
    ;;
esac

case "$scenario" in
  complete)
    # This fixture proves the successful BUILD/candidate boundary only. Keep its
    # assistant output terminal-only so repository/CLI tests do not also depend
    # on multi-text selection semantics; those are tested directly in opencode.
    emit "$step_start"
    commit_candidate_in docs/notes.txt
    emit "$tool_use"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  dirty_complete)
    emit "$step_start"
    emit "$text_progress"
    commit_candidate_in docs/notes.txt
    emit "$tool_use"
    printf 'uncommitted leftover\n' >> docs/notes.txt
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  outside_surface)
    emit "$step_start"
    emit "$text_progress"
    commit_candidate_in src/main.go
    emit "$tool_use"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  continuable)
    emit "$step_start"
    emit "$text_progress"
    base=$(git rev-parse HEAD)
    printf 'partial uncommitted work\n' >> docs/notes.txt
    emit "$tool_use"
    final="RESULT: CONTINUABLE\\nADMITTED_BASE: $base\\nCOMPLETED: scaffolding\\nREMAINING: finish implementation\\nNEXT_ACTION: resume implementation\\nDO_NOT_REOPEN: settled facts\\nVERIFICATION_ALREADY_DONE: go build\\nWORKTREE_STATE: uncommitted edit in docs/notes.txt"
    emit "{\"type\":\"text\",\"timestamp\":4,\"sessionID\":\"ses_1\",\"part\":{\"id\":\"p4\",\"type\":\"text\",\"text\":\"$final\",\"time\":{\"start\":3,\"end\":4}}}"
    ;;

  blocked)
    emit "$step_start"
    emit "$text_progress"
    emit "$tool_use"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: BLOCKED\nBLOCKER: required dependency unavailable\nEVIDENCE: go mod download fails with network unreachable","time":{"start":3,"end":4}}}'
    ;;

  malformed_then_finalized)
    if [ "$count" -eq 1 ]; then
      emit "$step_start"
      commit_candidate_in docs/notes.txt
      emit "$tool_use"
      emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"All PRIMARY PROOF criteria pass: candidate is ready.","time":{"start":3,"end":4}}}'
      exit 0
    fi
    if [ "$agent" != 'global-build-finalizer' ] || [ "$session" != 'ses_1' ]; then
      echo "fake opencode: finalizer must use exact agent/session, got agent=$agent session=$session" >&2
      exit 4
    fi
    emit "$step_start"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  malformed_then_malformed)
    if [ "$count" -eq 1 ]; then
      emit "$step_start"
      commit_candidate_in docs/notes.txt
      emit "$tool_use"
      emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"All PRIMARY PROOF criteria pass: candidate is ready.","time":{"start":3,"end":4}}}'
      exit 0
    fi
    if [ "$agent" != 'global-build-finalizer' ] || [ "$session" != 'ses_1' ]; then
      exit 4
    fi
    emit "$step_start"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"Still malformed prose.","time":{"start":3,"end":4}}}'
    ;;

  malformed_no_session)
    emit '{"type":"step_start","timestamp":1,"sessionID":"","part":{"id":"p1","type":"step-start"}}'
    commit_candidate_in docs/notes.txt
    emit '{"type":"tool_use","timestamp":3,"sessionID":"","part":{"id":"p3","type":"tool","tool":"bash","state":{"status":"completed","input":{},"output":"ok"}}}'
    emit '{"type":"text","timestamp":4,"sessionID":"","part":{"id":"p4","type":"text","text":"All PRIMARY PROOF criteria pass: candidate is ready.","time":{"start":3,"end":4}}}'
    ;;

  no_text)
    emit "$step_start"
    ;; # exits cleanly without any assistant text

  garbage_line)
    emit "$step_start"
    emit 'this is not JSON at all'
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  truncated_json)
    emit "$step_start"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","typ'
    ;;

  transient_then_success)
    if [ "$count" -eq 1 ]; then
      emit "$err_transient"
      exit 1
    fi
    emit "$step_start"
    emit "$text_progress"
    commit_candidate_in docs/notes.txt
    emit "$tool_use"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  substantive_then_transient)
    emit "$step_start"
    emit "$text_progress"
    emit "$err_transient"
    exit 1
    ;;

  tool_then_transient)
    emit "$step_start"
    emit "$tool_use"
    emit "$err_transient"
    exit 1
    ;;

  nontransient_error)
    emit "$step_start"
    emit "$err_auth"
    exit 1
    ;;

  sleep_forever)
    emit "$step_start"
    # Replace the shell so the watchdog kill reaches the sleeper directly
    # (no grandchild holding the stdout pipe open).
    exec sleep 120
    ;;

  progress_then_stall)
    # Emits periodic progress events for a while, then stalls past the
    # ProgressWindow without any further meaningful events. The watchdog must
    # fire on the stall, not on process start jitter.
    emit "$step_start"
    for i in 1 2 3; do
      printf '{"type":"text","timestamp":%d,"sessionID":"ses_1","part":{"id":"p%d","type":"text","text":"working...","time":{"start":%d,"end":%d}}}\n' "$((i*10))" "$i" "$(( (i-1)*10 ))" "$((i*10))"
      sleep 0.2
    done
    # Now stall: no more progress events for well past any reasonable window.
    sleep 5
    emit '{"type":"text","timestamp":99,"sessionID":"ses_1","part":{"id":"p99","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":98,"end":99}}}'
    ;;

  two_commits_complete)
    # Creates TWO commits in the worktree then returns COMPLETE. The runner must
    # reject this because exactly one commit is required.
    emit "$step_start"
    emit "$text_progress"
    printf 'first change\n' >> docs/notes.txt
    git add -A >/dev/null 2>&1
    git commit -qm "first change" >/dev/null 2>&1
    printf 'second change\n' >> docs/notes.txt
    git add -A >/dev/null 2>&1
    git commit -qm "second change" >/dev/null 2>&1
    emit "$tool_use"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  sustained_progress_no_kill)
    # Emits meaningful progress events continuously for longer than any test
    # ProgressWindow would allow, proving the watchdog clock follows the latest
    # event rather than process start time. Must NOT be killed.
    emit "$step_start"
    for i in $(seq 1 30); do
      printf '{"type":"text","timestamp":%d,"sessionID":"ses_1","part":{"id":"p%d","type":"text","text":"still working...","time":{"start":%d,"end":%d}}}\n' "$((i*3))" "$i" "$(((i-1)*3))" "$((i*3))"
      sleep 0.15
    done
    commit_candidate_in docs/notes.txt
    emit "$tool_use"
    emit '{"type":"text","timestamp":99,"sessionID":"ses_1","part":{"id":"p99","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":98,"end":99}}}'
    ;;

  identity_mismatch)
    emit "$step_start"
    emit "$text_progress"
    commit_candidate_in docs/notes.txt
    emit "$tool_use"
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    # Tamper with the registration the runner recorded: move this worktree
    # elsewhere so nothing is registered at the expected path anymore.
    commondir=$(git rev-parse --git-common-dir)
    mainrepo=$(dirname "$commondir")
    git -C "$mainrepo" worktree move "$PWD" "${PWD}.moved" >/dev/null 2>&1 || mv "$PWD" "${PWD}.moved"
    ;;

  non_git_proof)
    # Proves the worker can execute ordinary non-Git shell commands (e.g. go test,
    # echo, cat) as part of PRIMARY PROOF. The fake simulates a Go test passing
    # by writing a marker file and emitting COMPLETE.
    emit "$step_start"
    emit "$text_progress"
    # Simulate: worker ran `go test ./...` (ordinary non-Git shell) which passed.
    printf 'mutation\n' >> "$dir/docs/notes.txt"
    git add -A >/dev/null 2>&1
    git commit -qm "candidate change" >/dev/null 2>&1
    emit '{"type":"tool_use","timestamp":3,"sessionID":"ses_1","part":{"id":"p3","type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok"}}}'
    emit '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"id":"p4","type":"text","text":"RESULT: COMPLETE\nPRIMARY_PROOF: PASS","time":{"start":3,"end":4}}}'
    ;;

  failing_test_blocked)
    # Proves that when PRIMARY PROOF fails (simulated by a failing "test"),
    # the worker reports BLOCKED rather than COMPLETE.
    emit "$step_start"
    emit "$text_progress"
    emit "$tool_use"
    final='RESULT: BLOCKED\nBLOCKER: go test ./... failed with exit code 1\nEVIDENCE: go test ./... reported FAIL: TestFoo at main.go:5 (unexpected output)'
    emit "{\"type\":\"text\",\"timestamp\":4,\"sessionID\":\"ses_1\",\"part\":{\"id\":\"p4\",\"type\":\"text\",\"text\":\"$final\",\"time\":{\"start\":3,\"end\":4}}}"
    ;;

  *)
    echo "fake opencode: unknown scenario '$scenario'" >&2
    exit 3
    ;;
esac
exit 0
