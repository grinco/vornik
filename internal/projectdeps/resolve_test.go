package projectdeps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectWithLock(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "requirements.lock"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPlanReportsAnUnmaterialisedKeyAsPendingNotBroken(t *testing.T) {
	root := projectWithLock(t, goodLock)
	r := NewResolver(NewStore(t.TempDir(), PostureConnected), "linux-amd64")

	plans := r.Plan(root, []Entry{{Ecosystem: EcosystemPip, Lockfile: "requirements.lock"}})
	if len(plans) != 1 {
		t.Fatalf("Plan() returned %d plans, want 1", len(plans))
	}
	p := plans[0]
	if p.Problem != nil {
		// "The fetch has not happened yet" is not a defect, and a doctor
		// that reported it as one would cry wolf on every fresh install.
		t.Fatalf("Problem = %v, want nil for a merely-unfetched key", p.Problem)
	}
	if p.Materialised {
		t.Fatal("Materialised = true for a key that was never fetched")
	}
	if p.Key == "" {
		t.Fatal("Key is empty; the plan cannot name what the operator must import")
	}
	if !strings.HasSuffix(p.LockfilePath, "requirements.lock") {
		t.Fatalf("LockfilePath = %q", p.LockfilePath)
	}
}

func TestPlanReportsAMaterialisedKey(t *testing.T) {
	root := projectWithLock(t, goodLock)
	store := NewStore(t.TempDir(), PostureConnected)
	r := NewResolver(store, "linux-amd64")

	key := CacheKey(EcosystemPip, []byte(goodLock), "linux-amd64")
	writeTree(t, store.Path(key), map[string]string{CompletionMarker: "key: " + key + "\n"})

	plans := r.Plan(root, []Entry{{Ecosystem: EcosystemPip, Lockfile: "requirements.lock"}})
	if !plans[0].Materialised || plans[0].Problem != nil {
		t.Fatalf("plan = %+v, want materialised with no problem", plans[0])
	}
	if got := plans[0].Mount(store).HostPath; got != store.Path(key) {
		t.Fatalf("Mount().HostPath = %q, want %q", got, store.Path(key))
	}
}

func TestPlanReportsADefectiveManifestAsAProblem(t *testing.T) {
	store := NewStore(t.TempDir(), PostureConnected)
	r := NewResolver(store, "linux-amd64")

	t.Run("lockfile the project does not have", func(t *testing.T) {
		// Must read as a config error, not as an empty dependency set.
		plans := r.Plan(t.TempDir(), []Entry{{Ecosystem: EcosystemPip, Lockfile: "requirements.lock"}})
		if plans[0].Problem == nil {
			t.Fatal("Problem = nil for a missing lockfile")
		}
		if !strings.Contains(plans[0].Problem.Error(), "read lockfile") {
			t.Fatalf("Problem = %v, want a read failure", plans[0].Problem)
		}
	})

	t.Run("lockfile that cannot be keyed", func(t *testing.T) {
		root := projectWithLock(t, "numpy==1.26.4\n")
		plans := r.Plan(root, []Entry{{Ecosystem: EcosystemPip, Lockfile: "requirements.lock"}})
		var le LockfileError
		if !errors.As(plans[0].Problem, &le) {
			t.Fatalf("Problem = %v, want a LockfileError", plans[0].Problem)
		}
		if plans[0].Key != "" {
			t.Fatalf("Key = %q, want empty: an un-keyable lockfile has no key", plans[0].Key)
		}
	})
}

func TestPlanDoesNotEscapeTheProjectRoot(t *testing.T) {
	// Validate() refuses a traversing lockfile at registry load, but Plan
	// must not depend on having been called only on validated input.
	store := NewStore(t.TempDir(), PostureConnected)
	r := NewResolver(store, "linux-amd64")
	root := t.TempDir()

	plans := r.Plan(root, []Entry{{Ecosystem: EcosystemPip, Lockfile: "../../../etc/passwd"}})
	if plans[0].Problem == nil {
		t.Fatalf("Problem = nil; Plan resolved %q, which is outside %q", plans[0].LockfilePath, root)
	}
	if !strings.Contains(plans[0].Problem.Error(), "escapes the project tree") {
		t.Fatalf("Problem = %v, want an escape refusal", plans[0].Problem)
	}
	if plans[0].LockfilePath != "" {
		t.Fatalf("LockfilePath = %q, want empty for a refused path", plans[0].LockfilePath)
	}

	// An absolute path is refused for the same reason, and Validate's
	// refusal is not the only one standing between a config and a read.
	abs := r.Plan(root, []Entry{{Ecosystem: EcosystemPip, Lockfile: "/etc/passwd"}})
	if abs[0].Problem == nil || !strings.Contains(abs[0].Problem.Error(), "must be relative") {
		t.Fatalf("Problem = %v, want an absolute-path refusal", abs[0].Problem)
	}
	if empty := r.Plan(root, []Entry{{Ecosystem: EcosystemPip}}); empty[0].Problem == nil {
		t.Fatal("an empty lockfile path must be refused")
	}
}

func TestEnsureRefusesTheWholeSetOnOneProblem(t *testing.T) {
	// A partial mount hands the agent an environment that imports some of
	// what the project declared, and the missing half surfaces as an
	// ordinary ImportError that reads like the code's fault.
	store := NewStore(t.TempDir(), PostureConnected)
	r := NewResolver(store, "linux-amd64")

	mounts, err := r.Ensure(context.Background(), []Plan{
		{Entry: Entry{Ecosystem: EcosystemPip}, Key: "k1", Materialised: true},
		{Entry: Entry{Ecosystem: EcosystemPip}, Problem: errors.New("lockfile is not hash-pinned")},
	})
	if err == nil {
		t.Fatal("Ensure() = nil error, want the problem surfaced")
	}
	if mounts != nil {
		t.Fatalf("Ensure() = %v mounts, want none", mounts)
	}
}

func TestEnsureWithNoPlansInjectsNothing(t *testing.T) {
	r := NewResolver(NewStore(t.TempDir(), PostureConnected), "linux-amd64")
	mounts, err := r.Ensure(context.Background(), nil)
	if err != nil {
		t.Fatalf("Ensure(nil) = %v", err)
	}
	if mounts != nil {
		t.Fatalf("Ensure(nil) = %v, want nil so no empty PYTHONPATH can be injected", mounts)
	}
}

func TestEnsureReturnsMountsForAlreadyMaterialisedKeys(t *testing.T) {
	store := NewStore(t.TempDir(), PostureConnected)
	r := NewResolver(store, "linux-amd64")
	writeTree(t, store.Path("k1"), map[string]string{CompletionMarker: "key: k1\n"})

	mounts, err := r.Ensure(context.Background(), []Plan{{Entry: Entry{Ecosystem: EcosystemPip}, Key: "k1", Materialised: true}})
	if err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	if len(mounts) != 1 || mounts[0].HostPath != store.Path("k1") {
		t.Fatalf("Ensure() = %+v", mounts)
	}
}

func TestEnsureRefusesAnEcosystemSliceOneDoesNotMaterialise(t *testing.T) {
	r := NewResolver(NewStore(t.TempDir(), PostureConnected), "linux-amd64")
	_, err := r.Ensure(context.Background(), []Plan{{Entry: Entry{Ecosystem: EcosystemNPM}, Key: "k1"}})
	if err == nil || !strings.Contains(err.Error(), "slice 1 provisions pip only") {
		t.Fatalf("Ensure() = %v, want a not-yet-materialised refusal", err)
	}
}

func TestEnsureSurfacesTheAirGappedRefusal(t *testing.T) {
	store := NewStore(t.TempDir(), PostureAirGapped)
	r := NewResolver(store, "linux-amd64")

	_, err := r.Ensure(context.Background(), []Plan{{Entry: Entry{Ecosystem: EcosystemPip}, Key: "k1", LockfilePath: "/proj/requirements.lock"}})
	if !errors.Is(err, ErrAirGapped) {
		t.Fatalf("Ensure() = %v, want ErrAirGapped", err)
	}
}

func TestNewResolverDefaultsToTheMaterialisingHostsPlatform(t *testing.T) {
	// The materialising host is the one whose wheels the container
	// imports, so its platform is the honest default.
	r := NewResolver(NewStore(t.TempDir(), PostureConnected), "")
	if r.platform != Platform() {
		t.Fatalf("platform = %q, want %q", r.platform, Platform())
	}
	if r.Store() == nil {
		t.Fatal("Store() = nil")
	}
}
