#!/usr/bin/env python3
"""pty-based functional test harness for the OSC 52 wl-copy/wl-paste
wrapper scripts. Invoked from Go tests (see osc52_wrapper_test.go) via
`python3 osc52_test_harness.py <copy|paste> <rendered-script-path>`.

Not part of the canopy binary — a test-only tool, same tier as this
package's existing `bash -n` / socat / timeout PATH-lookup test deps
(see scripts_test.go's TestWrapperScripts_PassBashSyntaxCheck and
TestWrapperScripts_ListTypes*). Go's stdlib has no pty allocation
primitive; shelling out to a small, skip-if-missing external tool for
test-only pty emulation matches this codebase's established pattern
rather than adding a pty library dependency.

A real pty + explicit TIOCSCTTY claim is required because the wrapper
scripts open /dev/tty directly (not stdin/stdout) — that's the whole
point of the OSC 52 mechanism (querying the actual controlling
terminal, which owns the real system clipboard), so this harness plays
the role of "the local terminal emulator" on the pty master side.
"""
import base64
import fcntl
import os
import pty
import select
import subprocess
import sys
import termios
import time


def _claim_controlling_tty():
    # Popen's stdin=slave_fd dup2's the slave onto fd 0 before
    # preexec_fn runs, but inheriting the fd alone does not make it
    # the controlling terminal -- the kernel only auto-assigns one
    # when a session leader with none yet actually open()s a terminal
    # device. setsid() + TIOCSCTTY on the already-inherited fd 0
    # claims it explicitly.
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)


def run(script_path, args, reply_bytes, extra_env=None, timeout_s=6):
    """Spawns `bash script_path args...` on a real pty. If reply_bytes
    is not None, writes it to the pty master once any output appears
    on it (simulating the terminal answering an OSC 52 query). Returns
    (master_saw: bytes, stdout: bytes, stderr: bytes, exit_code)."""
    master_fd, slave_fd = pty.openpty()
    env = {k: v for k, v in os.environ.items() if k != "TMUX"}
    if extra_env:
        env.update(extra_env)
    proc = subprocess.Popen(
        ["bash", script_path] + args,
        stdin=slave_fd, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env=env, preexec_fn=_claim_controlling_tty, close_fds=True,
    )
    os.close(slave_fd)

    replied = reply_bytes is None  # if no reply configured, "already done"
    master_saw = b""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        r, _, _ = select.select([master_fd], [], [], 0.3)
        if master_fd in r:
            try:
                chunk = os.read(master_fd, 4096)
            except OSError:
                break
            if not chunk:
                break
            master_saw += chunk
            if not replied:
                replied = True
                os.write(master_fd, reply_bytes)
        if proc.poll() is not None:
            break
    try:
        out, err = proc.communicate(timeout=3)
    except subprocess.TimeoutExpired:
        proc.kill()
        out, err = proc.communicate()
    os.close(master_fd)
    return master_saw, out, err, proc.returncode


def check(name, cond, detail=""):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}" + (f" -- {detail}" if detail and not cond else ""))
    return cond


def test_paste(script_path):
    results = []
    payload = b"hello from local clipboard"
    b64 = base64.b64encode(payload)

    saw, out, err, code = run(script_path, ["--no-newline"], b"\x1b]52;c;" + b64 + b"\x1b\\")
    results.append(check("paste/ST_terminator", out == payload and code == 0, f"out={out!r} code={code} err={err!r}"))

    saw, out, err, code = run(script_path, ["--no-newline"], b"\x1b]52;c;" + b64 + b"\x07")
    results.append(check("paste/BEL_terminator", out == payload and code == 0, f"out={out!r} code={code} err={err!r}"))

    saw, out, err, code = run(script_path, ["--no-newline"], b"\x1b]52;c;\x1b\\")
    results.append(check("paste/empty_clipboard", out == b"" and code == 0, f"out={out!r} code={code} err={err!r}"))

    saw, out, err, code = run(script_path, ["--no-newline"], None, timeout_s=5)
    results.append(check("paste/no_reply_fails_closed", out == b"" and code == 1, f"out={out!r} code={code} err={err!r}"))

    saw, out, err, code = run(script_path, ["--list-types"], b"\x1b]52;c;" + b64 + b"\x1b\\")
    results.append(check("paste/list_types_responsive", out == b"text/plain;charset=utf-8\n" and code == 0, f"out={out!r} code={code}"))

    saw, out, err, code = run(script_path, ["--list-types"], None, timeout_s=5)
    results.append(check("paste/list_types_unresponsive_claims_nothing", out == b"" and code == 0, f"out={out!r} code={code}"))

    saw, out, err, code = run(script_path, [], b"\x1b]52;c;" + b64 + b"\x1b\\")
    results.append(check("paste/default_appends_newline", out == payload + b"\n" and code == 0, f"out={out!r} code={code}"))

    return results


def test_copy(script_path):
    results = []
    payload = "hello world"
    b64 = base64.b64encode(payload.encode()).decode()

    saw, out, err, code = run(script_path, [payload], None)
    want = f"\x1b]52;c;{b64}\x1b\\".encode()
    results.append(check("copy/plain_sequence", saw == want and code == 0, f"saw={saw!r} want={want!r} code={code} err={err!r}"))

    saw, out, err, code = run(script_path, [payload], None, extra_env={"TMUX": "/tmp/fake-tmux-socket,1,0"})
    inner = f"\x1b]52;c;{b64}\x1b\\"
    doubled_inner = inner.replace("\x1b", "\x1b\x1b")
    want = f"\x1bPtmux;{doubled_inner}\x1b\\".encode()
    results.append(check("copy/tmux_dcs_wrapped", saw == want and code == 0, f"saw={saw!r} want={want!r} code={code} err={err!r}"))

    return results


def main():
    if len(sys.argv) != 3 or sys.argv[1] not in ("copy", "paste"):
        print("usage: osc52_test_harness.py <copy|paste> <rendered-script-path>", file=sys.stderr)
        sys.exit(2)
    mode, script_path = sys.argv[1], sys.argv[2]
    results = test_copy(script_path) if mode == "copy" else test_paste(script_path)
    n = len(results)
    passed = sum(1 for r in results if r)
    print(f"{passed}/{n} PASS")
    sys.exit(0 if passed == n else 1)


if __name__ == "__main__":
    main()
