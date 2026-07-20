package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type fakeNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
	err   error
}

type notifyCall struct {
	project, host, reason, detail, loginCmd string
	screenshot                              []byte
}

func (f *fakeNotifier) NotifyScraperBlock(_ context.Context, project, host, reason, detail, loginCmd string, ss []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, notifyCall{project, host, reason, detail, loginCmd, ss})
	return f.err
}
func (f *fakeNotifier) last() notifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func webFetchResult(reason, detail, screenshot string) string {
	m := map[string]any{"status": 200, "block_reason": reason, "block_detail": detail}
	if screenshot != "" {
		m["block_screenshot"] = screenshot
	}
	b, _ := json.Marshal(m)
	return string(b)
}
func urlArgs(u string) string {
	b, _ := json.Marshal(map[string]any{"url": u})
	return string(b)
}
func newBN(fn OperatorNotifier) *BlockNotifier {
	return NewBlockNotifier([]string{"tripadvisor.com", "maps.google.com"}, time.Hour, fn, zerolog.Nop())
}

func TestBlockNotify_Gate(t *testing.T) {
	cases := []struct {
		name            string
		tool, args, res string
		wantEnqueued    bool
	}{
		{"solvable+curated (subdomain)", "web_fetch", urlArgs("https://www.tripadvisor.com/Hotel"), webFetchResult("captcha", "cf", ""), true},
		{"solvable+curated (apex)", "web_fetch", urlArgs("https://maps.google.com/place"), webFetchResult("login_wall", "", ""), true},
		{"non-curated host", "web_fetch", urlArgs("https://example.com/x"), webFetchResult("captcha", "", ""), false},
		{"non-solvable reason", "web_fetch", urlArgs("https://www.tripadvisor.com/x"), webFetchResult("paywall", "", ""), false},
		{"not web_fetch", "encode_image", urlArgs("https://www.tripadvisor.com/x"), webFetchResult("captcha", "", ""), false},
		{"malformed result json", "web_fetch", urlArgs("https://www.tripadvisor.com/x"), "{not json", false},
		{"no block", "web_fetch", urlArgs("https://www.tripadvisor.com/x"), webFetchResult("none", "", ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bn := newBN(&fakeNotifier{})
			bn.MaybeNotify("assistant", tc.tool, tc.args, tc.res)
			got := len(bn.queue) == 1
			if got != tc.wantEnqueued {
				t.Fatalf("enqueued=%v, want %v", got, tc.wantEnqueued)
			}
		})
	}
}

func TestBlockNotify_MatchPortal(t *testing.T) {
	bn := newBN(&fakeNotifier{}) // curated: tripadvisor.com, maps.google.com
	cases := []struct {
		host, want string
		ok         bool
	}{
		{"tripadvisor.com", "tripadvisor.com", true},
		{"www.tripadvisor.com", "tripadvisor.com", true},
		{"login.tripadvisor.com", "tripadvisor.com", true},
		{"maps.google.com", "maps.google.com", true},
		{"eviltripadvisor.com", "", false},      // no dot boundary → no bypass
		{"tripadvisor.com.evil.com", "", false}, // suffix trickery → no match
		{"example.com", "", false},
	}
	for _, c := range cases {
		got, ok := bn.matchPortal(c.host)
		if ok != c.ok || got != c.want {
			t.Errorf("matchPortal(%q) = %q,%v; want %q,%v", c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestBlockNotify_ClaimPrunesStaleEntries(t *testing.T) {
	bn := newBN(&fakeNotifier{}) // cooldown 1h
	base := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	cur := base
	bn.now = func() time.Time { return cur }
	bn.claim("p1", "tripadvisor.com")
	// Far in the future (> 2× cooldown): the stale p1 entry must be pruned
	// when a new claim sweeps.
	cur = base.Add(10 * time.Hour)
	bn.claim("p2", "maps.google.com")
	if _, ok := bn.last["p1\x00tripadvisor.com"]; ok {
		t.Fatal("stale entry should have been pruned")
	}
	if len(bn.last) != 1 {
		t.Fatalf("want 1 live entry after prune, got %d", len(bn.last))
	}
}

func TestBlockNotify_NilNotifierIsInert(t *testing.T) {
	bn := NewBlockNotifier([]string{"tripadvisor.com"}, time.Hour, nil, zerolog.Nop())
	bn.MaybeNotify("assistant", "web_fetch", urlArgs("https://www.tripadvisor.com/x"), webFetchResult("captcha", "", ""))
	if len(bn.queue) != 0 {
		t.Fatalf("nil notifier must not enqueue")
	}
}

func TestBlockNotify_Dedup(t *testing.T) {
	bn := newBN(&fakeNotifier{})
	base := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	cur := base
	bn.now = func() time.Time { return cur }

	fire := func() {
		bn.MaybeNotify("assistant", "web_fetch", urlArgs("https://www.tripadvisor.com/x"), webFetchResult("captcha", "", ""))
	}
	fire()
	fire() // within cooldown → deduped
	if len(bn.queue) != 1 {
		t.Fatalf("want 1 after dedup, got %d", len(bn.queue))
	}
	cur = base.Add(2 * time.Hour) // past the 1h cooldown
	fire()
	if len(bn.queue) != 2 {
		t.Fatalf("want 2 after cooldown elapsed, got %d", len(bn.queue))
	}
}

func TestBlockNotify_QueueFullDropsNeverBlocks(t *testing.T) {
	bn := newBN(&fakeNotifier{})
	// Distinct projects → distinct dedup keys, so each passes the cooldown
	// gate and competes for a queue slot. Fill past capacity; the worker is
	// NOT started, so nothing drains.
	for i := 0; i < blockNotifyQueueSize+8; i++ {
		bn.MaybeNotify("proj"+string(rune('a'+i)), "web_fetch",
			urlArgs("https://www.tripadvisor.com/x"), webFetchResult("captcha", "", ""))
	}
	if len(bn.queue) != blockNotifyQueueSize {
		t.Fatalf("queue should be full at cap %d, got %d", blockNotifyQueueSize, len(bn.queue))
	}
}

func TestBlockNotify_DeliverFormatsLoginCmdAndScreenshot(t *testing.T) {
	fn := &fakeNotifier{}
	bn := newBN(fn)
	bn.deliver(context.Background(), blockNotifyJob{
		project: "assistant", host: "tripadvisor.com",
		reqURL: "https://www.tripadvisor.com/Hotel_Review", reason: "captcha", detail: "cf challenge",
		screenshot: []byte{1, 2, 3},
	})
	c := fn.last()
	if c.loginCmd != "vornikctl scraper login start -p assistant --url https://www.tripadvisor.com/Hotel_Review" {
		t.Errorf("unexpected login cmd: %q", c.loginCmd)
	}
	if string(c.screenshot) != string([]byte{1, 2, 3}) {
		t.Errorf("screenshot not passed through: %v", c.screenshot)
	}
	if c.project != "assistant" || c.host != "tripadvisor.com" || c.reason != "captcha" {
		t.Errorf("fields wrong: %+v", c)
	}
}

func TestBlockNotify_DeliverSwallowsError(t *testing.T) {
	fn := &fakeNotifier{err: context.DeadlineExceeded}
	bn := newBN(fn)
	// deliver must not panic/propagate when the sink errors — the notifier is
	// still invoked once; the error is swallowed (logged + metric only).
	bn.deliver(context.Background(), blockNotifyJob{project: "assistant", host: "tripadvisor.com", reqURL: "https://x", reason: "captcha"})
	if len(fn.calls) != 1 {
		t.Fatalf("deliver should invoke the notifier once even on error, got %d", len(fn.calls))
	}
}

func TestBlockNotify_ScreenshotDataURIThreaded(t *testing.T) {
	bn := newBN(&fakeNotifier{})
	raw := []byte{0xff, 0xd8, 0xff, 0x00} // pretend JPEG bytes
	uri := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw)
	bn.MaybeNotify("assistant", "web_fetch", urlArgs("https://www.tripadvisor.com/x"), webFetchResult("captcha", "", uri))
	job := <-bn.queue
	if string(job.screenshot) != string(raw) {
		t.Fatalf("screenshot bytes not decoded from data URI: %v", job.screenshot)
	}
}

func TestDecodeDataURI(t *testing.T) {
	raw := []byte("hello")
	uri := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw)
	if got := decodeDataURI(uri); string(got) != "hello" {
		t.Errorf("decode: got %q", got)
	}
	for _, bad := range []string{"", "not a uri", "data:image/jpeg,plain", "https://x/y.jpg"} {
		if decodeDataURI(bad) != nil {
			t.Errorf("expected nil for %q", bad)
		}
	}
}
