package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/supportbundle"
	"vornik.io/vornik/internal/version"
)

// LOCAL COLLECTION — the Community-Edition path.
// ==============================================
//
// `POST /api/v1/support-report` sits behind the admin gate, Community ships no
// admin surface, and the call returns 501 EDITION_UNSUPPORTED. That is not a
// gap at the edge: it is the motivation of the feature, and it has already cost
// something — on 2026-08-05 Community's `vornikctl report` printed guidance
// naming `support-report` unconditionally, so a CE operator filing a bug was
// told by the product to run a command that answers only with the 501.
//
// What authorises this path (operator ruling, 2026-09-04): shell access to the
// host running Vornik. More precisely — and the precision matters — the
// mechanism is FILESYSTEM READ. config.yaml is 0600 and holds the database DSN,
// so anything that can read it already has database access, and five CLI paths
// already open the database on exactly that basis, the last of them
// (`vornikctl erase`) performing an irreversible Art 17 erasure. A read-only
// bundle is strictly less authority than a command that already ships.
//
// What the ruling does NOT authorise is relaxing the HTTP endpoint: a request
// does not prove its sender is on the host. The endpoint keeps its admin gate
// exactly as it is, and this path never touches it.
//
// See https://docs.vornik.io

// daemonIdentity is what the daemon says about itself — the answer to "whose
// version is in this bundle" (design §4.1). The field a support engineer trusts
// first must not quietly become the wrong build's: the client and the daemon
// CAN differ, and did on the reference host for part of 2026-09-04.
type daemonIdentity struct {
	Version   string
	Edition   string
	Reachable bool
	// Err is why the probe failed, kept verbatim for the bundle rather than
	// collapsed to a bool: "connection refused" and "401" are different
	// diagnoses of the same unreachability.
	Err string
}

// probeDaemonIdentity asks the daemon for its version and edition. It never
// fails the collection — an unreachable daemon is the case the local path
// exists for.
func probeDaemonIdentity(client supportHTTPClient) daemonIdentity {
	resp, err := client.Get("/api/v1/capabilities")
	if err != nil {
		return daemonIdentity{Err: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return daemonIdentity{Err: fmt.Sprintf("capabilities returned HTTP %d", resp.StatusCode)}
	}
	var caps struct {
		Version string `json:"version"`
		Edition string `json:"edition"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		return daemonIdentity{Err: "capabilities response did not decode: " + err.Error()}
	}
	id := daemonIdentity{Version: strings.TrimSpace(caps.Version), Reachable: true}
	// An older daemon has no edition field. Reporting the CLI's edition beside
	// the daemon's version would be a mixed answer, so leave it empty and let
	// the provenance record say the edition is unknown.
	id.Edition = strings.TrimSpace(caps.Edition)
	return id
}

// collectionProvenance is written into the bundle as collection.json AND
// mirrored into MANIFEST.json. A reader who does not know which path produced
// an archive cannot tell "Community has no metrics endpoint" from "the daemon
// was down", and those are opposite diagnoses.
type collectionProvenance struct {
	// Path is "daemon" or "local".
	Path string `json:"path"`
	// VersionSource is "daemon" or "cli" — whose build the version field
	// describes.
	VersionSource   string `json:"version_source"`
	Version         string `json:"version"`
	Edition         string `json:"edition"`
	CLIVersion      string `json:"cli_version"`
	CLIEdition      string `json:"cli_edition"`
	DaemonReachable bool   `json:"daemon_reachable"`
	DaemonVersion   string `json:"daemon_version,omitempty"`
	DaemonEdition   string `json:"daemon_edition,omitempty"`
	DaemonError     string `json:"daemon_error,omitempty"`
	Note            string `json:"note"`
}

// resolveProvenance applies §4.1: the daemon's version and edition when it is
// reachable, because the question the field answers is what produced this
// deployment's behaviour; otherwise the CLI's own, labelled as the CLI's.
func resolveProvenance(id daemonIdentity) collectionProvenance {
	p := collectionProvenance{
		Path:            "local",
		CLIVersion:      Version,
		CLIEdition:      version.NormalizeEdition(edition),
		DaemonReachable: id.Reachable,
		DaemonVersion:   id.Version,
		DaemonEdition:   id.Edition,
		DaemonError:     id.Err,
	}
	if id.Reachable && id.Version != "" {
		p.VersionSource = "daemon"
		p.Version = id.Version
		p.Edition = id.Edition
		if p.Edition == "" {
			// A daemon too old to report its edition. Say so rather than
			// borrowing the CLI's, which may be the other one.
			p.Edition = "unknown"
			p.Note = "collected locally by vornikctl; the daemon reported a version but not an edition (older build)"
			return p
		}
		p.Note = "collected locally by vornikctl; version and edition are the DAEMON's"
		return p
	}
	p.VersionSource = "cli"
	p.Version = Version
	p.Edition = p.CLIEdition
	p.Note = "collected locally by vornikctl with the daemon unreachable; version and edition are the CLI's, " +
		"which may differ from the daemon build this deployment runs"
	return p
}

// unavailableSource stands in for health and metrics on the local path. Those
// two sections are the daemon's live state and there is no offline equivalent
// — so they are recorded as SECTION ERRORS saying why, which is the bundle's
// rule (support-report design §7) and the reason it has a section_errors map at
// all. Omitting them silently would leave a reader unable to tell an absent
// section from a broken one.
type unavailableSource struct{ why string }

func (u unavailableSource) Snapshot(_ context.Context) (any, error) { return nil, errors.New(u.why) }

// metricsUnavailable is the MetricsSource shape of the same thing.
type metricsUnavailable struct{ why string }

func (m metricsUnavailable) Snapshot(_ context.Context) (string, error) {
	return "", errors.New(m.why)
}

// offlineDoctorRunner adapts the daemon-down doctor to the builder's seam. It
// is weaker than the daemon's doctor — static checks only — and the bundle says
// so in the report itself.
type offlineDoctorRunner struct {
	// cfg is the config the caller ALREADY loaded. It is not re-loaded here:
	// config.Load() registers process-global flags on every call, so a second
	// load in one command panics with "flag redefined".
	cfg     *config.Config
	cfgPath string
}

func (d offlineDoctorRunner) Run(ctx context.Context) (any, error) {
	report, cfgPath := buildOfflineDoctorReportFrom(ctx, d.cfg, d.cfgPath)
	return map[string]any{
		"mode":        "offline",
		"config_path": cfgPath,
		"note": "collected by vornikctl without the daemon: STATIC checks only " +
			"(config parse, database reachability, migration state, journal errors). " +
			"The daemon's doctor runs a larger set.",
		"report": report,
	}, nil
}

// collectLocalBundle builds the bundle in-process from the database, the
// registry and the config file. It is the SAME collector the daemon drives —
// internal/supportbundle — because the thing a second collector would duplicate
// is the redaction path, and a second redaction path is a second way to leak.
// loadSupportConfig is the config source for local collection. It is a var
// because config.Load() also applies VORNIK_* environment overrides: a test
// that let it run would collect from whatever database the developer's shell
// points at — on the reference host, the production one.
var loadSupportConfig = config.Load

func collectLocalBundle(ctx context.Context, detector secrets.Detector, opts supportReportOptions, prov collectionProvenance) (*supportbundle.Result, error) {
	cfg, cfgPath, err := loadSupportConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return collectLocalBundleFrom(ctx, cfg, cfgPath, detector, opts, prov)
}

// collectLocalBundleFrom is the same collection against an EXPLICIT config.
// The split is not cosmetic: config.Load() also applies VORNIK_* environment
// overrides, so a test that let it run would open whatever database the
// developer's shell happens to point at — on the reference host, the
// production one.
func collectLocalBundleFrom(ctx context.Context, cfg *config.Config, cfgPath string, detector secrets.Detector, opts supportReportOptions, prov collectionProvenance) (*supportbundle.Result, error) {
	return collectLocalBundleFromWithDoctor(ctx, cfg, cfgPath, detector, opts, prov,
		offlineDoctorRunner{cfg: cfg, cfgPath: cfgPath})
}

// collectLocalBundleFromWithDoctor takes the doctor as a parameter. The
// structural-parity test supplies a deterministic one so doctor.json can join
// the byte-for-byte comparison rather than being excluded from it; production
// always passes the offline doctor above.
func collectLocalBundleFromWithDoctor(ctx context.Context, cfg *config.Config, cfgPath string, detector secrets.Detector, opts supportReportOptions, prov collectionProvenance, doctor supportbundle.DoctorRunner) (*supportbundle.Result, error) {
	// OpenReadOnly, not Open: collection must not migrate the schema. A CLI one
	// build ahead of the daemon running Open's SQLite branch would move the
	// database forward under a running daemon while the operator believed they
	// were only gathering evidence.
	backend, err := storage.OpenReadOnly(ctx, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("open database (%s): %w", cfgPath, err)
	}
	defer func() { _ = backend.Close() }()

	b := &supportbundle.Builder{
		Detector: detector,
		Version:  prov.Version,
		Edition:  prov.Edition,
		// No blackbox trace: it is an Enterprise service reached through the
		// daemon, so on this path it is absent by construction rather than
		// broken. collectBlackBoxTrace omits the section when nil, and
		// collection.json records the path so a reader knows which it is.
		Health:  unavailableSource{why: localSectionUnavailable("health.json")},
		Metrics: metricsUnavailable{why: localSectionUnavailable("metrics.txt")},
		Doctor:  doctor,
	}

	if r := backend.Repos; r != nil {
		b.Repos = supportbundle.Repos{
			Tasks:       r.Tasks,
			Executions:  r.Executions,
			Outcomes:    r.StepOutcomes,
			ToolAudit:   r.ToolAudit,
			LLMUsage:    r.LLMUsage,
			Messages:    r.Messages,
			JudgeVerdct: r.JudgeVerdicts,
			PostMortem:  r.PostMortems,
			Artifacts:   r.Artifacts,
			AdminAudit:  r.AdminAudit,
		}
		if r.Webhooks != nil {
			b.Webhooks = r.Webhooks
		}
	}

	// The deployed registry — the same filesystem load the daemon does, and
	// verified daemon-independent: internal/registry has no HTTP client and no
	// dial, so it loads with the daemon down. Its absence is half a diagnosis:
	// a bundle carrying every execution row but no prompt cannot show a
	// workflow whose prompt asserts something the executor never sent.
	if dir := resolveConfigsDir(cfgPath); dir != "" {
		reg := registry.New()
		// Tolerate validation errors for the same reason the doctor checks do:
		// the point is to show what the operator's YAML says, not to refuse the
		// bundle because one unrelated project is misconfigured.
		var valErr *registry.ValidationError
		if lerr := reg.Load(dir); lerr == nil || errors.As(lerr, &valErr) {
			b.Registry = reg
		}
	}

	yamlSnapshot, yerr := supportbundle.RedactedConfigYAML(cfg)
	if yerr == nil {
		b.ConfigYAML = yamlSnapshot
	}

	req, err := localBundleRequest(opts)
	if err != nil {
		return nil, err
	}

	// collection.json is NOT written here: executeSupportReport writes it into
	// the staging tree on BOTH paths, so the daemon bundle and the local one
	// carry the same provenance record in the same place.
	return b.Build(ctx, req)
}

// localSectionUnavailable is the section-error text. It names the section, the
// reason, and what to do about it — a reader of section_errors otherwise
// cannot tell a Community deployment from a broken one.
func localSectionUnavailable(section string) string {
	return "unavailable on the local collection path: " + section +
		" is the daemon's live state and has no offline equivalent. " +
		"Re-run with the daemon up (and, on Enterprise, without --local) to include it."
}

// localBundleRequest resolves the CLI flags into the collector's Request —
// through the collector's OWN parser, so --since means the same thing on both
// paths.
func localBundleRequest(opts supportReportOptions) (supportbundle.Request, error) {
	req := supportbundle.Request{MaxSize: opts.MaxSize, IncludeRaw: opts.IncludeRaw}
	if req.MaxSize <= 0 {
		req.MaxSize = supportDefaultMaxSize
	}
	if t := strings.TrimSpace(opts.Task); t != "" {
		req.TaskID = t
		return req, nil
	}
	since, until, err := supportbundle.ParseWindow(opts.Since, opts.Until)
	if err != nil {
		return supportbundle.Request{}, err
	}
	req.Window = true
	req.Since = since
	req.Until = until
	return req, nil
}

// stageBundleFiles writes the in-memory tree to the staging dir the host
// sections are appended to — the same shape the daemon path produces after
// unpacking its archive, so everything downstream is identical.
func stageBundleFiles(dir string, files map[string][]byte) error {
	for name, content := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		// The names are collector-constructed, never operator input, but a
		// staging writer that trusts its input is how a path traversal gets in
		// later. Refuse anything that resolves outside the staging dir.
		if rel, err := filepath.Rel(dir, target); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("refusing to stage %q: it resolves outside the staging directory", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	return nil
}
