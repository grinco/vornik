package main

// The unreferenced ratchet. Same semantics as the drift linter's ceilings: a
// versioned count that may only fall, a malformed file is a hard error rather
// than a silent zero, and raising it requires a visible commit.
//
// A one-directional counter is the right adoption strategy here for the same
// reason it is in the drift design: the existing residue is large, mixed
// (genuinely dead code AND undocumented entry points), and needs human
// judgement per item — but NEW unreferenced code should be stopped at the door.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func loadCeiling(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Uninitialised: force a deliberate baseline via -ratchet rather than
			// silently tolerating whatever exists today.
			return 0, nil
		}
		return 0, fmt.Errorf("read ceiling %s: %w", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n, convErr := strconv.Atoi(line)
		if convErr != nil {
			return 0, fmt.Errorf("ceiling %s: %q is not an integer", path, line)
		}
		if n < 0 {
			return 0, fmt.Errorf("ceiling %s: negative count %d", path, n)
		}
		return n, nil
	}
	return 0, fmt.Errorf("ceiling %s contains no count", path)
}

func writeCeiling(path string, count int) error {
	body := fmt.Sprintf(`# Ratcheted debt counter — symbols neither reachable from a main nor named by a contract.
#
# This number may only go DOWN. `+"`make reachability`"+` fails when the current count
# exceeds it. Lower it with `+"`make reachability-ratchet`"+` once you have reduced the debt.
#
# Each entry is EITHER genuinely dead code OR an undocumented entry point — the
# analysis cannot tell which, and the fix differs (delete it, or write the
# contract). That is why this is a report under a ceiling rather than a hard gate.
#
# RAISING this number means new debt was accepted. Nothing automates that, and
# this repo has no branch protection, so the commit is the control. Say why.
%d
`, count)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
