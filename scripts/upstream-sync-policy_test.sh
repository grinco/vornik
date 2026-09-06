#!/usr/bin/env bash
# Keep cross-owner synchronization repo-scoped and receiver execution inert.
set -euo pipefail
python3 - <<'PY'
from pathlib import Path
import os
import subprocess
import tempfile
import yaml
if not Path('.github/workflows/release-upstream-pr.yaml').exists():
    print('skip: upstream synchronization is EE-only')
    raise SystemExit(0)
def read(path):
    return yaml.load(Path(path).read_text(), Loader=yaml.BaseLoader)
publisher = read('.github/workflows/release-upstream-pr.yaml')
body = str(publisher)
assert 'UPSTREAM_SYNC_TOKEN' not in body, 'publisher still needs a cross-owner PAT'
assert 'UPSTREAM_DEPLOY_KEY' in body, 'publisher must use a repository deploy key'
assert '--force' not in body, 'published sync branches must remain immutable'
steps = publisher['jobs']['upstream-pr']['steps']
checkout = next(s for s in steps if s.get('uses', '').startswith('actions/checkout@'))
assert checkout['with']['ref'] == 'main', 'publisher must use current trusted main'
assert checkout['with']['persist-credentials'] == 'false'
assert 'git rev-parse HEAD' in body, 'branch identity must bind to the pushed commit'
receiver = read('.github/workflows/upstream-sync-pr.yaml')
assert receiver['on']['push']['branches'] == ['sync/release-*']
assert receiver['permissions'] == {'contents': 'read', 'pull-requests': 'write'}
job = receiver['jobs']['open-pr']
assert "github.repository == 'grinco/vornik-ee'" in job['if']
assert not any('uses' in s for s in job['steps']), 'receiver must not execute branch actions'
run = '\n'.join(s.get('run', '') for s in job['steps'])
assert '${{' not in run, 'event values must enter shell via environment variables'
assert 'gh pr create' in run and 'gh pr list' in run, 'receiver must reuse existing PRs'
assert 'checkout' not in run and 'git ' not in run, 'receiver must not execute branch code'
assert '^sync/release-[0-9a-f]{40}$' in run, 'receiver must reject unexpected branch names'
assert 'sync/release-$SOURCE_SHA' in run, 'branch name must identify event SHA'
with tempfile.TemporaryDirectory() as tmp:
    directory = Path(tmp)
    fake_gh = directory / 'gh'
    fake_gh.write_text('''#!/usr/bin/env bash
printf '%s\\n' "$*" >> "$CALL_LOG"
case "$*" in
  'pr list '*) printf '%s' "${EXISTING_URL:-}" ;;
  'pr create '*) printf '%s' 'https://github.com/grinco/vornik-ee/pull/1' ;;
  *) exit 99 ;;
esac
''')
    fake_gh.chmod(0o700)
    env = dict(os.environ, PATH=tmp + os.pathsep + os.environ['PATH'],
               CALL_LOG=str(directory / 'calls'), RUNNER_TEMP=tmp,
               GITHUB_STEP_SUMMARY=str(directory / 'summary'),
               SOURCE_SHA='a' * 40, BRANCH='sync/release-' + 'a' * 40)
    def invoke():
        return subprocess.run(['bash', '-c', run], env=env, capture_output=True)
    assert invoke().returncode == 0
    assert 'pr create' in (directory / 'calls').read_text()
    (directory / 'calls').write_text('')
    env['EXISTING_URL'] = 'https://github.com/grinco/vornik-ee/pull/1'
    assert invoke().returncode == 0
    assert 'pr create' not in (directory / 'calls').read_text(), 'rerun duplicated the PR'
    for bad in ['sync/release-' + 'b' * 40, 'sync/release-$(touch injected)', 'other']:
        env['BRANCH'] = bad
        (directory / 'calls').write_text('')
        assert invoke().returncode != 0, 'invalid source identity was accepted'
        assert not (directory / 'calls').read_text(), 'invalid input reached GitHub'
print('upstream sync policy: PASS')
PY
