// skills-site builds the static site that backs the vornik
// skill registry — by default skills.vornik.io.
//
// Input: a curated skills.yaml file (path passed via -in flag,
// default seed/skills.yaml relative to the cwd).
// Output, written under the directory passed via -out
// (default site/):
//
//   - index.json — what `vornikctl skill install <h>/<s>` /
//                  `vornikctl skill search` fetches.
//   - index.html — the discovery landing page.
//   - <handle>/<skill>.html — per-skill landing page.
//
// Deployment: copy the output dir into a GitHub Pages repo
// (or any static host). The CLI's contract is the JSON shape;
// the HTML is purely operator-facing.
//
// The generator is dependency-free at runtime — it ships in the
// vornik repo so anyone can self-host their own registry by
// running it against their own skills.yaml.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/cli"
)

func main() {
	in := flag.String("in", "seed/skills.yaml", "input skills.yaml")
	out := flag.String("out", "site", "output directory")
	flag.Parse()

	data, err := os.ReadFile(*in)
	if err != nil {
		fatal("read %s: %v", *in, err)
	}
	var idx cli.SkillIndex
	if err := yaml.Unmarshal(data, &idx); err != nil {
		fatal("parse %s: %v", *in, err)
	}
	if idx.Version == 0 {
		idx.Version = 1
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal("mkdir %s: %v", *out, err)
	}

	if err := writeIndexJSON(*out, &idx); err != nil {
		fatal("write index.json: %v", err)
	}
	if err := writeIndexHTML(*out, &idx); err != nil {
		fatal("write index.html: %v", err)
	}
	if err := writeSkillPages(*out, &idx); err != nil {
		fatal("write skill pages: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote %d skill(s) to %s\n", len(idx.Skills), *out)
}

func writeIndexJSON(dir string, idx *cli.SkillIndex) error {
	f, err := os.Create(filepath.Join(dir, "index.json"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(idx)
}

const indexHTMLTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>vornik skill registry</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body { font-family: -apple-system, system-ui, sans-serif; margin: 2rem auto; max-width: 64rem; padding: 0 1rem; color: #111; }
  h1 { font-size: 1.6rem; margin: 0 0 0.4rem 0; }
  .sub { color: #555; margin-bottom: 1.5rem; }
  .filter { width: 100%; padding: 0.5rem 0.6rem; font-size: 1rem; box-sizing: border-box; border: 1px solid #ccc; border-radius: 4px; }
  table { width: 100%; border-collapse: collapse; margin-top: 1rem; }
  th, td { text-align: left; padding: 0.5rem 0.6rem; border-bottom: 1px solid #eee; font-size: 0.95rem; vertical-align: top; }
  th { background: #fafafa; color: #555; font-weight: 600; }
  .handle { font-family: ui-monospace, "SF Mono", monospace; }
  .tags { color: #2a6; font-size: 0.85rem; }
  code { background: #f4f4f4; padding: 0.05rem 0.3rem; border-radius: 3px; font-family: ui-monospace, "SF Mono", monospace; }
  footer { color: #888; font-size: 0.85rem; margin-top: 3rem; padding-top: 1rem; border-top: 1px solid #eee; }
</style>
</head>
<body>
<h1>vornik skill registry</h1>
<p class="sub">Browse, install, and share portable SWARM-SKILL.md bundles. Install with <code>vornikctl skill install &lt;handle&gt;/&lt;skill&gt;</code>.</p>
<input id="filter" class="filter" type="search" placeholder="filter by handle, name, description, or tag…" oninput="filterRows()">
<table>
<thead><tr><th>Skill</th><th>Description</th><th>Tags</th><th>Installs</th><th>Rating</th></tr></thead>
<tbody id="rows">
{{range .Skills}}
<tr data-search="{{.Handle}} {{.Skill}} {{.Description}} {{range .Tags}}{{.}} {{end}}">
  <td><a href="{{.Handle}}/{{.Skill}}.html" class="handle">{{.Handle}}/{{.Skill}}</a></td>
  <td>{{.Description}}</td>
  <td class="tags">{{join .Tags ", "}}</td>
  <td>{{.InstallCount}}</td>
  <td>{{rating .RatingAvg .RatingN}}</td>
</tr>
{{end}}
</tbody>
</table>
<footer>
  <p>Want to publish your own? See <a href="https://docs.vornik.io/skills">docs.vornik.io/skills</a> or run <code>vornikctl skill register &lt;handle&gt;/&lt;skill&gt; --git-url ...</code>.</p>
</footer>
<script>
  function filterRows() {
    var q = document.getElementById('filter').value.toLowerCase();
    var rows = document.querySelectorAll('#rows tr');
    rows.forEach(function(r) {
      var hay = (r.getAttribute('data-search') || '').toLowerCase();
      r.style.display = hay.indexOf(q) >= 0 ? '' : 'none';
    });
  }
</script>
</body>
</html>
`

const skillHTMLTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Handle}}/{{.Skill}} — vornik skill registry</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body { font-family: -apple-system, system-ui, sans-serif; margin: 2rem auto; max-width: 48rem; padding: 0 1rem; color: #111; }
  h1 { font-size: 1.5rem; margin: 0 0 0.4rem 0; font-family: ui-monospace, monospace; }
  .desc { font-size: 1.05rem; margin: 0.5rem 0 1rem 0; }
  .meta { color: #555; font-size: 0.9rem; }
  .tags { color: #2a6; }
  code { background: #f4f4f4; padding: 0.1rem 0.4rem; border-radius: 3px; font-family: ui-monospace, monospace; }
  pre { background: #f4f4f4; padding: 0.8rem 1rem; border-radius: 4px; overflow-x: auto; }
  a { color: #06c; }
</style>
</head>
<body>
<p><a href="../index.html">← back to registry</a></p>
<h1>{{.Handle}}/{{.Skill}}</h1>
<p class="desc">{{.Description}}</p>
<p class="meta">
  {{if .Tags}}<span class="tags">tags: {{join .Tags ", "}}</span><br>{{end}}
  source: <a href="{{.GitURL}}">{{.GitURL}}</a><br>
  {{if .Homepage}}homepage: <a href="{{.Homepage}}">{{.Homepage}}</a><br>{{end}}
  installs: {{.InstallCount}} · rating: {{rating .RatingAvg .RatingN}}
</p>
<h2>Install</h2>
<pre>vornikctl skill install {{.Handle}}/{{.Skill}}</pre>
<h2>Or via git URL directly</h2>
<pre>vornikctl skill install {{.GitURL}}</pre>
</body>
</html>
`

func writeIndexHTML(dir string, idx *cli.SkillIndex) error {
	tmpl, err := template.New("index").Funcs(indexFuncs()).Parse(indexHTMLTemplate)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "index.html"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return tmpl.Execute(f, idx)
}

func writeSkillPages(dir string, idx *cli.SkillIndex) error {
	tmpl, err := template.New("skill").Funcs(indexFuncs()).Parse(skillHTMLTemplate)
	if err != nil {
		return err
	}
	for _, s := range idx.Skills {
		page := filepath.Join(dir, s.Handle, s.Skill+".html")
		if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
			return err
		}
		f, err := os.Create(page)
		if err != nil {
			return err
		}
		if err := tmpl.Execute(f, s); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func indexFuncs() template.FuncMap {
	return template.FuncMap{
		"join": strings.Join,
		"rating": func(avg float64, n int) string {
			if n == 0 {
				return "—"
			}
			return fmt.Sprintf("%.1f (%d)", avg, n)
		},
		"safe": func(s string) template.HTML { return template.HTML(html.EscapeString(s)) },
	}
}

func fatal(format string, args ...any) {
	_, _ = io.WriteString(os.Stderr, "skills-site: "+fmt.Sprintf(format, args...)+"\n")
	os.Exit(1)
}
