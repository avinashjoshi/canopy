# Agent classifier test fixtures

Raw `tmux capture-pane` snapshots of real agent TUI states, used as ground truth
for `internal/agent/classifier_*_test.go`. When the upstream CLI ships UI
changes, re-capture these files against a known-good version and bump the
"last verified" comment next to each regex pattern in the matching
`classifier_<launcher>.go`.

## codex (captured 2026-06-17 against codex-cli 0.140.0, model gpt-5.5)

- `codex_trust_dialog.txt` — first-launch trust prompt
  ("Do you trust the contents of this directory?")
- `codex_idle.txt` — codex at input prompt, no activity (banner + footer)
- `codex_thinking_a.txt` + `codex_thinking_b.txt` — mid-response, captured 2s apart;
  the only line that changed is the spinner timer (`• Working (1s ...)` →
  `• Working (3s ...)`). Used to prove motion-based Thinking detection AND
  to validate that `normalize()` strips the spinner line so a long-idle pane
  doesn't flip-flop on timer.
- `codex_awaiting_input.txt` — codex's approval dialog for a file-write
  ("Would you like to make the following edits?" / "Press enter to confirm or
  esc to cancel"). Only appears under `--ask-for-approval untrusted` or
  `on-request`; in default mode codex auto-applies edits with no dialog.

## Re-capture procedure

```bash
SPIKE=/tmp/codex-spike-cwd
mkdir -p "$SPIKE" && cd "$SPIKE" && git init -q
tmux -L codex-spike new-session -d -s spike -x 200 -y 50 \
  -c "$SPIKE" "codex --ask-for-approval untrusted"
sleep 5
# Trust dialog:
tmux -L codex-spike capture-pane -t spike -p > codex_trust_dialog.txt
tmux -L codex-spike send-keys -t spike Enter
sleep 4
# Idle:
tmux -L codex-spike capture-pane -t spike -p > codex_idle.txt
# Thinking: send any prompt and capture twice during execution
# Awaiting: ask codex to write a file in untrusted mode
tmux -L codex-spike kill-server
```
