package api

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// processStartedAt is captured once at package load. By the time
// HTTP serving is up (and the doctor endpoint can be reached) this
// value is a few milliseconds newer than the actual binary start
// — close enough to detect env-file edits that happened in the
// minutes or hours after the daemon was launched, which is the
// real failure mode B-13 guards against:
//
//	operator edits ~/.config/vornik/secrets/vertex.env
//	operator forgets to `systemctl --user restart vornik`
//	chat router uses stale GCP_API_KEY (or empty, on first edit)
//	tasks fail with cryptic 401/404 from upstream LLM
//
// The mtime comparison is intentionally one-directional: env file
// newer than the daemon process = stale env. The reverse can't
// happen (env file existed when daemon started → already loaded).
var processStartedAt = time.Now()

// checkEnvFileFreshness compares mtime of every EnvironmentFile=
// entry in the vornik systemd unit against the captured daemon
// start time. Files newer than the daemon's boot are not visible
// to the running process — systemd only reads EnvironmentFile=
// at ExecStart.
//
// Non-Linux platforms and setups without a vornik.service file
// return OK with a "not applicable" message — this is a Linux/
// systemd-only diagnostic and we don't want it to noise up
// containerised or macOS dev runs.
func (h *DoctorHandlers) checkEnvFileFreshness() DoctorCheck {
	name := "env_file_freshness"

	unitPath, ok := locateVornikUnitFile()
	if !ok {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "no vornik.service unit file found (containerised or non-systemd setup)",
		}
	}

	entries, err := parseEnvironmentFileEntries(unitPath)
	if err != nil {
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("could not parse %s: %v", unitPath, err),
		}
	}
	if len(entries) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("%s has no EnvironmentFile= entries", unitPath),
		}
	}

	var stale []string
	checked := 0
	for _, e := range entries {
		st, err := os.Stat(e.Path)
		if err != nil {
			if os.IsNotExist(err) && e.Optional {
				// `-`-prefixed optional file that doesn't exist; that's
				// fine, systemd will silently skip it at boot.
				continue
			}
			stale = append(stale, fmt.Sprintf("%s: cannot stat (%v)", e.Path, err))
			continue
		}
		checked++
		if st.ModTime().After(processStartedAt) {
			delta := st.ModTime().Sub(processStartedAt).Truncate(time.Second)
			stale = append(stale, fmt.Sprintf(
				"%s: modified %s after daemon start — `systemctl --user restart vornik` to reload",
				e.Path, delta))
		}
	}

	if len(stale) > 0 {
		sort.Strings(stale)
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("%d env file(s) modified after daemon start; daemon is running with stale environment", len(stale)),
			Items:   stale,
		}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "OK",
		Message: fmt.Sprintf("%d env file(s) all older than daemon start", checked),
	}
}

// envFileEntry is one EnvironmentFile= line from the unit file.
type envFileEntry struct {
	Path     string
	Optional bool // `-` prefix in systemd means "skip if missing"
}

// locateVornikUnitFile returns the first existing vornik.service
// in systemd's --user and --system search order. The returned
// path is suitable for parsing; an empty bool means we couldn't
// find one (e.g. running in a container, or unit installed under
// a non-standard name).
func locateVornikUnitFile() (string, bool) {
	var candidates []string

	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "systemd/user/vornik.service"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".config/systemd/user/vornik.service"),
			filepath.Join(home, ".local/share/systemd/user/vornik.service"),
		)
	}
	candidates = append(candidates,
		"/etc/systemd/user/vornik.service",
		"/etc/systemd/system/vornik.service",
		"/usr/lib/systemd/user/vornik.service",
		"/usr/lib/systemd/system/vornik.service",
	)

	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, true
		}
	}
	return "", false
}

// parseEnvironmentFileEntries reads a systemd unit file and
// returns every EnvironmentFile= entry it finds (including those
// with a leading `-` to mark them optional). systemd allows
// multiple EnvironmentFile= directives per unit; each one is one
// path. Tokens like %h (home) and %t (runtime dir) are expanded
// so the returned paths are absolute and statable.
//
// We intentionally do NOT follow Drop-Ins (override.conf) yet —
// in practice vornik installs don't use them, and parsing the
// full drop-in stack would duplicate systemd's own logic. If
// this surfaces as a real false-negative, switch this function
// to shell out to `systemctl --user show vornik
// --property=EnvironmentFiles` which does that work for us.
func parseEnvironmentFileEntries(unitPath string) ([]envFileEntry, error) {
	f, err := os.Open(unitPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	home, _ := os.UserHomeDir()

	var out []envFileEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		const prefix = "EnvironmentFile="
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if value == "" {
			continue
		}
		optional := false
		if strings.HasPrefix(value, "-") {
			optional = true
			value = strings.TrimPrefix(value, "-")
		}
		value = expandUnitSpecifiers(value, home)
		if !filepath.IsAbs(value) {
			// systemd requires absolute paths for EnvironmentFile=;
			// a relative path here is a unit-file bug we can't
			// silently resolve. Skip with no error so other entries
			// still report.
			continue
		}
		out = append(out, envFileEntry{Path: value, Optional: optional})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// expandUnitSpecifiers handles the small subset of systemd %
// tokens that show up in real vornik EnvironmentFile= paths.
// %h expands to the user's home (`~`); %% escapes a literal `%`.
// Unknown tokens are left as-is so we don't accidentally rewrite
// a future installer's path into something wrong; the downstream
// stat() call will then fail and surface in the report.
func expandUnitSpecifiers(s, home string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		next := s[i+1]
		switch next {
		case '%':
			b.WriteByte('%')
		case 'h':
			b.WriteString(home)
		default:
			b.WriteByte('%')
			b.WriteByte(next)
		}
		i++
	}
	return b.String()
}
