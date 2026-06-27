package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/ui"
)

// runtimeReadinessProbe satisfies ui.RuntimeReadinessSource. Reads
// the daemon's resolved Config.Voice + Config.Storage at request
// time and probes binary / model / bucket reachability. Kept thin
// — the actual probe helpers (stat for filesystem-shaped checks,
// HeadBucket-shaped check via the same FileBackend the daemon uses)
// live below as package-private helpers so unit tests can pin each
// behaviour without spinning up a Container.
//
// The probe is best-effort. A failure on one row (e.g. ffmpeg not
// installed) shows the row red AND lets every other row render —
// the operator's diagnostic loop is "find the one thing that's
// wrong," not "is the page healthy?"
type runtimeReadinessProbe struct {
	cfg *config.Config
}

func newRuntimeReadinessProbe(cfg *config.Config) ui.RuntimeReadinessSource {
	return &runtimeReadinessProbe{cfg: cfg}
}

// VoiceStatus probes the configured STT + TTS provider's binary +
// model + ffmpeg paths. Mirrors container_voice.go's boot probes
// (probeBinary + probeModel) but renders to a slice of result rows
// instead of WARN log lines.
func (p *runtimeReadinessProbe) VoiceStatus(_ context.Context) ui.VoiceRuntimeStatus {
	out := ui.VoiceRuntimeStatus{
		STTProvider: strings.TrimSpace(p.cfg.Voice.STT.Provider),
		TTSProvider: strings.TrimSpace(p.cfg.Voice.TTS.Provider),
	}
	// STT block.
	if out.STTProvider != "" {
		stt := p.cfg.Voice.STT
		out.Probes = append(out.Probes,
			probeBinaryRow("Whisper binary", stt.BinaryPath, []string{"whisper-cpp", "whisper-cli", "main"}),
			probeModelRow("Whisper model", stt.Model),
			probeBinaryRow("ffmpeg (STT)", stt.FFmpegPath, []string{"ffmpeg"}),
		)
	}
	// TTS block.
	if out.TTSProvider != "" {
		tts := p.cfg.Voice.TTS
		out.Probes = append(out.Probes,
			probeBinaryRow("Piper binary", tts.BinaryPath, []string{"piper"}),
			probeModelRow("Piper voice model", tts.Voice),
			probeBinaryRow("ffmpeg (TTS)", tts.FFmpegPath, []string{"ffmpeg"}),
		)
	}
	return out
}

// StorageStatus reports the active artifact FileBackend. For
// filesystem we stat the artifacts directory + try a tiny
// touch/remove for writability. For S3 we report the configured
// values; live HeadBucket reachability is left for a follow-up to
// keep this batch's wire-up small (the operator can see the
// settings; a reachability button would be the next step).
func (p *runtimeReadinessProbe) StorageStatus(_ context.Context) ui.StorageRuntimeStatus {
	backend := p.cfg.Storage.NormalizedBackend()
	out := ui.StorageRuntimeStatus{Backend: backend}
	switch backend {
	case "filesystem":
		out.FilesystemPath = p.cfg.Storage.ArtifactsPath
		writable, err := probeFilesystemWritable(out.FilesystemPath)
		out.FilesystemWritable = writable
		if err != nil {
			out.FilesystemError = err.Error()
		}
	case "s3":
		s3 := p.cfg.Storage.S3
		out.S3Endpoint = s3.Endpoint
		out.S3Region = s3.Region
		out.S3Bucket = s3.Bucket
		out.S3Prefix = s3.Prefix
		out.S3UsePathStyle = s3.UsePathStyle
		// Live HeadBucket probe is deferred — the SDK client needs
		// the same construction the daemon uses, and threading it
		// through here would couple this file to the s3 package.
		// Operators get the configured shape; reachability tracks
		// via the same admin-readiness path the api/healthz handler
		// already exercises.
		out.S3Reachable = false
		out.S3Error = "live reachability probe deferred — configuration shown below; see /readyz for the daemon's own probe"
	}
	return out
}

// probeBinaryRow stats the configured binary path; falls back to
// $PATH lookup against the supplied candidate list when the
// configured path is empty. Returns a ui.VoiceProbeStatus filled
// in for the table renderer.
func probeBinaryRow(label, configured string, pathCandidates []string) ui.VoiceProbeStatus {
	row := ui.VoiceProbeStatus{Label: label, Configured: true}
	configured = strings.TrimSpace(configured)
	if configured == "" {
		// Fall back to PATH lookup. configured stays empty in the
		// rendered table because the operator hasn't pinned it;
		// the resolved Path field shows where it was found.
		row.Configured = false
		for _, name := range pathCandidates {
			if p, err := exec.LookPath(name); err == nil {
				row.Path = p
				row.OK = true
				return row
			}
		}
		row.Error = fmt.Sprintf("not found on $PATH (tried %s)", strings.Join(pathCandidates, ", "))
		return row
	}
	row.Path = configured
	info, err := os.Stat(configured)
	switch {
	case err == nil && info.Mode()&0o111 == 0:
		row.Error = fmt.Sprintf("file exists but not executable (mode=%s)", info.Mode())
	case err == nil:
		row.OK = true
	case os.IsNotExist(err):
		row.Error = "file not found at configured path"
	default:
		row.Error = err.Error()
	}
	return row
}

// probeModelRow stats the configured model path. Empty path is a
// soft error (the operator should set it) but still renders so the
// reason is visible.
func probeModelRow(label, configured string) ui.VoiceProbeStatus {
	row := ui.VoiceProbeStatus{Label: label, Configured: configured != ""}
	configured = strings.TrimSpace(configured)
	row.Path = configured
	if configured == "" {
		row.Error = "model path is empty in config"
		return row
	}
	info, err := os.Stat(configured)
	switch {
	case err == nil && info.IsDir():
		row.Error = "configured path is a directory, not a file"
	case err == nil:
		row.OK = true
	case os.IsNotExist(err):
		row.Error = "model file not found"
	default:
		row.Error = err.Error()
	}
	return row
}

// probeFilesystemWritable does a small touch+remove in the
// artifacts directory. Skipped if the directory itself is missing —
// that case yields a Writable=false + an explanatory error rather
// than a misleading "writable" verdict.
func probeFilesystemWritable(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, fmt.Errorf("artifacts_path is not configured")
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s exists but is not a directory", path)
	}
	probe := filepath.Join(path, fmt.Sprintf(".vornik-writable-probe-%d", time.Now().UnixNano()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true, nil
}
