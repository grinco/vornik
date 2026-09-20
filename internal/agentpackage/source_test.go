package agentpackage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePackageDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const goodManifest = `package: acme-incident-response
version: 1.2.0
contributes:
  workflows:
    - incident-triage.md
  roles:
    - incident-lead.md
`

func TestOpenReadsADirectoryPackage(t *testing.T) {
	// A directory is what an author has while building a package;
	// refusing it would make every iteration a repack.
	dir := writePackageDir(t, map[string]string{
		ManifestFile:         goodManifest,
		"incident-triage.md": "# triage\n",
		"incident-lead.md":   "# lead\n",
	})

	src, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer src.Close()

	if src.Manifest.Package != "acme-incident-response" {
		t.Fatalf("manifest = %+v", src.Manifest)
	}
	body, err := src.ReadPayload("incident-triage.md")
	if err != nil || string(body) != "# triage\n" {
		t.Fatalf("ReadPayload() = %q, %v", body, err)
	}
}

func TestOpenRefusesAManifestKeyThisBinaryDoesNotModel(t *testing.T) {
	// A key this schema does not model means a package built for a newer
	// Vornik. Installing the half this binary understands is the
	// upgrade-safety failure the project loader already learned to refuse.
	dir := writePackageDir(t, map[string]string{
		ManifestFile: goodManifest + "requires_vornik: 2027.1.0\n",
	})
	_, err := Open(dir)
	if err == nil {
		t.Fatal("Open() accepted an unknown manifest key")
	}
	if !strings.Contains(err.Error(), "requires_vornik") {
		t.Fatalf("err = %q, want it to name the key", err)
	}
}

func TestReadPayloadRefusesAnEscapingPath(t *testing.T) {
	dir := writePackageDir(t, map[string]string{
		ManifestFile:         goodManifest,
		"incident-triage.md": "x",
		"incident-lead.md":   "y",
	})
	src, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	for _, bad := range []string{"../../../etc/passwd", "/etc/passwd"} {
		if _, err := src.ReadPayload(bad); err == nil {
			t.Fatalf("ReadPayload(%q) = nil error, want a refusal", bad)
		}
	}
}

func tarGz(t *testing.T, members []tarMember) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		hdr := &tar.Header{Name: m.name, Mode: 0o600, Size: int64(len(m.body)), Typeflag: m.typeflag}
		if m.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if m.linkname != "" {
			hdr.Linkname = m.linkname
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type tarMember struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func TestOpenReadsATarballPackage(t *testing.T) {
	path := tarGz(t, []tarMember{
		{name: ManifestFile, body: goodManifest},
		{name: "incident-triage.md", body: "# triage\n"},
		{name: "incident-lead.md", body: "# lead\n"},
	})

	src, err := Open(path)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer src.Close()

	body, err := src.ReadPayload("incident-lead.md")
	if err != nil || string(body) != "# lead\n" {
		t.Fatalf("ReadPayload() = %q, %v", body, err)
	}
}

func TestOpenRefusesAnArchiveMemberThatEscapesTheTree(t *testing.T) {
	// A package is an artifact from OUTSIDE this deployment, and a tar
	// file is free to carry a member named ../../../configs/workflows/x.md.
	path := tarGz(t, []tarMember{
		{name: ManifestFile, body: goodManifest},
		{name: "../../../etc/cron.d/pwn", body: "* * * * * root sh\n"},
	})

	if _, err := Open(path); err == nil {
		t.Fatal("Open() accepted a traversing archive member")
	} else if !strings.Contains(err.Error(), "escapes the package tree") {
		t.Fatalf("err = %q, want the zip-slip refusal", err)
	}
}

func TestOpenRefusesASymlinkMember(t *testing.T) {
	// A skipped symlink is a package that installs differently than it
	// reads; a followed one undoes the containment check via a file that
	// already passed it.
	path := tarGz(t, []tarMember{
		{name: ManifestFile, body: goodManifest},
		{name: "incident-triage.md", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})

	if _, err := Open(path); err == nil {
		t.Fatal("Open() accepted a symlink member")
	} else if !strings.Contains(err.Error(), "neither symlinks nor devices") {
		t.Fatalf("err = %q", err)
	}
}

func TestOpenRefusesAnOversizedMember(t *testing.T) {
	path := tarGz(t, []tarMember{
		{name: ManifestFile, body: goodManifest},
		{name: "incident-triage.md", body: strings.Repeat("x", maxPayloadBytes+1)},
	})
	if _, err := Open(path); err == nil {
		t.Fatal("Open() accepted a member past the per-file ceiling")
	}
}

func TestOpenRejectsANonArchiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-package")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "not a gzip archive") {
		t.Fatalf("Open() = %v, want a clear not-an-archive message", err)
	}
}

func TestOpenReportsAMissingManifest(t *testing.T) {
	dir := writePackageDir(t, map[string]string{"incident-triage.md": "x"})
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), ManifestFile) {
		t.Fatalf("Open() = %v, want the missing manifest named", err)
	}
}

func TestOpenRefusesTooManyMembers(t *testing.T) {
	// A tarball with a million empty files would exhaust inodes before
	// anything validated it.
	members := []tarMember{{name: ManifestFile, body: goodManifest}}
	for i := 0; i < maxPayloadFiles+1; i++ {
		members = append(members, tarMember{name: filepath.Join("payload", "f"+strings.Repeat("0", 3)+itoa(i)+".md"), body: "x"})
	}
	if _, err := Open(tarGz(t, members)); err == nil {
		t.Fatal("Open() accepted an archive past the member ceiling")
	} else if !strings.Contains(err.Error(), "members") {
		t.Fatalf("err = %q, want the member-count refusal", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestOpenUnpacksDirectoryMembers(t *testing.T) {
	path := tarGz(t, []tarMember{
		{name: "nested/", typeflag: tar.TypeDir},
		{name: ManifestFile, body: "package: acme\nversion: 1.0.0\ncontributes:\n  workflows: [nested/triage.md]\n"},
		{name: "nested/triage.md", body: "# triage\n"},
	})
	src, err := Open(path)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer src.Close()

	body, err := src.ReadPayload("nested/triage.md")
	if err != nil || string(body) != "# triage\n" {
		t.Fatalf("ReadPayload() = %q, %v", body, err)
	}
}

func TestReadPayloadRefusesAnOversizedFileInADirectoryPackage(t *testing.T) {
	// The tarball path checks the header AND the bytes; a directory
	// package has no header, so the stat is the only ceiling there is.
	dir := writePackageDir(t, map[string]string{
		ManifestFile:         goodManifest,
		"incident-triage.md": strings.Repeat("x", maxPayloadBytes+1),
		"incident-lead.md":   "y",
	})
	src, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if _, err := src.ReadPayload("incident-triage.md"); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("ReadPayload() = %v, want the per-file ceiling enforced", err)
	}
}

func TestReadPayloadReportsAMissingFile(t *testing.T) {
	dir := writePackageDir(t, map[string]string{
		ManifestFile:         goodManifest,
		"incident-triage.md": "x",
		"incident-lead.md":   "y",
	})
	src, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if _, err := src.ReadPayload("not-in-the-package.md"); err == nil {
		t.Fatal("ReadPayload() = nil error for a file the package does not carry")
	}
}

func TestOpenReportsAMissingPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "no-such-package")); err == nil {
		t.Fatal("Open() = nil error for a path that does not exist")
	}
}

func TestOpenRefusesAnInvalidManifest(t *testing.T) {
	dir := writePackageDir(t, map[string]string{
		ManifestFile: "package: ACME\nversion: 1.0.0\ncontributes:\n  workflows: [w.md]\n",
	})
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "kebab-case") {
		t.Fatalf("Open() = %v, want Validate's refusal", err)
	}
}
