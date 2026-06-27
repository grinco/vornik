# merge-coverage.awk — merge two or more Go coverage profiles into one.
#
# Go's text coverage profiles (covermode=atomic/count) carry, per code
# block, a line of the form:
#
#     <file>:<startLine>.<startCol>,<endLine>.<endCol> <numStmts> <count>
#
# The first two fields (block position + statement count) uniquely
# identify a block; the trailing count is how many times it executed.
# Two profiles produced from the SAME source compiled different ways
# (e.g. unit `./...` vs `-tags=integration`) describe the same blocks,
# so merging is just: keep one `mode:` header, and for every block key
# sum the counts. A block covered only in the integration run (count 0
# in the unit run, >0 in the integration run) therefore ends up >0 in
# the merge — which is the whole point: it makes integration-only
# coverage visible in the aggregate.
#
# Usage:
#     awk -f scripts/merge-coverage.awk a.out b.out [c.out ...] > merged.out
#
# Output block order follows first-seen order so the result is stable
# across runs (golden-diff friendly). `go tool cover` does not require
# any particular ordering.

FNR == 1 {
    # First physical line of each input file is the `mode:` header.
    if ($0 ~ /^mode:/) {
        if (mode == "") {
            mode = $0
        } else if (mode != $0) {
            printf("merge-coverage: mode mismatch: %s vs %s\n", mode, $0) > "/dev/stderr"
            exit 1
        }
        next
    }
}

{
    # key = everything except the trailing count (position + numStmts).
    n = NF
    count = $n
    key = $1
    for (i = 2; i < n; i++) key = key " " $i

    if (!(key in seen)) {
        order[++norder] = key
        seen[key] = 1
    }
    total[key] += count
}

END {
    if (mode == "") mode = "mode: atomic"
    print mode
    for (i = 1; i <= norder; i++) {
        print order[i] " " total[order[i]]
    }
}
