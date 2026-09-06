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
