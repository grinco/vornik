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
# The CE tag step. Nothing created the CE tag until 2026-09-06: the export
# pushed to main and stopped, every previous tag was made by hand, and 2026.9.3
# shipped with no public tag and no version-tagged agent image because of it.
# A documented step with no mechanism is a step that is sometimes skipped.
ce=read('.github/workflows/publish-ce.yaml')
steps=ce['jobs']['publish-ce']['steps'] if 'publish-ce' in ce['jobs'] else list(ce['jobs'].values())[0]['steps']
tag_steps=[s for s in steps if 'tag' in s.get('name','').lower()]
assert tag_steps, 'publish-ce must tag the exported CE tree'
body=str(tag_steps[0])
assert 'ls-remote' in body, 'the CE tag step must be idempotent — a re-run must not fail on an existing tag'
assert 'release.tag_name' in body, 'the tag must come from the release that triggered the publish'
# A release tag names one artifact forever. Moving it makes a published name
# mean something new, which is the one thing a tag must never do.
assert '--force' not in body and '-f ' not in body, 'the CE tag must never be moved'
# The release job DISPATCHES publish-ce (a GITHUB_TOKEN-published release fires
# no release event), so on every automated release `github.event.release` is
# empty and the tag step reads `inputs.tag`. 2026.9.4 shipped with no CE tag —
# the 2026.9.3 gap again, through the other door — until a second, manual
# dispatch carried `-f tag=`. The release job must pass it.
dispatch=[s for s in r['jobs']['goreleaser']['steps'] if 'fan-out' in s.get('name','').lower()]
assert dispatch, 'release.yaml must dispatch the publication fan-out'
assert 'tag=' in str(dispatch[0]), 'release.yaml must pass the release tag to publish-ce (-f tag=…), or the CE tree is never tagged'

# BOTH publication arms are asserted, not just the CE one. The upstream sync
# branch reaches grinco/vornik-ee only through this dispatch — the `release:
# published` trigger on release-upstream-pr.yaml never fires, for exactly the
# reason stated above (a GITHUB_TOKEN-published release emits no release
# event). So the dispatch line IS the mechanism, and until now nothing
# asserted it: deleting it would have silently stopped every future release
# from reaching the parent repo, with no failure anywhere.
#
# That is not hypothetical. It is the same unasserted-dispatch shape that
# shipped 2026.9.3 AND 2026.9.4 with no CE tag, twice, through two different
# doors. Asserting one arm and not the other leaves the trap armed on the
# side nobody checked.
assert 'release-upstream-pr.yaml' in str(dispatch[0]), \
    'release.yaml must dispatch release-upstream-pr.yaml, or no release ever reaches grinco/vornik-ee'
assert 'publish-ce.yaml' in str(dispatch[0]), \
    'release.yaml must dispatch publish-ce.yaml, or no release is ever exported to CE'

# --- 2026-09-22: assert the PUBLISHER SET, not a list of names ---------------
#
# The two assertions above name two arms. The fan-out dispatches THREE — docs
# is the one nobody pinned, and the 2026.9.5 notes recorded the work as "BOTH
# publication arms are now asserted". Two of three was written down as all of
# them, and the arm left out publishes the customer-facing documentation site:
# delete its line and every test here still passes while docs.vornik.io
# silently stops tracking releases.
#
# A three-name list drifts the day a fourth publisher is added, exactly as the
# two-name list did. §C of enterprise-packaging-design.md already settled this
# for a different check: assert the property GENERICALLY, "because an allowlist
# of job names carries exactly the drift hazard the omission came from". So
# this is a SET EQUALITY against the designated publishers, and adding a
# publisher without dispatching it fails here rather than in production.
PUBLISHERS={'docs.yaml','publish-ce.yaml','release-upstream-pr.yaml'}
dispatched={w for w in PUBLISHERS|{'ci.yaml','release.yaml','upstream-sync-pr.yaml'}
            if w in str(dispatch[0])}
assert dispatched == PUBLISHERS, (
    'the release fan-out must dispatch exactly the designated publishers; '
    f'dispatched={sorted(dispatched)} designated={sorted(PUBLISHERS)}')

# --- Gap 1: the mirror's tag push, asserted at the step, not at the dispatch -
#
# Nothing has EVER tagged grinco/vornik-ee. The content arrives (PRs #76-78
# merged for 9.3/9.4/9.5) and the tags do not, because release-upstream-pr.yaml
# pushes a branch and stops. That is the CE-tag defect of 2026-09-06 one
# repository over, and it went unexamined for sixteen days because the fix was
# applied to the instance rather than the class.
#
# Asserting only that the workflow is dispatched would repeat the very failure
# this file catalogues — assert the trigger, not the outcome. Deleting the tag
# push from the workflow must turn THIS red.
up=read('.github/workflows/release-upstream-pr.yaml')
def _d(node, key):
    # workflow_dispatch: with no body parses as '' under BaseLoader, which is
    # precisely the pre-fix state this assertion exists to reject.
    v = node.get(key) if isinstance(node, dict) else None
    return v if isinstance(v, dict) else {}
assert 'tag' in _d(_d(_d(up,'on'),'workflow_dispatch'),'inputs'), \
    'release-upstream-pr.yaml must declare a `tag` input, or the fan-out cannot pass one'
upbody=str(up['jobs'])
# The PUSH, not merely the string 'refs/tags/' — the first version of this
# assertion matched the idempotency `ls-remote ... refs/tags/$TAG` check, so
# deleting the push left it GREEN. Found by mutation-checking it, which is the
# only reason it is not still inert.
assert 'push upstream "HEAD:refs/tags/' in upbody, \
    'release-upstream-pr.yaml must PUSH a tag to the mirror, or grinco/vornik-ee is never tagged'
assert 'ls-remote' in upbody, \
    'the mirror tag push must be idempotent — a re-run must not fail on an existing tag'
assert '--force' not in upbody and ' -f ' not in upbody, \
    'the mirror tag must never be moved: a release tag names one artifact forever'
assert 'tag=' in str(dispatch[0]).split('release-upstream-pr.yaml')[1][:80], \
    'the fan-out must pass -f tag= to release-upstream-pr.yaml, or the tag step sees nothing'

# --- Gap 3: the installers must be stamped for the tag being released --------
#
# RELEASE.md step 1 says to run `make quickstart-stamp-ref REF=<tag>`. It was
# not run for 2026.9.5, so for five days from 2026-09-17 every fresh Podman and
# macOS install defaulted to the 2026.9.4 tree — two releases behind what the
# notes described. Nothing failed loudly, because a stale pin installs a
# working system, just the wrong one.
#
# The release job is the one place that holds both the tag and the stamped ref.
rbody=str(r['jobs']['goreleaser']['steps'])
assert 'DEFAULT_VORNIK_REF' in rbody, \
    'release.yaml must verify the podman installer is stamped for this tag'
assert 'deployments/macos/install.sh' in rbody, \
    'release.yaml must verify the macOS installer too — stamping only podman left it behind before'

# AND IT MUST RUN BEFORE ANYTHING PUBLISHES. The first version of this gate sat
# after "Publish signed checksums and release", so it reported on a release the
# world could already download — the exact defect that moved the mirror-merge
# check out of the fan-out and into a separate audit, reintroduced two gaps
# later in the same change. A gate downstream of the thing it gates is a
# notification.
names=[st.get('name','') for st in r['jobs']['goreleaser']['steps']]
stamp_at=next(i for i,n in enumerate(names) if 'stamped for this tag' in n)
publish_at=next(i for i,n in enumerate(names) if n.startswith('Publish'))
build_at=next(i for i,n in enumerate(names) if n.startswith('Build'))
assert stamp_at < build_at < publish_at, (
    'the installer-stamp gate must precede the build and the publish; '
    f'order is {names}')

# --- Gap 2: the CE release workflow, injected into the public repo ----------
#
# publish-ce creates the CE TAG and not the CE RELEASE, because the deploy key
# pushes refs and cannot call the releases API. Filed as a P3 on 2026-09-06
# with this exact route identified, and unbuilt for sixteen days — during which
# 2026.9.5 shipped with a tag and no release. A filed P3 with no owner and no
# date is indistinguishable from a step nobody wrote down.
tmpl='scripts/public-ce-templates/publish-release.yml'
assert (root/tmpl).exists(), 'the CE release workflow template must exist'
assert 'publish-release.yml' in (root/'scripts/export-public-ce.sh').read_text(), \
    'the CE export must inject publish-release.yml, or the workflow never reaches the public repo'
pr=read(tmpl)
pron=pr.get('on',{})
# TAG PUSH ONLY. A fork cannot push a tag here and pull_request does not fire
# push:tags, so this reduces the trigger set to holders of tag-push rights. A
# workflow_dispatch would let any write collaborator fire it at an arbitrary
# ref carrying a fabricated section — defensible for an idempotent image build,
# not for a release object.
assert set(pron.keys()) == {'push'}, \
    f'the CE release workflow must trigger on tag push ONLY; found {sorted(pron.keys())}'
assert 'tags' in pron['push'] and 'branches' not in pron['push'], \
    'the CE release workflow must trigger on tags, never on branch pushes'
# EXPLICIT permissions. With no block the job inherits the repository default,
# which is a blast radius nobody chose.
assert pr.get('permissions') == {'contents': 'write'}, \
    f"the CE release workflow must declare permissions: contents: write and nothing else; found {pr.get('permissions')}"
prbody=str(pr['jobs'])
assert 'gh release view' in prbody, \
    'creating the CE release must be idempotent — a re-run must not fail on an existing release'
assert 'docs/public/release-notes/index.md' in prbody, \
    'the CE release body must come from the curated PUBLIC notes, never the EE ones'

print('CI/release policy: PASS')
PY
