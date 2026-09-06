#!/usr/bin/env bash
# Regression: September 2026 audit found unused binaries, partial dispatch
# verification, duplicate unsigned publishing and PR cache uploads.
set -euo pipefail
[ -f .goreleaser.enterprise.yaml ] || exit 0
python3 - <<'PY'
from pathlib import Path
import yaml
root=Path(__file__).resolve().parent if False else Path.cwd()
def read(p): return yaml.load((root/p).read_text(), Loader=yaml.BaseLoader)
ci=read('.github/workflows/ci.yaml')
for job in ci['jobs'].values():
 for step in job.get('steps',[]):
  if 'actions/upload-artifact@' in step.get('uses',''):
   assert step['with']['name'] != 'vornik-binary', 'unused binary artifact'
   assert step['with'].get('retention-days') == '1'
   assert 'full' in step.get('if',''), 'partial PR artifact has no consumer'
assert 'workflow_dispatch' in str(ci['jobs']['changes']['steps']), 'dispatch must run full tests'

# --- the sync/release-* branch gate -----------------------------------------
# The upstream receiver's sync PR skips CI because the PUSH to the same
# immutable sync branch already ran it. Two things have to hold for that to be
# a gate rather than a hole, and neither was asserted anywhere:
#
#   1. the push CI it defers to must actually exist, and
#   2. the skip must be narrow enough that nothing else can claim it. Drop the
#      same-repo clause and a FORK PR could name its head branch
#      "sync/release-<40 hex>" and skip every check in this workflow.
push_branches = ci['on' if 'on' in ci else True]['push']['branches']
assert 'sync/release-*' in push_branches, \
    'sync branches must run full CI on push — the PR-side skip defers to it'
changes_if = ci['jobs']['changes']['if']
for clause in ("github.event_name != 'pull_request'",
               "github.repository != 'grinco/vornik-ee'",
               'github.event.pull_request.head.repo.full_name != github.repository',
               "!startsWith(github.head_ref, 'sync/release-')"):
    assert clause in changes_if, f'changes-job skip lost its {clause!r} narrowing clause'
# verify is the single required check. If it could pass while `changes` was
# skipped, a skipped sync PR would report a green aggregate over zero jobs.
verify_if = ci['jobs']['verify']['if']
assert "needs.changes.result != 'skipped'" in verify_if, \
    'verify must skip in lockstep with changes, never report success over a skipped run'

# EVERY job must reach the aggregate, and every needed job must be reported.
# Named generically on purpose: an allowlist of job names is the same drift
# hazard the job list itself is. `ce-export-verify` — the CE operator-token leak
# scan and the Art 50 parity self-test — was the one job outside verify's needs
# from its introduction until 2026-09-06, so a leak-scan failure left the single
# aggregate check green.
jobs = set(ci['jobs'])
needs = set(ci['jobs']['verify']['needs'])
results = ci['jobs']['verify']['steps'][0]['env']['RESULTS']
reported = {line.split('=')[0].strip() for line in results.split() if '=' in line}
assert not (jobs - needs - {'verify'}), \
    f'job(s) outside the verify aggregate: {sorted(jobs - needs - {"verify"})}'
assert not (needs - reported), \
    f'job(s) verify waits on but never reports: {sorted(needs - reported)}'
r=read('.github/workflows/release.yaml')
assert not Path('.github/workflows/release-enterprise.yaml').exists(), 'duplicate package publisher'
assert 'grinco/vornik-enterprise' in r['jobs']['goreleaser']['if']
assert 'GPG_PRIVATE_KEY' in str(r)
assert '--skip=sign' not in str(r)
assert 'Verify full CI' in str(r)
a=read('.github/actions/setup-go/action.yaml')
assert a['inputs']['cache-write']['default'] == 'false'
assert 'refs/heads/main' in str(a)
assert 'actions/cache/restore@' in str(a)
print('CI/release policy: PASS')
PY
