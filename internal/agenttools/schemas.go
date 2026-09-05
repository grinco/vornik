package agenttools

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

// schemaFS holds one OpenAI function definition per declared tool,
// schemas/<name>.json. The test asserts the file set equals the declared name
// set and that each file's function.name is its filename, so a schema without
// a declaration or a declaration without a schema fails `go test`.
//
//go:embed schemas/*.json
var schemaFS embed.FS

func init() {
	for i := range Tools {
		b, err := fs.ReadFile(schemaFS, "schemas/"+Tools[i].Name+".json")
		if err != nil {
			panic(fmt.Sprintf("agenttools: %s is declared but schemas/%s.json is missing: %v", Tools[i].Name, Tools[i].Name, err))
		}
		Tools[i].Schema = b
	}
}

// schemaFiles returns the embedded schema file names without the directory
// and extension, for the declaration test.
func schemaFiles() ([]string, error) {
	entries, err := fs.ReadDir(schemaFS, "schemas")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	return out, nil
}
