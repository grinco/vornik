package api

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// realBakedUID runs `id -u` inside the agent image. The Containerfile sets
// USER vornik:vornik — a NAME, not a numeric uid — so `podman image
// inspect` cannot yield the baked uid; running id -u inside the image
// returns the effective uid the workload actually runs as. See design F3b.
func realBakedUID(ctx context.Context, image string) (int, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "podman", "run", "--rm", "--entrypoint", "id", image, "-u").Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// subuidProvisioned reports whether the current user has /etc/subuid and
// /etc/subgid entries and newuidmap is on PATH — the prerequisites
// userns_mode=keep-id needs to actually remap the container's uid. Without
// these, podman fails to start keep-id containers with an inscrutable error
// rather than a diagnosable one; this is the preflight half of F3b.
func subuidProvisioned() bool {
	if _, err := exec.LookPath("newuidmap"); err != nil {
		return false
	}
	u, err := user.Current()
	if err != nil {
		return false
	}
	has := func(path string) bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, u.Username+":") || strings.HasPrefix(line, u.Uid+":") {
				return true
			}
		}
		return false
	}
	return has("/etc/subuid") && has("/etc/subgid")
}

// checkAgentImageUID is the doctor check guarding the SECOND guard for the
// rootless workspace "Permission denied" incident (F3b, see also Task 1's
// userns_mode=keep-id and the related checkPodmanConfig subuid warning).
// keep-id maps the container's baked uid to the host uid at container
// start, but ONLY when the image's baked uid matches — a bare `podman
// build` (bypassing `make build-agent`, which auto-sets VORNIK_UID/GID)
// bakes the Containerfile default uid 1000, and keep-id cannot bridge a
// mismatch: the workspace bind-mount is still owned by the host uid, so
// writes from the container's real (mismatched) uid still fail with
// permission denied. This check also preflights keep-id's own prerequisites
// (/etc/subuid, /etc/subgid, newuidmap) so a keep-id host missing them
// fails here diagnosably instead of at container start.
//
// bakedUIDFunc and subuidOKFunc are injectable seams (nil ⇒ realBakedUID /
// subuidProvisioned) so unit tests never shell out to podman or touch
// /etc/subuid.
func (h *DoctorHandlers) checkAgentImageUID(ctx context.Context) DoctorCheck {
	name := "agent_image_uid"

	// keep-id preflight runs FIRST and unconditionally — it is a host-level
	// concern (does this machine have the subuid/subgid/newuidmap prereqs
	// keep-id needs?) independent of whether any config directory or agent
	// image is configured yet. Gating it behind the configDir=="" guard
	// below would let a keep-id host with no configDir report a false OK.
	if h.usernsMode == "keep-id" {
		subuidOK := h.subuidOKFunc
		if subuidOK == nil {
			subuidOK = subuidProvisioned
		}
		if !subuidOK() {
			return DoctorCheck{
				Name:   name,
				Status: "ERROR",
				Message: "userns_mode=keep-id but /etc/subuid|/etc/subgid entries or newuidmap are missing; " +
					"agent containers will fail to start. Fix: `sudo usermod --add-subuids 100000-165535 " +
					"--add-subgids 100000-165535 $USER` then `podman system migrate`.",
			}
		}
	}

	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config directory, skipping"}
	}

	// Image resolution runs unconditionally — in production AND in tests —
	// so the SKIPPED branch is reachable and assertable regardless of
	// whether bakedUIDFunc is injected.
	image, err := firstAgentImage(h.configDir)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not resolve agent image: " + err.Error()}
	}
	if image == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no agent image configured"}
	}

	baked := h.bakedUIDFunc
	if baked == nil {
		baked = realBakedUID
	}
	uid, err := baked(ctx, image)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read agent image uid: " + err.Error()}
	}
	host := os.Getuid()
	if uid == host {
		return DoctorCheck{Name: name, Status: "OK", Message: "agent image uid matches host uid (keep-id maps it to the workspace owner)"}
	}
	return DoctorCheck{
		Name:   name,
		Status: "ERROR",
		Message: "agent image built for uid " + strconv.Itoa(uid) + " but the daemon runs as uid " + strconv.Itoa(host) +
			"; keep-id cannot bridge this and rootless workspace writes will fail. Rebuild with `make build-agent` " +
			"(auto-matches your uid) — never a bare `podman build`.",
	}
}
