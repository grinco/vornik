package email

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// NewIMAPClient returns the production IMAPClient adapter, backed
// by github.com/emersion/go-imap/v2. Use this when wiring the email
// channel against a real IMAP server; tests in this package use
// the in-memory fakeIMAPClient instead.
//
// The adapter dials TLS on Connect (port 993 by default), runs
// LOGIN, SELECTs the configured mailbox, then leaves the connection
// open for FetchUnseen / MarkSeen calls. Close runs LOGOUT and
// tears the connection down.
//
// Slice 1 deliberately keeps the adapter simple: one connection
// per channel, no IDLE/polling-via-server-push, no reconnect on
// transport drop. The poll loop will surface a transport drop as
// a "fetch failed" log line on the next cycle; a slice-2
// hardening pass adds auto-reconnect.
func NewIMAPClient() IMAPClient {
	return &emersionIMAPClient{}
}

type emersionIMAPClient struct {
	mu     sync.Mutex
	client *imapclient.Client
	closed bool

	// lastCfg holds the IMAPDialConfig passed at the most recent
	// successful Connect call. Reconnect reuses it so the channel
	// doesn't have to thread credentials through a second time.
	// Zero-value means "Connect has never succeeded" — Reconnect
	// in that state returns an error rather than dialling against
	// empty credentials.
	lastCfg IMAPDialConfig
}

func (c *emersionIMAPClient) Connect(ctx context.Context, cfg IMAPDialConfig) error {
	port := cfg.Port
	if port == 0 {
		port = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)

	cli, err := imapclient.DialTLS(addr, nil)
	if err != nil {
		return fmt.Errorf("dial TLS %s: %w", addr, err)
	}
	if err := cli.Login(cfg.Username, cfg.Password).Wait(); err != nil {
		_ = cli.Close()
		return fmt.Errorf("IMAP login: %w", err)
	}
	mailbox := cfg.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if _, err := cli.Select(mailbox, nil).Wait(); err != nil {
		_ = cli.Logout().Wait()
		_ = cli.Close()
		return fmt.Errorf("IMAP select %q: %w", mailbox, err)
	}

	c.mu.Lock()
	c.client = cli
	c.closed = false
	c.lastCfg = cfg
	c.mu.Unlock()
	return nil
}

// Reconnect tears down the existing IMAP connection (if any) and
// dials a fresh one using the credentials cached on the most recent
// successful Connect. Returns a sentinel-shaped error when Connect
// has never succeeded — the channel layer uses that as the signal
// to wait for the next poll cycle instead of looping on a
// dead client.
//
// Slice-2 hardening pass over slice-1's "log the fetch error and
// hope it heals." Mirrors the runtime/warmpool reconnect pattern:
// idempotent enough to call from multiple goroutines but the channel
// serialises around its own runPollCycle so the practical caller
// shape is one-at-a-time.
func (c *emersionIMAPClient) Reconnect(ctx context.Context) error {
	c.mu.Lock()
	cfg := c.lastCfg
	c.mu.Unlock()
	if strings.TrimSpace(cfg.Host) == "" || strings.TrimSpace(cfg.Username) == "" {
		return errors.New("IMAP client: not connected — Reconnect requires a prior successful Connect")
	}
	// Tear down whatever's there. Ignore Close errors — we're about
	// to throw away the connection regardless.
	_ = c.Close()
	// Close cleared lastCfg via the c.client = nil path? No — Close
	// touches only client + closed. Re-set so the new Connect call
	// re-stamps it. We can just re-invoke Connect to redial.
	return c.Connect(ctx, cfg)
}

func (c *emersionIMAPClient) FetchUnseen(ctx context.Context) ([]RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil {
		return nil, errors.New("IMAP client: not connected")
	}

	// NOOP forces the server to flush any queued untagged responses
	// (EXISTS / RECENT / FETCH) accumulated since the last command.
	// Without this, Gmail (and other servers that batch unilateral
	// updates) can leave the client's mailbox state stale after a
	// long idle stretch — the next SEARCH then sees a snapshot from
	// before the new mail arrived. Symptom: a fresh inbound message
	// silently fails to appear on every subsequent poll cycle until
	// the connection is dropped and re-established. NOOP is cheap
	// (one round-trip, no payload) and the canonical IMAP idiom for
	// "give me your current view."
	if err := cli.Noop().Wait(); err != nil {
		// A NOOP failure is itself a transport-level signal — the
		// channel-side runPollCycle's transport-error path will
		// trigger Reconnect on the next cycle when FetchUnseen
		// returns this error.
		return nil, fmt.Errorf("NOOP: %w", err)
	}

	// SEARCH for messages without the \Seen flag. Per RFC 3501,
	// UID SEARCH UNSEEN is what every mainstream provider supports.
	criteria := &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}
	searchData, err := cli.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("UID SEARCH: %w", err)
	}
	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}

	set := imap.UIDSetNum(uids...)
	bs := &imap.FetchItemBodySection{Peek: true}
	fetchOpts := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bs},
	}
	fetchCmd := cli.Fetch(set, fetchOpts)
	defer func() { _ = fetchCmd.Close() }()

	msgs, err := fetchCmd.Collect()
	if err != nil {
		return nil, fmt.Errorf("FETCH: %w", err)
	}

	out := make([]RawMessage, 0, len(msgs))
	for _, m := range msgs {
		body := m.FindBodySection(bs)
		if body == nil {
			continue
		}
		out = append(out, RawMessage{
			UID:  strconv.FormatUint(uint64(m.UID), 10),
			Body: body,
		})
	}
	return out, nil
}

func (c *emersionIMAPClient) MarkSeen(ctx context.Context, uid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil {
		return errors.New("IMAP client: not connected")
	}

	parsed, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		return fmt.Errorf("parse UID %q: %w", uid, err)
	}
	set := imap.UIDSetNum(imap.UID(parsed))
	storeOp := &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}
	storeCmd := cli.Store(set, storeOp, nil)
	if _, err := storeCmd.Collect(); err != nil {
		return fmt.Errorf("UID STORE +FLAGS \\Seen: %w", err)
	}
	return nil
}

func (c *emersionIMAPClient) Close() error {
	c.mu.Lock()
	cli := c.client
	already := c.closed
	c.closed = true
	c.client = nil
	c.mu.Unlock()
	if already || cli == nil {
		return nil
	}
	_ = cli.Logout().Wait()
	return cli.Close()
}
