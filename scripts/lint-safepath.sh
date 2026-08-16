#!/usr/bin/env bash
# lint-safepath — keep the barrier that justifies a CodeQL suppression.
#
# The public CE repo suppresses go/path-injection and go/zipslip
# (.github/codeql/codeql-config.yml) because every open instance was verified to
# route through internal/safepath, which CodeQL does not recognise as a barrier.
#
# A suppression is only as good as the invariant behind it. This check asserts
# that invariant still holds, so removing the barrier fails the build instead of
# silently landing an unscanned path traversal.
#
# It deliberately does NOT attempt dataflow — that is CodeQL's job and it would
# be a bad reimplementation. It asserts something narrower and checkable: each
# file whose alert we suppressed still reaches a safepath barrier, either
# directly or through the named file that builds its paths.
#
# Adding a file here is fine when a new sink genuinely routes through safepath.
# Removing one means the suppression no longer covers it — either restore the
# barrier or stop suppressing the rule.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

# Files that had a suppressed alert AND sanitise in-file.
DIRECT=(
  internal/api/extract_handlers.go
  internal/api/extract_inputs.go
  internal/artifacts/local_backend.go
  internal/artifacts/store.go
  internal/archiveutil/archiveutil.go
  internal/extractor/runner.go
  internal/projectarchive/lifecycle.go
  internal/service/container_workflow_architect.go
  internal/templates/suggest.go
  internal/templates/write.go
  internal/ui/artifacts.go
  internal/ui/project_archive.go
  internal/ui/project_brief.go
  internal/ui/project_config.go
  internal/ui/project_config_form.go
  internal/ui/project_schema_config.go
  internal/ui/swarm_edit.go
  internal/ui/swarm_schema_config.go
  internal/ui/wizard.go
  internal/ui/workflow_edit.go
  internal/ui/workflow_schema_config.go
)

# Files whose sink takes a path built elsewhere. "<sink file>:<barrier file>" —
# the barrier file is where safepath is actually applied. This mapping is the
# reason CodeQL misses these: the barrier is in a different file from the sink.
INDIRECT=(
  "internal/ui/swarm_delete.go:internal/ui/swarm_edit.go"
  "internal/ui/schema_config_save.go:internal/ui/project_schema_config.go"
  "internal/extractor/textfile/textfile.go:internal/extractor/runner.go"
  "internal/extractor/html/html.go:internal/extractor/runner.go"
  "internal/extractor/image/image.go:internal/extractor/runner.go"
  "internal/extractor/audio/audio.go:internal/extractor/runner.go"
  "internal/extractor/epub/epub.go:internal/extractor/runner.go"
  "internal/dispatcher/attachment_bytes.go:internal/artifacts/store.go"
)

fail=0

for f in "${DIRECT[@]}"; do
  if [ ! -f "$f" ]; then
    echo "lint-safepath: $f is listed but no longer exists — update the list or the suppression."
    fail=1
    continue
  fi
  if ! grep -q "internal/safepath" "$f"; then
    echo "lint-safepath: $f no longer references internal/safepath."
    echo "  Its go/path-injection alert is SUPPRESSED on the public repo on the"
    echo "  grounds that safepath sanitises it. Restore the barrier, or remove the"
    echo "  exclusion from .github/codeql/codeql-config.yml so the scanner sees it."
    fail=1
  fi
done

for pair in "${INDIRECT[@]}"; do
  sink="${pair%%:*}"
  barrier="${pair##*:}"
  if [ ! -f "$sink" ]; then
    echo "lint-safepath: $sink is listed but no longer exists — update the list."
    fail=1
    continue
  fi
  if [ ! -f "$barrier" ]; then
    echo "lint-safepath: barrier file $barrier (for $sink) no longer exists."
    fail=1
    continue
  fi
  if ! grep -q "internal/safepath" "$barrier"; then
    echo "lint-safepath: $sink relies on $barrier for sanitisation, but that file"
    echo "  no longer references internal/safepath."
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo
  echo "lint-safepath: FAILED — a suppressed CodeQL rule lost its barrier."
  exit 1
fi

echo "lint-safepath: OK — ${#DIRECT[@]} direct + ${#INDIRECT[@]} indirect sinks still reach a safepath barrier."
