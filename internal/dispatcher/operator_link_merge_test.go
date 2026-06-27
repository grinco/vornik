package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// fakeProfileRepo / fakeLinkRepo are in-memory stubs that
// implement the persistence interfaces the merge logic touches.
// Keeping them local + tightly-typed avoids reaching for sqlmock
// (the merge code is pure-Go business logic; we don't need to
// exercise SQL).
type fakeProfileRepo struct {
	mu       sync.Mutex
	profiles map[string]*persistence.OperatorProfile
}

func newFakeProfileRepo() *fakeProfileRepo {
	return &fakeProfileRepo{profiles: map[string]*persistence.OperatorProfile{}}
}

func (f *fakeProfileRepo) Get(_ context.Context, id string) (*persistence.OperatorProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.profiles[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	clone := *p
	return &clone, nil
}
func (f *fakeProfileRepo) Upsert(_ context.Context, p *persistence.OperatorProfile) error {
	if p == nil || p.OperatorID == "" {
		return errors.New("Upsert: operator id required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *p
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now()
	}
	clone.UpdatedAt = time.Now()
	f.profiles[p.OperatorID] = &clone
	return nil
}
func (f *fakeProfileRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.profiles, id)
	return nil
}
func (f *fakeProfileRepo) List(_ context.Context, _ int) ([]*persistence.OperatorProfile, error) {
	return nil, nil
}

type fakeLinkRepo struct {
	mu    sync.Mutex
	links map[string]*persistence.OperatorIdentityLink
}

func newFakeLinkRepo() *fakeLinkRepo {
	return &fakeLinkRepo{links: map[string]*persistence.OperatorIdentityLink{}}
}

func (f *fakeLinkRepo) Get(_ context.Context, id string) (*persistence.OperatorIdentityLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.links[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	clone := *l
	return &clone, nil
}
func (f *fakeLinkRepo) ListForOperator(_ context.Context, op string) ([]*persistence.OperatorIdentityLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*persistence.OperatorIdentityLink
	for _, l := range f.links {
		if l.OperatorID == op {
			clone := *l
			out = append(out, &clone)
		}
	}
	return out, nil
}
func (f *fakeLinkRepo) Upsert(_ context.Context, l *persistence.OperatorIdentityLink) error {
	if l == nil || l.ChannelSpeakerID == "" || l.OperatorID == "" {
		return errors.New("Upsert: ids required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *l
	if clone.LinkedAt.IsZero() {
		clone.LinkedAt = time.Now()
	}
	f.links[l.ChannelSpeakerID] = &clone
	return nil
}
func (f *fakeLinkRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.links, id)
	return nil
}
func (f *fakeLinkRepo) DeleteAllForOperator(_ context.Context, op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, l := range f.links {
		if l.OperatorID == op {
			delete(f.links, id)
		}
	}
	return nil
}

func TestPerformOperatorLink_SelfLinkRejected(t *testing.T) {
	repos := OperatorLinkRepos{Profiles: newFakeProfileRepo(), Links: newFakeLinkRepo()}
	_, err := PerformOperatorLink(context.Background(), repos, "tg:42", "tg:42", "self")
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("self-link should error, got %v", err)
	}
}

func TestPerformOperatorLink_BothPristine_IssuerWins(t *testing.T) {
	repos := OperatorLinkRepos{Profiles: newFakeProfileRepo(), Links: newFakeLinkRepo()}
	res, err := PerformOperatorLink(context.Background(), repos, "tg:42", "web:abc", "self")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if res.Canonical != "tg:42" {
		t.Errorf("canonical: %q want tg:42 (tie → issuer wins)", res.Canonical)
	}
	// Both speakers should resolve to the canonical.
	link, _ := repos.Links.Get(context.Background(), "web:abc")
	if link == nil || link.OperatorID != "tg:42" {
		t.Errorf("web:abc not linked to canonical: %#v", link)
	}
}

func TestPerformOperatorLink_LoserProfileMergedIntoWinner(t *testing.T) {
	profiles := newFakeProfileRepo()
	_ = profiles.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "web:abc",
		Structured: []byte(`{"tone":"terse","verbosity":"low"}`),
		Notes:      "prefers Czech UI",
	})
	_ = profiles.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "tg:42",
		Structured: []byte(`{"time_zone":"Europe/Prague"}`),
		Notes:      "",
	})
	repos := OperatorLinkRepos{Profiles: profiles, Links: newFakeLinkRepo()}
	res, err := PerformOperatorLink(context.Background(), repos, "web:abc", "tg:42", "self")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	// web:abc has 2 keys + notes; tg:42 has 1 key. web:abc wins.
	if res.Canonical != "web:abc" {
		t.Errorf("canonical: %q want web:abc (more-content wins)", res.Canonical)
	}
	if !res.Merged {
		t.Errorf("merge should have happened")
	}
	// Winner profile now carries tg:42's time_zone too.
	merged, _ := profiles.Get(context.Background(), "web:abc")
	if merged == nil {
		t.Fatal("winner profile missing")
	}
	var struct1 map[string]any
	_ = json.Unmarshal(merged.Structured, &struct1)
	if struct1["time_zone"] != "Europe/Prague" {
		t.Errorf("merged structured lost loser key: %v", struct1)
	}
	if struct1["tone"] != "terse" {
		t.Errorf("merged structured lost winner key: %v", struct1)
	}
	// Loser profile is gone.
	if _, err := profiles.Get(context.Background(), "tg:42"); err == nil {
		t.Errorf("loser profile should have been deleted")
	}
}

func TestPerformOperatorLink_WinnerProfileWinsOnConflict(t *testing.T) {
	profiles := newFakeProfileRepo()
	_ = profiles.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "web:abc",
		Structured: []byte(`{"tone":"terse"}`),
	})
	_ = profiles.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "tg:42",
		Structured: []byte(`{"tone":"verbose"}`),
	})
	repos := OperatorLinkRepos{Profiles: profiles, Links: newFakeLinkRepo()}
	_, err := PerformOperatorLink(context.Background(), repos, "web:abc", "tg:42", "self")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	merged, _ := profiles.Get(context.Background(), "web:abc")
	var s map[string]any
	_ = json.Unmarshal(merged.Structured, &s)
	if s["tone"] != "terse" {
		t.Errorf("winner should keep its conflicting value: %v", s)
	}
}

func TestPerformOperatorLink_NotesAppendedWithSeparator(t *testing.T) {
	profiles := newFakeProfileRepo()
	_ = profiles.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "web:abc", Notes: "prefers Czech UI",
		Structured: []byte(`{"tone":"terse","verbosity":"low"}`),
	})
	_ = profiles.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "tg:42", Notes: "uses metric units",
	})
	repos := OperatorLinkRepos{Profiles: profiles, Links: newFakeLinkRepo()}
	_, err := PerformOperatorLink(context.Background(), repos, "web:abc", "tg:42", "self")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	merged, _ := profiles.Get(context.Background(), "web:abc")
	if !strings.Contains(merged.Notes, "prefers Czech UI") {
		t.Errorf("winner notes lost: %q", merged.Notes)
	}
	if !strings.Contains(merged.Notes, "uses metric units") {
		t.Errorf("loser notes not appended: %q", merged.Notes)
	}
	if !strings.Contains(merged.Notes, "[merged from tg:42") {
		t.Errorf("merge separator missing: %q", merged.Notes)
	}
}

func TestPerformOperatorLink_ExistingLinksRepointed(t *testing.T) {
	links := newFakeLinkRepo()
	// tg:42 has two pre-existing peers, both pointing at it
	// as their canonical.
	_ = links.Upsert(context.Background(), &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "tg:other-1", OperatorID: "tg:42", LinkedBy: "cli",
	})
	_ = links.Upsert(context.Background(), &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "tg:other-2", OperatorID: "tg:42", LinkedBy: "cli",
	})
	profiles := newFakeProfileRepo()
	// Make web:abc the winner via more content.
	_ = profiles.Upsert(context.Background(), &persistence.OperatorProfile{
		OperatorID: "web:abc",
		Structured: []byte(`{"tone":"terse","verbosity":"low","time_zone":"Europe/Prague"}`),
	})
	repos := OperatorLinkRepos{Profiles: profiles, Links: links}

	_, err := PerformOperatorLink(context.Background(), repos, "web:abc", "tg:42", "self")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	for _, id := range []string{"tg:other-1", "tg:other-2", "tg:42"} {
		row, err := links.Get(context.Background(), id)
		if err != nil {
			t.Errorf("link %s missing post-merge: %v", id, err)
			continue
		}
		if row.OperatorID != "web:abc" {
			t.Errorf("link %s still pointing at old canonical: %q", id, row.OperatorID)
		}
	}
}

func TestPerformOperatorLink_AlreadyLinkedNoOp(t *testing.T) {
	links := newFakeLinkRepo()
	// Both speakers already point at the same canonical.
	_ = links.Upsert(context.Background(), &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "tg:42", OperatorID: "web:canon",
	})
	_ = links.Upsert(context.Background(), &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "tg:99", OperatorID: "web:canon",
	})
	repos := OperatorLinkRepos{Profiles: newFakeProfileRepo(), Links: links}
	res, err := PerformOperatorLink(context.Background(), repos, "tg:42", "tg:99", "self")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if res.Canonical != "web:canon" {
		t.Errorf("already-linked path should resolve to web:canon, got %q", res.Canonical)
	}
	if res.Merged {
		t.Errorf("no merge should have happened")
	}
}

func TestPerformOperatorLink_RequiresRepos(t *testing.T) {
	_, err := PerformOperatorLink(context.Background(), OperatorLinkRepos{}, "a", "b", "self")
	if err == nil || !strings.Contains(err.Error(), "repositories required") {
		t.Errorf("missing repos must error, got %v", err)
	}
}
