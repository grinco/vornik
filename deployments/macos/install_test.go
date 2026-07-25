// Package macos holds the macOS Lima-VM installer artifacts (the "one-liner"
// path from https://docs.vornik.io). These
// tests read the shipped shell/YAML text and assert on its structure — they
// do NOT boot Lima or exercise a real VM (that needs a Mac; see the manual
// smoke step in the design's §6). Mirrors the
// deployments/podman/*_test.go pattern of reading a deployment artifact by
// path and asserting string/structural properties on it.
package macos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoFile reads a file by REPO-ROOT-relative path (e.g.
// "deployments/lima/vornik.yaml"), regardless of the test binary's working
// directory. It walks up from this source file's own directory looking for
// go.mod, the repo root marker.
func repoFile(t *testing.T, relPath string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := dir
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("could not find repo root (go.mod) above %s", dir)
		}
		root = parent
	}
	full := filepath.Join(root, relPath)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("failed to read %s: %v", relPath, err)
	}
	return string(data)
}

// --- deployments/lima/vornik.yaml ------------------------------------------

// limaTemplate is the minimal shape this test cares about — just enough of
// deployments/lima/vornik.yaml's schema to assert the required keys are
// present, not a full Lima config model.
type limaTemplate struct {
	PortForwards []struct {
		GuestPort int    `yaml:"guestPort"`
		HostPort  int    `yaml:"hostPort"`
		HostIP    string `yaml:"hostIP"`
	} `yaml:"portForwards"`
	Mounts []struct {
		Location   string `yaml:"location"`
		MountPoint string `yaml:"mountPoint"`
		Writable   bool   `yaml:"writable"`
	} `yaml:"mounts"`
	Provision []struct {
		Mode   string `yaml:"mode"`
		Boot   bool   `yaml:"boot"`
		Script string `yaml:"script"`
	} `yaml:"provision"`
}

// TestLimaTemplateParsesAsYAML is the design's (§6) "YAML-parse + required-
// key check" fallback for `limactl validate`, which needs a Mac to run. It
// asserts deployments/lima/vornik.yaml (a) parses as valid YAML via the
// repo's own YAML lib, and (b) has the required keys: a portForwards entry
// for guest 8080, a mounts entry for the config dir, and a provision entry.
// This runs under `go test ./...`, so CI catches a malformed/incomplete
// template with no limactl available.
func TestLimaTemplateParsesAsYAML(t *testing.T) {
	tmpl := repoFile(t, "deployments/lima/vornik.yaml")

	var parsed limaTemplate
	if err := yaml.Unmarshal([]byte(tmpl), &parsed); err != nil {
		t.Fatalf("deployments/lima/vornik.yaml failed to parse as YAML: %v", err)
	}

	foundPortForward8080 := false
	for _, pf := range parsed.PortForwards {
		if pf.GuestPort == 8080 {
			foundPortForward8080 = true
			break
		}
	}
	if !foundPortForward8080 {
		t.Error("vornik.yaml must parse with a portForwards entry for guestPort 8080")
	}

	foundConfigMount := false
	for _, m := range parsed.Mounts {
		if strings.Contains(m.MountPoint, ".config/vornik") || strings.Contains(m.Location, ".config/vornik") {
			foundConfigMount = true
			break
		}
	}
	if !foundConfigMount {
		t.Error("vornik.yaml must parse with a mounts entry for the config dir (~/.config/vornik)")
	}

	if len(parsed.Provision) == 0 {
		t.Error("vornik.yaml must parse with at least one provision entry")
	}
}

// TestLimaTemplatePortForward — the daemon API/UI (default 8080, see
// design §3.2) must be forwarded to the mac's loopback, never a wider bind.
func TestLimaTemplatePortForward(t *testing.T) {
	tmpl := repoFile(t, "deployments/lima/vornik.yaml")
	if !strings.Contains(tmpl, "portForwards:") {
		t.Fatal("vornik.yaml must define portForwards")
	}
	if !strings.Contains(tmpl, "guestPort: 8080") {
		t.Error("vornik.yaml must forward guestPort 8080 (the daemon API/UI)")
	}
	if !strings.Contains(tmpl, "hostPort: 8080") {
		t.Error("vornik.yaml must forward to hostPort 8080")
	}
	if !strings.Contains(tmpl, `hostIP: "127.0.0.1"`) {
		t.Error("vornik.yaml must bind the host side of the forward to 127.0.0.1 (not 0.0.0.0 — F6 trust boundary, design §5)")
	}
}

// TestLimaTemplateConfigOnlyMount — only ~/.config/vornik is host-mounted
// (writable, mac<->VM). The data dir (~/.local/share/vornik, Postgres
// included) must stay VM-internal — see design §3.4 (F3): virtiofs cannot
// guarantee fsync durability for Postgres's WAL.
func TestLimaTemplateConfigOnlyMount(t *testing.T) {
	tmpl := repoFile(t, "deployments/lima/vornik.yaml")
	if !strings.Contains(tmpl, ".config/vornik") {
		t.Error("vornik.yaml must mount ~/.config/vornik")
	}
	if !strings.Contains(tmpl, "writable: true") {
		t.Error("vornik.yaml's config mount must be writable: true")
	}
	if strings.Contains(tmpl, ".local/share/vornik") {
		t.Error("vornik.yaml must NOT mount ~/.local/share/vornik (data dir) — Postgres data must stay on the VM's own ext4/xfs, never virtiofs (design §3.4, F3)")
	}
}

// TestLimaTemplateProvisionHookSentinelGuarded — the provision hook runs as
// root at boot but must be idempotent (Lima re-runs `provision:` on every
// boot), and must leave the VM in the state quickstart.sh assumes: subuid/
// subgid seeded for the pinned VM user, and that user's login lingering
// enabled so systemd --user survives a non-interactive `limactl shell`.
func TestLimaTemplateProvisionHookSentinelGuarded(t *testing.T) {
	tmpl := repoFile(t, "deployments/lima/vornik.yaml")
	if !strings.Contains(tmpl, "provision:") {
		t.Fatal("vornik.yaml must define a provision: hook")
	}
	if !strings.Contains(tmpl, "mode: system") {
		t.Error("the provision hook must run mode: system (root) — subuid/subgid + linger need root")
	}
	if !strings.Contains(tmpl, "boot: true") {
		t.Error("the provision hook must run at boot: true")
	}
	if !strings.Contains(tmpl, "subuid") || !strings.Contains(tmpl, "subgid") {
		t.Error("the provision hook must seed /etc/subuid and /etc/subgid for the pinned VM user")
	}
	if !strings.Contains(tmpl, "enable-linger") {
		t.Error("the provision hook must loginctl enable-linger the pinned VM user")
	}
	// Sentinel guard: some marker-file check so the (idempotent but
	// non-trivial: subuid seeding + linger + podman first-run) work
	// happens once, not on every `limactl start`.
	hasSentinel := strings.Contains(tmpl, "SENTINEL") || strings.Contains(tmpl, "sentinel") ||
		strings.Contains(tmpl, "already provisioned") || strings.Contains(tmpl, "-f ") && strings.Contains(tmpl, "exit 0")
	if !hasSentinel {
		t.Error("the provision hook must be sentinel/marker-file guarded so it runs once, not every boot")
	}
}

// TestLimaTemplatePinnedUser — the VM user is pinned (not left to Lima's
// default host-username-matching behavior), so the provision hook and
// quickstart target a stable, known account across `limactl delete`+recreate.
func TestLimaTemplatePinnedUser(t *testing.T) {
	tmpl := repoFile(t, "deployments/lima/vornik.yaml")
	if !strings.Contains(tmpl, "user:") {
		t.Fatal("vornik.yaml must pin a user: block (not Lima's default host-username guest user)")
	}
	if !strings.Contains(tmpl, "ubuntu") {
		t.Error("vornik.yaml should pin the Ubuntu image's default 'ubuntu' user")
	}
}

// TestLimaTemplateBackendPlaceholder — vz on Apple Silicon / qemu on Intel is
// pinned per design §3.1, not left to Lima's auto-detection; if the template
// itself can't conditionally select per-arch, install.sh must document and
// fill the choice at `limactl start` time (vmType/--vm-type).
func TestLimaTemplateBackendPlaceholder(t *testing.T) {
	tmpl := repoFile(t, "deployments/lima/vornik.yaml")
	if !strings.Contains(tmpl, "vmType") {
		t.Error("vornik.yaml must reference vmType (vz/qemu selection), even if install.sh overrides it at launch")
	}
}

// --- deployments/macos/install.sh -------------------------------------------

func TestInstallShDelegatesToQuickstart(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "#!/usr/bin/env bash") {
		t.Error("install.sh must use #!/usr/bin/env bash")
	}
	if !strings.Contains(sh, "set -euo pipefail") {
		t.Error("install.sh must set -euo pipefail")
	}
	if !strings.Contains(sh, "quickstart.sh") {
		t.Error("install.sh must delegate to the unmodified Linux quickstart.sh inside the VM")
	}
}

func TestInstallShAssertsDarwin(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "uname -s") || !strings.Contains(sh, "Darwin") {
		t.Error("install.sh must assert uname -s = Darwin before proceeding")
	}
}

func TestInstallShAssertsNotRoot(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "id -un") {
		t.Error("install.sh must assert `id -un` (the in-VM user) is not root before invoking quickstart (design §3.2 step 3)")
	}
	if !strings.Contains(sh, "root") {
		t.Error("install.sh must check the in-VM user against 'root'")
	}
}

func TestInstallShProbesVzFloor(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "sw_vers") {
		t.Error("install.sh must probe sw_vers for the vz macOS-13 floor")
	}
	if !strings.Contains(sh, "qemu") {
		t.Error("install.sh must fall back to qemu when vz is unavailable (pre-macOS-13)")
	}
}

func TestInstallShPicksArchViaUnameM(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "uname -m") {
		t.Error("install.sh must pick guest image arch via uname -m")
	}
	if !strings.Contains(sh, "arm64") || !strings.Contains(sh, "amd64") {
		t.Error("install.sh must map uname -m to arm64/amd64")
	}
}

func TestInstallShHasTestableHelperFunctions(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	for _, fn := range []string{"vm_exists", "pick_arch", "pick_backend"} {
		if !strings.Contains(sh, fn+"(") && !strings.Contains(sh, fn+" (") {
			t.Errorf("install.sh must factor a testable %s() function", fn)
		}
	}
}

func TestInstallShEnvTunableKnobs(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	for _, envVar := range []string{"VORNIK_VM_CPUS", "VORNIK_VM_MEM", "VORNIK_VM_DISK", "VORNIK_HTTP_PORT"} {
		if !strings.Contains(sh, envVar) {
			t.Errorf("install.sh must expose the env-tunable knob %s", envVar)
		}
	}
}

func TestInstallShDoesNotInterpolateEnvIntoGuestShell(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	for _, unsafe := range []string{"'${QUICKSTART_URL}'", "'${HTTP_PORT}'"} {
		if strings.Contains(sh, unsafe) {
			t.Errorf("install.sh interpolates attacker-controlled environment into bash -c: %s", unsafe)
		}
	}
	if !strings.Contains(sh, `VORNIK_QUICKSTART_URL="$QUICKSTART_URL"`) {
		t.Error("quickstart URL must be passed as an argument-safe env value")
	}
	if !strings.Contains(sh, `VORNIK_HTTP_PORT="$HTTP_PORT"`) {
		t.Error("HTTP port must be passed as an argument-safe env value")
	}
	if !strings.Contains(sh, `https://*`) {
		t.Error("quickstart URL override must be restricted to HTTPS")
	}
}

func TestInstallShPinsNestedQuickstartToVornikRef(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, `REF="${VORNIK_REF:-`) {
		t.Error("install.sh must consume the release ref propagated by the Linux entry point")
	}
	if !strings.Contains(sh, `grinco/vornik/${REF}/deployments/podman/quickstart.sh`) {
		t.Error("nested quickstart fetch must use the pinned VORNIK_REF, not a moving branch")
	}
	if strings.Contains(sh, "grinco/vornik/main/deployments/podman/quickstart.sh") {
		t.Error("nested quickstart must not silently fetch moving main")
	}
}

func TestInstallShVerifiesNestedQuickstartChecksum(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "quickstart.sh.sha256") {
		t.Error("nested installer must fetch the published quickstart checksum")
	}
	if !strings.Contains(sh, "sha256sum -c") {
		t.Error("nested installer must verify quickstart before execution")
	}
	checkIdx := strings.Index(sh, "sha256sum -c")
	runIdx := strings.LastIndex(sh, "/tmp/quickstart.sh")
	if checkIdx < 0 || runIdx < 0 || checkIdx >= runIdx {
		t.Error("checksum verification must occur before quickstart execution")
	}
}

func TestInstallShStartsLimaVmByName(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "limactl start") {
		t.Error("install.sh must limactl start the VM")
	}
	if !strings.Contains(sh, "--name vornik") && !strings.Contains(sh, `--name "vornik"`) && !strings.Contains(sh, "--name=vornik") {
		t.Error("install.sh must name the VM 'vornik' (the vornikctl shim and vornik.yaml template both assume this)")
	}
	if !strings.Contains(sh, "deployments/lima/vornik.yaml") {
		t.Error("install.sh must start from deployments/lima/vornik.yaml")
	}
}

func TestInstallShInstallsShim(t *testing.T) {
	sh := repoFile(t, "deployments/macos/install.sh")
	if !strings.Contains(sh, "vornikctl") {
		t.Error("install.sh must install the vornikctl shim to the mac PATH")
	}
}

// --- deployments/macos/vornikctl --------------------------------------------

func TestShimShebangAndStrict(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")
	if !strings.Contains(sh, "#!/usr/bin/env bash") {
		t.Error("vornikctl shim must use #!/usr/bin/env bash")
	}
	if !strings.Contains(sh, "set -euo pipefail") {
		t.Error("vornikctl shim must set -euo pipefail")
	}
}

func TestShimStatusLogsIntercept(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")
	if !strings.Contains(sh, "systemctl --user status vornik") {
		t.Error("vornikctl shim's status must also surface `systemctl --user status vornik` in the VM")
	}
	if !strings.Contains(sh, "journalctl --user -u vornik") {
		t.Error("vornikctl shim's logs must run `journalctl --user -u vornik` in the VM")
	}
}

func TestShimDeleteGuard(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")
	if !strings.Contains(sh, "--force") {
		t.Fatal("vornikctl shim's delete must require --force")
	}
	if !strings.Contains(sh, "vornikctl backup") {
		t.Error(`vornikctl shim's delete guard must print "run vornikctl backup <path> first"`)
	}
	if !strings.Contains(sh, "destroys all") {
		t.Error("vornikctl shim's delete guard must warn this destroys all in-VM data")
	}
}

// TestShimDeleteGuardPrecedesGenericForward is the fall-through-bug test the
// design calls out explicitly (§6): `delete` must be handled BEFORE the
// generic `limactl shell vornik vornikctl "$@"` catch-all forward, or an
// unguarded `vornikctl delete` would silently reach `limactl delete`.
func TestShimDeleteGuardPrecedesGenericForward(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")

	deleteIdx := strings.Index(sh, "delete)")
	if deleteIdx == -1 {
		deleteIdx = strings.Index(sh, `"delete"`)
	}
	if deleteIdx == -1 {
		t.Fatal("vornikctl shim must have a delete) case")
	}

	forwardIdx := strings.Index(sh, `limactl shell vornik vornikctl "$@"`)
	if forwardIdx == -1 {
		t.Fatal(`vornikctl shim must have a generic limactl shell vornik vornikctl "$@" forward`)
	}

	if deleteIdx >= forwardIdx {
		t.Error("the delete) case must be handled BEFORE the generic catch-all forward, or `vornikctl delete` silently reaches `limactl delete` (the fall-through bug design §6 calls out)")
	}
}

func TestShimVmMissingDetection(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")
	if !strings.Contains(sh, "limactl list") {
		t.Error("vornikctl shim must detect VM-missing/stopped via `limactl list` rather than hanging")
	}
}

func TestShimUpdateNotesTwoSurfaces(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")
	if !strings.Contains(sh, "update") {
		t.Fatal("vornikctl shim must handle update")
	}
	if !strings.Contains(sh, "install.sh") {
		t.Error("vornikctl shim's update must note the mac-side tooling surface is updated by re-running install.sh")
	}
}

// TestShimUpdateShiftsArgBeforeForward regression-tests the duplicate-arg
// bug a review found: without a `shift` before its forward, `vornikctl
// update` would run `... vornikctl update update` in the VM (the literal
// "update" token never popped off "$@"). Mirrors the delete-ordering test's
// style: locate the `update)` case body and assert it contains a `shift`
// before its forward line.
func TestShimUpdateShiftsArgBeforeForward(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")

	updateIdx := strings.Index(sh, "update)")
	if updateIdx == -1 {
		t.Fatal("vornikctl shim must have an update) case")
	}

	forwardMarker := `exec limactl shell vornik vornikctl update "$@"`
	forwardIdx := strings.Index(sh[updateIdx:], forwardMarker)
	if forwardIdx == -1 {
		t.Fatal(`vornikctl shim's update) case must forward via exec limactl shell vornik vornikctl update "$@"`)
	}
	forwardIdx += updateIdx

	body := sh[updateIdx:forwardIdx]
	if !strings.Contains(body, "shift") {
		t.Error("vornikctl shim's update) case must `shift` off the 'update' token before forwarding \"$@\", " +
			"or `vornikctl update` runs `... vornikctl update update` in the VM (duplicate-arg bug)")
	}
}

func TestShimGenericForward(t *testing.T) {
	sh := repoFile(t, "deployments/macos/vornikctl")
	if !strings.Contains(sh, `limactl shell vornik vornikctl "$@"`) {
		t.Error(`vornikctl shim must forward unrecognized commands via limactl shell vornik vornikctl "$@"`)
	}
}
