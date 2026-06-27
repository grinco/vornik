package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Scraper login sidecar CLI.
//
// The headless scraper service (vornik-scraper) can't get past login
// walls, paywalls, or anti-bot CAPTCHAs on its own. When a portal
// returns block_reason="login_wall" / "auth_required" / "captcha"
// often enough to matter, an operator runs `vornikctl scraper login
// start --project=X --profile=Y`, opens the printed URL in a real
// browser, drives Chrome via noVNC to log in, and runs `... login
// stop` when done. The session cookies persist into the same
// profile dir the headless scraper reads on its next run.
//
// Why we manage the lifecycle here rather than via systemd:
//
//   - Chrome locks the profile directory with a flock. The
//     headless scraper holds it while running; the login sidecar
//     can't open the same profile without first stopping the
//     scraper. This CLI sequences stop → start sidecar → start →
//     restart scraper so operators don't have to remember the
//     order.
//   - The sidecar is bursty (a few minutes of use, then idle for
//     days). A systemd unit would either need manual start/stop
//     anyway, or sit idle consuming a network port. Keeping the
//     lifecycle in a CLI command makes the on-demand nature
//     explicit.

const (
	loginContainerName = "vornik-scraper-login"
	loginImage         = "localhost/vornik-scraper-login:latest"
	scraperUnit        = "vornik-scraper.service"
	loginVNCPort       = 6080
)

var (
	scraperCmd = &cobra.Command{
		Use:   "scraper",
		Short: "Manage the headless-browser scraper service",
	}
	scraperLoginCmd = &cobra.Command{
		Use:   "login",
		Short: "Manually authenticate a scraper profile via VNC",
		Long: `Manually authenticate a scraper profile when a portal is gated
by a login wall, paywall, or anti-bot CAPTCHA the headless scraper
cannot bypass.

The 'start' subcommand stops the headless scraper to release its
profile lock, runs the login sidecar (Xvfb + Chrome + noVNC), and
prints the URL to open in a real browser. The operator drives
Chrome via the noVNC web UI, logs in, then runs 'login stop' which
tears down the sidecar and restarts the headless scraper.

Examples:
  vornikctl scraper login start --project=janka --profile=default
  vornikctl scraper login start -p janka --url https://www.linkedin.com
  vornikctl scraper login stop`,
	}
	scraperLoginStartCmd = &cobra.Command{
		Use:   "start",
		Short: "Stop scraper, start login sidecar, print noVNC URL",
		RunE:  runScraperLoginStart,
	}
	scraperLoginStopCmd = &cobra.Command{
		Use:   "stop",
		Short: "Stop login sidecar and restart the scraper",
		RunE:  runScraperLoginStop,
	}
	scraperLoginStatusCmd = &cobra.Command{
		Use:   "status",
		Short: "Show whether a login session is active",
		RunE:  runScraperLoginStatus,
	}

	scraperLoginProject  string
	scraperLoginProfile  string
	scraperLoginURL      string
	scraperLoginPassword string
	scraperLoginPort     int
	scraperLoginBind     string
)

func init() {
	scraperLoginStartCmd.Flags().StringVarP(&scraperLoginProject, "project", "p", "",
		"Project ID whose profile to authenticate (required)")
	scraperLoginStartCmd.Flags().StringVar(&scraperLoginProfile, "profile", "default",
		"Named profile under the project (mirrors the scraper's profile arg)")
	scraperLoginStartCmd.Flags().StringVar(&scraperLoginURL, "url", "about:blank",
		"URL Chrome opens at startup — set this to the portal you're authenticating")
	scraperLoginStartCmd.Flags().StringVar(&scraperLoginPassword, "vnc-password", "",
		"Optional VNC password. Empty = open VNC; safe ONLY when bound to localhost. "+
			"Set this if --bind is anything other than 127.0.0.1.")
	scraperLoginStartCmd.Flags().IntVar(&scraperLoginPort, "port", loginVNCPort,
		"Host port for noVNC")
	scraperLoginStartCmd.Flags().StringVar(&scraperLoginBind, "bind", "127.0.0.1",
		"Host IP to bind the noVNC port to. Default 127.0.0.1 (localhost only). "+
			"Use 0.0.0.0 to allow LAN access — set --vnc-password too in that case "+
			"because the VNC stream is unencrypted.")
	_ = scraperLoginStartCmd.MarkFlagRequired("project")

	scraperLoginCmd.AddCommand(scraperLoginStartCmd, scraperLoginStopCmd, scraperLoginStatusCmd)
	scraperCmd.AddCommand(scraperLoginCmd)
	rootCmd.AddCommand(scraperCmd)
}

// runScraperLoginStart sequences the lifecycle: scraper stop → start
// sidecar → wait for ready → print URL. Returns to the shell with
// the sidecar running in the background so the operator can browse
// to the URL and log in. `login stop` closes the loop.
func runScraperLoginStart(cmd *cobra.Command, _ []string) error {
	if scraperLoginBind != "127.0.0.1" && scraperLoginPassword == "" {
		return fmt.Errorf("refusing non-local bind without --vnc-password; noVNC/VNC traffic is unencrypted and would be exposed on %s:%d",
			scraperLoginBind, scraperLoginPort)
	}

	// Reject if the sidecar is already running — chaining starts
	// would orphan the previous container and leave the operator
	// confused about which session their browser is connected to.
	if running, _ := loginContainerRunning(); running {
		return fmt.Errorf("login sidecar already running; run `vornikctl scraper login stop` first")
	}

	// Stop the headless scraper to release the profile flock. The
	// systemd unit name is the canonical service identifier; if the
	// operator runs the scraper outside systemd they'll need to
	// stop it themselves before invoking start.
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopping headless scraper to release profile lock...")
	if err := systemctlUser("stop", scraperUnit); err != nil {
		// A non-systemd deployment will fail here. Log the message
		// and continue — the podman run below will fail with a
		// clear "profile dir locked" error if the scraper is still
		// holding the dir, which is more diagnostic than a generic
		// systemctl failure.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: could not stop %s via systemctl --user (%v); proceeding\n",
			scraperUnit, err)
	}

	// Build the podman run command. We deliberately do NOT pass
	// --userns=keep-id: the headless scraper service runs without
	// it, so the named volume's /profiles tree is owned by a
	// rootless-podman subuid that the operator's uid (mapped via
	// keep-id) does not have write access to. Skipping keep-id
	// makes both containers see /profiles through the same default
	// user-namespace mapping, so the login sidecar's writes are
	// readable by the scraper on its next run and vice versa. The
	// host operator never needs direct fs access to /profiles —
	// everything goes through the volume.
	args := []string{
		"run", "--rm", "-d",
		"--name", loginContainerName,
		"--pids-limit", "512",
		// Chrome stores tab-renderer shared memory in /dev/shm.
		// Podman's default is 64M which Chrome blows through on
		// any heavy page (Gmail, LinkedIn, banking portals),
		// renderer SIGBUSes, and the tab crashes — what the user
		// sees as "the browser froze". 2G is plenty for a real-
		// world manual login session and matches the value the
		// Playwright/Puppeteer ecosystem recommends.
		// (--disable-dev-shm-usage in entrypoint.sh redirects
		// most of the allocation to /tmp anyway, but small
		// fragments still land in /dev/shm and the headroom
		// avoids surprises.)
		"--shm-size=2g",
		"-p", fmt.Sprintf("%s:%d:6080", scraperLoginBind, scraperLoginPort),
		"-v", "vornik-scraper-profiles:/profiles",
		"-e", fmt.Sprintf("SCRAPER_PROJECT_ID=%s", scraperLoginProject),
		"-e", fmt.Sprintf("SCRAPER_PROFILE_NAME=%s", scraperLoginProfile),
		"-e", fmt.Sprintf("SCRAPER_LOGIN_URL=%s", scraperLoginURL),
	}
	if scraperLoginPassword != "" {
		args = append(args, "-e", fmt.Sprintf("SCRAPER_VNC_PASSWORD=%s", scraperLoginPassword))
	}
	args = append(args, loginImage)

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "starting login sidecar...")
	out, err := exec.Command("podman", args...).CombinedOutput()
	if err != nil {
		// On failure, restart the scraper so the operator isn't left
		// with both services down. The sidecar may have left a
		// half-started container behind too — clean it up.
		_ = exec.Command("podman", "rm", "-f", loginContainerName).Run()
		_ = systemctlUser("start", scraperUnit)
		return fmt.Errorf("podman run failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// Verify the container is actually still running. `podman run -d`
	// returns 0 the moment the container starts; if Xvfb / Chrome /
	// websockify fails immediately (a profile-permission error, for
	// instance), --rm tears the container down within ~50ms and
	// the user would otherwise see the "Login sidecar running"
	// banner for a container that already died.
	time.Sleep(800 * time.Millisecond)
	if running, _ := loginContainerRunning(); !running {
		// Pull the last few log lines so the operator can diagnose
		// without separately running `journalctl --user`.
		logOut, _ := exec.Command("journalctl", "--user", "-n", "20", "--no-pager",
			"-t", loginContainerName).CombinedOutput()
		_ = systemctlUser("start", scraperUnit)
		return fmt.Errorf(
			"login sidecar exited immediately after start. Last log lines:\n%s",
			strings.TrimSpace(string(logOut)),
		)
	}

	// URL host: when bound to 0.0.0.0 the operator wants to reach
	// the sidecar from another host, so print the LAN address. For
	// any specific bind we just echo it back. 127.0.0.1 stays as
	// localhost.
	urlHost := scraperLoginBind
	if scraperLoginBind == "0.0.0.0" {
		urlHost = lanIPHint()
	}

	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "── Login sidecar running ──────────────────────────────────")
	_, _ = fmt.Fprintf(w, "  Project:   %s\n", scraperLoginProject)
	_, _ = fmt.Fprintf(w, "  Profile:   %s\n", scraperLoginProfile)
	_, _ = fmt.Fprintf(w, "  Bind:      %s:%d\n", scraperLoginBind, scraperLoginPort)
	_, _ = fmt.Fprintf(w, "  noVNC URL: http://%s:%d/\n", urlHost, scraperLoginPort)
	if scraperLoginPassword != "" {
		_, _ = fmt.Fprintf(w, "  Password:  %s\n", scraperLoginPassword)
	} else if scraperLoginBind == "127.0.0.1" {
		_, _ = fmt.Fprintln(w, "  Password:  (none — bound to localhost)")
	} else {
		_, _ = fmt.Fprintln(w, "  Password:  (none — WARNING: bound non-locally without a password)")
		_, _ = fmt.Fprintln(w, "             VNC stream is unencrypted; anyone reachable on")
		_, _ = fmt.Fprintf(w, "             %s:%d can take over the session. Set --vnc-password.\n", scraperLoginBind, scraperLoginPort)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Open the URL in a browser, log into the portal, then run:")
	_, _ = fmt.Fprintln(w, "  vornikctl scraper login stop")
	_, _ = fmt.Fprintln(w, "──────────────────────────────────────────────────────────")
	return nil
}

// runScraperLoginStop is the unwind: stop the sidecar (Chrome's
// on-exit handler flushes cookies to disk first), then restart the
// headless scraper so automated workflows resume.
func runScraperLoginStop(cmd *cobra.Command, _ []string) error {
	running, _ := loginContainerRunning()
	if !running {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no login sidecar running")
	} else {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopping login sidecar (Chrome will flush cookies to disk)...")
		// `podman stop` with a 10s grace gives Chrome time for its
		// shutdown profile-flush. The container has --rm so it gets
		// removed once stopped.
		if out, err := exec.Command("podman", "stop", "-t", "10", loginContainerName).CombinedOutput(); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: podman stop failed (%v): %s\n",
				err, strings.TrimSpace(string(out)))
		}
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "restarting headless scraper...")
	if err := systemctlUser("start", scraperUnit); err != nil {
		return fmt.Errorf("could not start %s: %w", scraperUnit, err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "done. Authenticated session is live for the next scraper run.")
	return nil
}

// runScraperLoginStatus reports whether a login session is active
// and the URL to open it. Read-only — does not change state.
func runScraperLoginStatus(cmd *cobra.Command, _ []string) error {
	running, port := loginContainerRunning()
	if !running {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no login sidecar running")
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "login sidecar running; open http://127.0.0.1:%d/\n", port)
	return nil
}

// loginContainerRunning probes podman for the sidecar container.
// Returns the published host port when running so `status` can print
// the URL even if the operator forgot which port they passed to
// `start`.
func loginContainerRunning() (bool, int) {
	out, err := exec.Command(
		"podman", "ps", "--filter", "name=^"+loginContainerName+"$",
		"--format", "{{.Ports}}",
	).Output()
	if err != nil {
		return false, 0
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return false, 0
	}
	// Port string looks like "127.0.0.1:6080->6080/tcp". Best-effort
	// parse — if it doesn't match, fall back to the default. We
	// only need this for the status output.
	port := loginVNCPort
	if i := strings.Index(line, "->"); i > 0 {
		hp := line[:i]
		if j := strings.LastIndex(hp, ":"); j >= 0 {
			var p int
			if _, perr := fmt.Sscanf(hp[j+1:], "%d", &p); perr == nil && p > 0 {
				port = p
			}
		}
	}
	return true, port
}

// systemctlUser is a thin wrapper that swallows ExitError stderr
// into the returned error message so callers get one line instead of
// a cryptic "exit status 1".
func systemctlUser(verb, unit string) error {
	cmd := exec.Command("systemctl", "--user", verb, unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Stderr // keep import live in case future revisions log here
		return fmt.Errorf("%s %s: %w: %s", verb, unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lanIPHint returns a best-effort guess of an IP address an operator
// on another host could use to reach this machine. When --bind=0.0.0.0
// the actual listen address (literally "0.0.0.0") isn't a usable URL
// host, so the printed link substitutes the first non-loopback IPv4
// address we find. When detection fails we fall back to "<host>" so
// the operator sees an obvious placeholder rather than a confusing
// "0.0.0.0:6080" link.
func lanIPHint() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "<host>"
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4.String()
		}
	}
	return "<host>"
}
