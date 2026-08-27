#!/bin/sh
# Fake OpenCode CLI for global-build runner tests.
#
# Behavior is selected via GB_FAKE_SCENARIO. Controlled NDJSON goes to stdout;
# the task body received on stdin is copied to GB_FAKE_STDIN_COPY; every
# invocation appends one line to GB_FAKE_CALLS so tests can count attempts.
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
  printf '%s\n' "${GB_FAKE_VERSION:-1.18.23}"
  exit 0
fi

echo run >> "$GB_FAKE_CALLS"
count=$(grep -c . "$GB_FAKE_CALLS")
if [ -n "${GB_FAKE_STDIN_COPY:-}" ] && [ "$count" -eq 1 ]; then
  cat > "$GB_FAKE_STDIN_COPY"
else
  cat > /dev/null
fi

dir=''
prev=''
for arg in "$@"; do
  if [ "$prev" = '--dir' ]; then
    dir=$arg
    prev=''
  elif [ "$arg" = '--dir' ]; then
    prev='--dir'
  else
    prev=''
  fi
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
  complete|dirty_complete|outside_surface|continuable|identity_mismatch|transient_then_success)
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

  *)
    echo "fake opencode: unknown scenario '$scenario'" >&2
    exit 3
    ;;
esac
exit 0
