// Command docs-gen generates the mechanically-derivable customer-docs pages
// (the config + CLI reference) from their source of truth, and stamps the
// provenance hashes on narrative pages. It is the anti-drift half of the docs
// pipeline: the reference pages are generator OUTPUT, never hand-edited, and
// CI fails if the committed page differs from a fresh generation.
//
// Usage:
//
//	docs-gen cli      # regenerate docs/public/reference/vornikctl.md
//	docs-gen config   # regenerate docs/public/reference/configuration.md
//	docs-gen all      # both reference pages
//	docs-gen stamp    # re-anchor sources: hashes on all docs/public pages
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"vornik.io/vornik/internal/cli"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/docsmeta"
)

const genHeader = "<!-- Generated from source — do not edit by hand. -->\n\n"

const (
	cliPage    = "docs/public/reference/vornikctl.md"
	configPage = "docs/public/reference/configuration.md"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: docs-gen <cli|config|editions|all|stamp>")
		os.Exit(2)
	}
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	deny, err := docsmeta.LoadDenylist(filepath.Join(root, "scripts", "docs-ip-denylist.txt"))
	if err != nil {
		fatal(err)
	}
	switch os.Args[1] {
	case "cli":
		writePage(filepath.Join(root, cliPage), genHeader+renderCLI(cli.RootCmd(), loadCLIAllow(root)), deny)
	case "config":
		writePage(filepath.Join(root, configPage), genHeader+renderConfig(reflect.TypeOf(config.Config{})), deny)
	case "editions":
		writeEditions(root, deny)
	case "all":
		writePage(filepath.Join(root, cliPage), genHeader+renderCLI(cli.RootCmd(), loadCLIAllow(root)), deny)
		writePage(filepath.Join(root, configPage), genHeader+renderConfig(reflect.TypeOf(config.Config{})), deny)
		writeEditions(root, deny)
	case "stamp":
		stamp(root)
	default:
		fmt.Fprintf(os.Stderr, "docs-gen: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "docs-gen: %v\n", err)
	os.Exit(1)
}

// writePage refuses to emit a page that trips the IP guard — a generated page
// is only as safe as its source, and cobra help / config comments can cite
// internal detail. A leak fails the build instead of shipping.
func writePage(path, content string, deny []string) {
	if hits := docsmeta.ForbiddenHits(content, deny); len(hits) > 0 {
		fmt.Fprintf(os.Stderr, "docs-gen: refusing to write %s — IP markers in generated output:\n", path)
		for _, h := range hits {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
		fmt.Fprintln(os.Stderr, "fix the source (help text / config comment) or adjust the allowlist.")
		os.Exit(1)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s\n", path)
}

// loadCLIAllow reads the customer-facing top-level command allowlist. Each
// non-empty, non-comment line is one top-level vornikctl command name. Missing
// file => empty allowlist => nothing emitted (deny-by-default).
func loadCLIAllow(root string) map[string]bool {
	allow := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(root, "scripts", "docs-cli-allowlist.txt"))
	if err != nil {
		return allow
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allow[line] = true
	}
	return allow
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

// stamp re-anchors provenance hashes on every docs/public page that declares a
// sources: block. Run after reviewing a page whose source drifted.
func stamp(root string) {
	pub := filepath.Join(root, "docs", "public")
	_ = filepath.WalkDir(pub, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		changed, rerr := docsmeta.Restamp(root, path)
		if rerr != nil {
			fatal(rerr)
		}
		if changed {
			rel, _ := filepath.Rel(root, path)
			fmt.Printf("stamped %s\n", rel)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// CLI reference generation
// ---------------------------------------------------------------------------

// renderCLI walks a cobra command tree and emits a single Markdown reference.
// Deny-by-default: only top-level command groups named in allow are emitted
// (with all their descendants). This keeps internal/admin commands out of the
// customer docs even though they remain in `vornikctl --help` for operators.
func renderCLI(root *cobra.Command, allow map[string]bool) string {
	var b strings.Builder
	b.WriteString("# vornikctl CLI reference\n\n")
	if root.Long != "" {
		b.WriteString(root.Long + "\n\n")
	} else if root.Short != "" {
		b.WriteString(root.Short + "\n\n")
	}
	var emit func(c *cobra.Command)
	emit = func(c *cobra.Command) {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			return
		}
		b.WriteString("## " + c.CommandPath() + "\n\n")
		if c.Short != "" {
			b.WriteString(c.Short + "\n\n")
		}
		if c.Long != "" && c.Long != c.Short {
			b.WriteString(c.Long + "\n\n")
		}
		if c.Runnable() {
			b.WriteString("```\n" + strings.TrimSpace(c.UseLine()) + "\n```\n\n")
		}
		b.WriteString(renderFlags(c.LocalFlags()))
		if c.Example != "" {
			b.WriteString("Example:\n\n```\n" + strings.TrimSpace(c.Example) + "\n```\n\n")
		}
		children := append([]*cobra.Command(nil), c.Commands()...)
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			emit(child)
		}
	}
	top := append([]*cobra.Command(nil), root.Commands()...)
	sort.Slice(top, func(i, j int) bool { return top[i].Name() < top[j].Name() })
	for _, c := range top {
		if allow[c.Name()] {
			emit(c)
		}
	}
	return b.String()
}

func renderFlags(fs *pflag.FlagSet) string {
	type row struct{ name, def, usage string }
	var rows []row
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		rows = append(rows, row{name, f.DefValue, f.Usage})
	})
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var b strings.Builder
	b.WriteString("| Flag | Default | Description |\n|---|---|---|\n")
	for _, r := range rows {
		def := r.def
		if def != "" {
			def = "`" + def + "`"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.name, def, escapePipes(r.usage))
	}
	b.WriteString("\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// config reference generation
// ---------------------------------------------------------------------------

type docRow struct{ key, typ, doc string }

// renderConfig reflects over the config struct type and emits a Markdown table
// of every field carrying a `doc:"..."` tag, grouped by top-level section.
// Deny-by-default: an untagged field is never published.
func renderConfig(t reflect.Type) string {
	rows := collectDocRows(t, "")
	var b strings.Builder
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("vornik reads its configuration from `config.yaml`. The keys below are the customer-facing settings, by dotted YAML path.\n\n")

	// Group by first path segment, preserving first-seen section order.
	var order []string
	groups := map[string][]docRow{}
	for _, r := range rows {
		sec := r.key
		if i := strings.IndexByte(r.key, '.'); i >= 0 {
			sec = r.key[:i]
		}
		if _, ok := groups[sec]; !ok {
			order = append(order, sec)
		}
		groups[sec] = append(groups[sec], r)
	}
	for _, sec := range order {
		b.WriteString("## " + sec + "\n\n")
		b.WriteString("| Key | Type | Description |\n|---|---|---|\n")
		for _, r := range groups[sec] {
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.key, r.typ, escapePipes(r.doc))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func collectDocRows(t reflect.Type, prefix string) []docRow {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var rows []docRow
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		// Skip top-level sections deliberately hidden from the PUBLIC config
		// reference. The trading feature is withheld from the published site
		// (mkdocs exclude_docs drops its feature/guide pages); the generated
		// config reference must not re-expose it via its keys. The keys still
		// work — they're just not advertised on docs.vornik.io.
		if publicDocExcludedSections[strings.SplitN(key, ".", 2)[0]] {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		// A doc tag makes the field a documented leaf, even if it is a struct.
		if doc := f.Tag.Get("doc"); doc != "" {
			rows = append(rows, docRow{key: key, typ: friendlyType(ft), doc: doc})
			continue
		}
		// No doc tag: recurse into nested config sections; skip plain leaves
		// (deny-by-default — an untagged scalar is internal).
		if ft.Kind() == reflect.Struct && !isLeafStruct(ft) {
			rows = append(rows, collectDocRows(ft, key)...)
		}
	}
	return rows
}

// publicDocExcludedSections are top-level config sections kept OUT of the
// generated PUBLIC config reference. The trading feature is hidden from the
// published docs (see mkdocs.yml exclude_docs); its config keys would
// otherwise leak the feature's existence onto docs.vornik.io.
var publicDocExcludedSections = map[string]bool{
	"trading": true,
}

// isLeafStruct treats stdlib value-structs (time.Time, etc.) as leaves so the
// walker doesn't descend into their internals.
func isLeafStruct(t reflect.Type) bool {
	return t.PkgPath() == "time"
}

func friendlyType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Slice, reflect.Array:
		return "list"
	case reflect.Map:
		return "map"
	default:
		return t.Kind().String()
	}
}

func escapePipes(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
}
