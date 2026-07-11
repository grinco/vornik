#!/usr/bin/env bash
# Regression guard for audit HIGH-1 (2026-07-09): the agent's test_run /
# lint_run / typecheck_run runners execute the REVIEWED repo's own code and
# must NOT expose the push-capable GH_TOKEN/GITHUB_TOKEN to it. This asserts
# every subprocess.run that runs a repo toolchain inside the test-run heredoc
# passes the scrubbed env (env=SAFE_ENV). A future edit that adds a runner
# without the scrub — reintroducing the leak — fails this check.
#
# No agent-image build required; a static assertion over entrypoint.sh.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"

# 1. The script must parse.
bash -n "$ep"

# 2. Extract the test-run heredoc (from the SAFE_ENV definition to the next
#    lone 'PY' terminator) and assert no bare `subprocess.run(` inside it lacks
#    env=SAFE_ENV. awk sets a flag on the SAFE_ENV line and clears it at PY.
violations="$(awk '
  /SAFE_ENV = \{k: v for k/ { inblk=1 }
  inblk && /^PY$/            { inblk=0 }
  inblk && /subprocess\.run\(/ && $0 !~ /env=SAFE_ENV/ { print NR": "$0 }
' "$ep")"

if [ -n "$violations" ]; then
  echo "FAIL: test-run subprocess.run without env=SAFE_ENV (audit HIGH-1 leak):" >&2
  echo "$violations" >&2
  exit 1
fi

# 3. SAFE_ENV must actually strip both token vars.
grep -q '_SECRET_ENV_KEYS = ("GH_TOKEN", "GITHUB_TOKEN")' "$ep" \
  || { echo "FAIL: SAFE_ENV no longer strips GH_TOKEN/GITHUB_TOKEN" >&2; exit 1; }

echo "OK: agent test-run env scrubs GH_TOKEN/GITHUB_TOKEN (audit HIGH-1)"
