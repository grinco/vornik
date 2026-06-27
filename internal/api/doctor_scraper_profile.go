package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultScraperLoginCadence is the fallback re-login interval used when a
// profile's entry is absent from loginRequired. Conservative: warn when a
// session hasn't been refreshed in 30 days.
const defaultScraperLoginCadence = 30 * 24 * time.Hour

// checkScraperProfileFreshness walks scraperProfileRoot and checks each
// browser-profile directory for the newest cookie file. When the cookie's
// mtime exceeds the configured cadence for that profile, it emits a WARNING
// naming the stale profile and the re-login command.
//
// Profile tree layout expected:
//
//	<scraperProfileRoot>/<project>/<profile>/Default/Network/Cookies
//	<scraperProfileRoot>/<project>/<profile>/Default/Cookies  (fallback)
//
// The check is nil-safe: when scraperProfileRoot is empty (not configured)
// it returns OK immediately.
func (h *DoctorHandlers) checkScraperProfileFreshness(_ context.Context, _ bool) DoctorCheck {
	name := "scraper_profile_freshness"

	if h.scraperProfileRoot == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "scraper profiles not configured"}
	}

	// Walk <root>/<project>/<profile> — two directory levels deep.
	projectEntries, err := os.ReadDir(h.scraperProfileRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return DoctorCheck{Name: name, Status: "OK", Message: "scraper profile root does not exist; skipping"}
		}
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("cannot read scraper profile root: %v", err)}
	}

	var stale []string
	now := time.Now()

	for _, projEntry := range projectEntries {
		if !projEntry.IsDir() {
			continue
		}
		projDir := filepath.Join(h.scraperProfileRoot, projEntry.Name())
		profileEntries, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, profEntry := range profileEntries {
			if !profEntry.IsDir() {
				continue
			}
			profileName := profEntry.Name()
			profileDir := filepath.Join(projDir, profileName)

			// Find the newest cookie file — prefer Network/Cookies then Default/Cookies.
			cookieCandidates := []string{
				filepath.Join(profileDir, "Default", "Network", "Cookies"),
				filepath.Join(profileDir, "Default", "Cookies"),
			}
			var newestCookie time.Time
			for _, cp := range cookieCandidates {
				st, err := os.Stat(cp)
				if err != nil {
					continue
				}
				if st.ModTime().After(newestCookie) {
					newestCookie = st.ModTime()
				}
			}
			if newestCookie.IsZero() {
				// No cookie files found for this profile — session may never
				// have been seeded. Surface as stale so the operator knows.
				stale = append(stale, fmt.Sprintf(
					"%s (no cookie file found — run: vornikctl scraper login start -p %s)",
					profileName, projEntry.Name(),
				))
				continue
			}

			// Determine the required cadence for this profile.
			cadence := defaultScraperLoginCadence
			if h.loginRequired != nil {
				if c, ok := h.loginRequired[profileName]; ok {
					cadence = c
				}
			}

			age := now.Sub(newestCookie)
			if age > cadence {
				stale = append(stale, fmt.Sprintf(
					"%s (session %s old, cadence %s — run: vornikctl scraper login start -p %s)",
					profileName,
					age.Truncate(time.Hour),
					cadence.Truncate(time.Hour),
					projEntry.Name(),
				))
			}
		}
	}

	if len(stale) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "all scraper profiles have fresh login sessions"}
	}
	// Collect just the profile names for the message (the Items carry full detail).
	profileNames := make([]string, 0, len(stale))
	for _, s := range stale {
		// Each stale entry starts with "<profileName> (" — extract the name.
		end := strings.Index(s, " ")
		if end <= 0 {
			end = len(s)
		}
		profileNames = append(profileNames, s[:end])
	}
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf(
			"%d scraper profile(s) have stale sessions (%s) — run: vornikctl scraper login start",
			len(stale), strings.Join(profileNames, ", "),
		),
		Items: stale,
	}
}

// SetScraperProfileRoot wires the scraper profile root directory and login
// cadence map at boot. Called from the service container. Nil-safe.
func (h *DoctorHandlers) SetScraperProfileRoot(root string, cadences map[string]time.Duration) {
	if h == nil {
		return
	}
	h.scraperProfileRoot = root
	h.loginRequired = cadences
}
