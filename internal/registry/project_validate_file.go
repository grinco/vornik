package registry

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ValidateProjectFile reads the YAML at path into a Project and runs
// Project.Validate against it — the project-scope analogue of
// config.ValidateFile (internal/config/loader.go), which parses a
// config.yaml into config.Config and validates THAT schema.
//
// Guided Integrations Hub task 5.2b needs this because
// featuredoctor.FileConfigWriter.Validate() always calls config.ValidateFile
// regardless of which file it's pointed at — correct for daemon-scope saves
// (config.yaml) but wrong for a project-scope save (projects/<id>.yaml):
// re-parsing a project file as a daemon config.Config either produces
// spurious validation errors (a project file has no "server"/"database"
// block) or, worse, silently "validates" a structurally-broken project file
// because config.Config's required fields happen to be satisfied by an
// empty document with defaults. ProjectConfigWriter (internal/integrations)
// uses this function for its Validate() instead.
//
// Deliberately independent of LoadProjects: LoadProjects walks a whole
// projects/ directory, attaches PROJECT.md brief companions, and skips
// (rather than errors on) a YAML-syntax-broken file so one bad project
// doesn't take down daemon boot — none of that applies to validating one
// candidate file mid-save, where a syntax or schema error MUST fail loudly
// so the caller rolls back.
func ValidateProjectFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var project Project
	if err := yaml.Unmarshal(data, &project); err != nil {
		return fmt.Errorf("%s: parse: %w", path, err)
	}
	return project.Validate(path)
}
