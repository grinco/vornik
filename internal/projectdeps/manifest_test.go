package projectdeps

import (
	"errors"
	"strings"
	"testing"
)

func TestManifestValidate(t *testing.T) {
	tests := []struct {
		name    string
		m       Manifest
		wantErr string
	}{
		{
			name: "pip with a lockfile is accepted",
			m:    Manifest{Entries: []Entry{{Ecosystem: EcosystemPip, Lockfile: "requirements.lock"}}},
		},
		{
			name: "nested lockfile path is accepted",
			m:    Manifest{Entries: []Entry{{Ecosystem: EcosystemPip, Lockfile: "deploy/requirements.lock"}}},
		},
		{
			name:    "empty manifest is accepted",
			m:       Manifest{},
			wantErr: "",
		},
		{
			// design §5.1: an install: field is a remote-triggered exec
			// by another name (the 2026-08-03 ruling).
			name:    "install command is refused",
			m:       Manifest{Entries: []Entry{{Ecosystem: EcosystemPip, Lockfile: "r.lock", Install: "pip install -r r.txt"}}},
			wantErr: "not a supported field",
		},
		{
			name:    "missing ecosystem",
			m:       Manifest{Entries: []Entry{{Lockfile: "r.lock"}}},
			wantErr: "ecosystem: is required",
		},
		{
			name:    "unknown ecosystem names the known set",
			m:       Manifest{Entries: []Entry{{Ecosystem: "cargo", Lockfile: "Cargo.lock"}}},
			wantErr: `unknown ecosystem "cargo"`,
		},
		{
			// A gap and a typo must not read the same.
			name:    "recognised but unimplemented ecosystem says so",
			m:       Manifest{Entries: []Entry{{Ecosystem: EcosystemNPM, Lockfile: "package-lock.json"}}},
			wantErr: "not yet materialised",
		},
		{
			name:    "duplicate ecosystem",
			m:       Manifest{Entries: []Entry{{Ecosystem: EcosystemPip, Lockfile: "a.lock"}, {Ecosystem: EcosystemPip, Lockfile: "b.lock"}}},
			wantErr: "already declared at dependencies[0]",
		},
		{
			name:    "missing lockfile",
			m:       Manifest{Entries: []Entry{{Ecosystem: EcosystemPip}}},
			wantErr: "lockfile: is required",
		},
		{
			name:    "absolute lockfile path",
			m:       Manifest{Entries: []Entry{{Ecosystem: EcosystemPip, Lockfile: "/etc/requirements.lock"}}},
			wantErr: "must be relative to the project root",
		},
		{
			name:    "lockfile escaping the project tree",
			m:       Manifest{Entries: []Entry{{Ecosystem: EcosystemPip, Lockfile: "../../other/requirements.lock"}}},
			wantErr: "must stay within the project tree",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
			var ve ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate() returned %T, want ValidationError so callers can report the index", err)
			}
		})
	}
}

func TestValidationErrorMessage(t *testing.T) {
	if got := (ValidationError{Index: 2, Field: "lockfile", Message: "boom"}).Error(); got != "dependencies[2].lockfile: boom" {
		t.Fatalf("Error() = %q", got)
	}
	if got := (ValidationError{Index: 1, Message: "boom"}).Error(); got != "dependencies[1]: boom" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestCacheKeyIdentifiesContentAndPlatform(t *testing.T) {
	a := CacheKey(EcosystemPip, []byte("numpy==1.0\n"), "linux-amd64")
	same := CacheKey(EcosystemPip, []byte("numpy==1.0\n"), "linux-amd64")
	if a != same {
		t.Fatalf("the same lockfile on the same platform must produce one key: %q vs %q", a, same)
	}

	// design §3 property 2: a manifest change is a NEW key rather than a
	// mutation of a live one, so a task that started against the old key
	// finishes against it.
	changed := CacheKey(EcosystemPip, []byte("numpy==2.0\n"), "linux-amd64")
	if changed == a {
		t.Fatal("a changed lockfile must produce a different key")
	}

	// package-lock.json resolves platform-specific optional deps, so an
	// x64 materialisation and an arm64 one are different byte sets under
	// the same lockfile. The platform is in the key for every ecosystem.
	otherArch := CacheKey(EcosystemPip, []byte("numpy==1.0\n"), "linux-arm64")
	if otherArch == a {
		t.Fatal("a different platform must produce a different key")
	}

	if otherEco := CacheKey(EcosystemNPM, []byte("numpy==1.0\n"), "linux-amd64"); otherEco == a {
		t.Fatal("a different ecosystem must produce a different key")
	}
}

func TestCacheKeyIsOnePathSegment(t *testing.T) {
	key := CacheKey(EcosystemPip, []byte("x"), "linux/amd64 ../escape")
	if strings.ContainsAny(key, `/\`) {
		t.Fatalf("key %q must stay a single path segment: it is used as a directory name under the deps root", key)
	}
	if strings.Contains(key, "..") {
		t.Fatalf("key %q must not carry a traversal sequence", key)
	}
	if empty := CacheKey(EcosystemPip, []byte("x"), ""); !strings.Contains(empty, "unknown") {
		t.Fatalf("an empty platform should be named, got %q", empty)
	}
}

func TestPlatformIsPopulated(t *testing.T) {
	if p := Platform(); !strings.Contains(p, "-") {
		t.Fatalf("Platform() = %q, want <os>-<arch>", p)
	}
}
