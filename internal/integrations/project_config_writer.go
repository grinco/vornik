package integrations

import (
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/registry"
)

// ProjectConfigWriter is the project-scope analogue of
// featuredoctor.FileConfigWriter (task 5.2b, 5.2 review finding 3).
//
// FileConfigWriter.Validate() unconditionally calls config.ValidateFile,
// which parses the file at its Path as a daemon config.Config and runs
// THAT schema's validation — correct for config.yaml (daemon scope), wrong
// for projects/<id>.yaml (project scope): a project file has no
// "server"/"database" block a daemon config requires, and conversely a
// structurally-broken project file (e.g. a github_app block missing its
// required webhook_secret_env) would not be caught by the daemon schema at
// all, since config.Config doesn't know that field exists.
//
// ProjectConfigWriter embeds the real FileConfigWriter for Read/Write/
// Backup/Restore — identical atomic temp+rename write, identical
// Backup/Restore rollback (the design's §5.4/§9 write-model decision: ONE
// write model for both scopes, no dual path) — and overrides only
// Validate() to run the file through the project schema instead
// (registry.ValidateProjectFile, which parses into registry.Project and
// calls Project.Validate).
type ProjectConfigWriter struct {
	featuredoctor.FileConfigWriter
}

// Validate re-reads the file at w.Path and runs it through the project
// registry's parse+validate pipeline, mirroring FileConfigWriter.Validate's
// "no global state, callable immediately after Write" contract.
func (w *ProjectConfigWriter) Validate() error {
	return registry.ValidateProjectFile(w.Path)
}
