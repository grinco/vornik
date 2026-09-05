# tool_dispatch golden fixtures

Recorded 2026-09-05 against `images/vornik-agent/entrypoint.sh` at commit
`aba4c44c` — the last revision in which file_read, file_write, file_edit,
read_many_files, grep, glob, git_status, git_diff, git_log and git_show were
bash cases in `exec_tool` (each shelling out to python3) — by

    test/agent/tool_dispatch_golden_test.sh --record

Each file is `exec_tool`'s stdout for one case over the deterministic
workspace the test builds (fixed content, fixed mtimes, a git repository with
fixed identity and dates), with the temporary workspace path replaced by
`$WS`. They are the behaviour-preservation spec for the port in
`https://docs.vornik.io` §5.

Re-record ONLY for a deliberate change to what a tool prints, in the same
commit as that change, and say so in the commit message. The four divergences
the design names (D1–D4) each have a case whose fixture is re-recorded in the
commit that introduces that divergence, and no other. A re-record that
accompanies a refactor is the refactor changing behaviour.

Two behaviours worth knowing are pinned here rather than chosen: `glob`
follows directory symlinks under `**`, including one that leaves the
workspace (`out/x.txt` in `glob-D3-doublestar.out`) — it lists the name and
nothing more, and `file_read` on that path is refused (`file_read-escaping-symlink.out`);
and `grep` on `big.bin` prints the whole 40 000-byte line in content mode.

## Re-recorded 2026-09-05, with the port

Four fixtures — and only four — were re-recorded when the tools moved to Go,
one per named divergence: `file_read-after-D1.out` (D1: the invalid byte is
preserved rather than replaced by U+FFFD), `grep-invalid-regex.out` and
`grep-D2-backreference.out` (D2: RE2's error text, and a backreference is a
rejected pattern), `git_diff-D4-wide-truncated.out` (D4: the cut and the
total are bytes). The other sixty compare equal to the bash recording.

