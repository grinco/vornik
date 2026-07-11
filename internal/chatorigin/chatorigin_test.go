package chatorigin

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// --- fakes ------------------------------------------------------------

type fakeAudit struct {
	row *persistence.ChatAuditEntry
	err error
}

func (f fakeAudit) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return f.row, f.err
}

type fakeChannel struct {
	name string
	sent []conversation.ChannelMessage
}

func (f *fakeChannel) Name() string                                       { return f.name }
func (f *fakeChannel) Start(context.Context, conversation.Receiver) error { return nil }
func (f *fakeChannel) Stop() error                                        { return nil }
func (f *fakeChannel) Send(_ context.Context, m conversation.ChannelMessage) (string, error) {
	f.sent = append(f.sent, m)
	return "sent-1", nil
}
func (f *fakeChannel) ListSessions(context.Context) ([]conversation.Session, error) {
	return nil, nil
}
func (f *fakeChannel) ResolveSpeaker(context.Context, string) (conversation.Speaker, error) {
	return conversation.Speaker{}, nil
}

type fakeResolver struct {
	byName map[string]conversation.Channel
}

func (f fakeResolver) ResolveChannel(name string) conversation.Channel {
	if f.byName == nil {
		return nil
	}
	return f.byName[name]
}

type fakeTaskGetter struct {
	byID map[string]*persistence.Task
}

func (f fakeTaskGetter) Get(_ context.Context, id string) (*persistence.Task, error) {
	return f.byID[id], nil
}

func strp(s string) *string { return &s }

// --- Resolve ------------------------------------------------------------

func TestResolve_ChatOriginated(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", UserID: "telegram:555", ProjectID: "p1"}
	ch := &fakeChannel{name: "telegram"}
	res := fakeResolver{byName: map[string]conversation.Channel{"telegram": ch}}
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}

	got, ok := Resolve(context.Background(), task, nil, fakeAudit{row: row}, res)
	if !ok {
		t.Fatal("want ok=true for a chat-originated task")
	}
	if got.Channel != ch {
		t.Errorf("Channel = %v, want the resolved telegram fake", got.Channel)
	}
	if got.ChannelName != "telegram" {
		t.Errorf("ChannelName = %q, want telegram", got.ChannelName)
	}
	if got.SessionID != "555" {
		t.Errorf("SessionID = %q, want 555", got.SessionID)
	}
	if got.AuditRow == nil || got.AuditRow.UserID != "telegram:555" {
		t.Errorf("AuditRow not carried through: %+v", got.AuditRow)
	}
}

func TestResolve_ViaAncestor(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", ProjectID: "p1"}
	ch := &fakeChannel{name: "telegram"}
	res := fakeResolver{byName: map[string]conversation.Channel{"telegram": ch}}

	parentID := "parent"
	parent := &persistence.Task{ID: parentID, ProjectID: "p1", ChatTurnID: strp("chat_1")}
	child := &persistence.Task{ID: "child", ProjectID: "p1", ParentTaskID: &parentID}
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{parentID: parent}}

	got, ok := Resolve(context.Background(), child, getter, fakeAudit{row: row}, res)
	if !ok {
		t.Fatal("want ok=true via ancestor walk")
	}
	if got.SessionID != "555" {
		t.Errorf("SessionID = %q, want 555", got.SessionID)
	}
}

func TestResolve_NonChatOriginated_NotOK(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"} // no ChatTurnID, no parent
	_, ok := Resolve(context.Background(), task, nil, fakeAudit{}, fakeResolver{})
	if ok {
		t.Fatal("non-chat-originated task must resolve ok=false")
	}
}

func TestResolve_NilTask_NotOK(t *testing.T) {
	_, ok := Resolve(context.Background(), nil, nil, fakeAudit{}, fakeResolver{})
	if ok {
		t.Fatal("nil task must resolve ok=false")
	}
}

func TestResolve_AuditLookupError_NotOK(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	_, ok := Resolve(context.Background(), task, nil, fakeAudit{err: context.DeadlineExceeded}, fakeResolver{})
	if ok {
		t.Fatal("audit lookup error must resolve ok=false")
	}
}

func TestResolve_UnresolvableChannel_NotOK(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "web-chat:uuid", ProjectID: "p1"}
	task := &persistence.Task{ID: "t1", ProjectID: "p1", ChatTurnID: strp("chat_1")}
	_, ok := Resolve(context.Background(), task, nil, fakeAudit{row: row}, fakeResolver{}) // no channels wired
	if ok {
		t.Fatal("un-wired channel must resolve ok=false")
	}
}

func TestResolveForTurn_EmptyTurnID_NotOK(t *testing.T) {
	_, ok := ResolveForTurn(context.Background(), "", fakeAudit{}, fakeResolver{})
	if ok {
		t.Fatal("empty turn id must resolve ok=false")
	}
}

func TestResolveForTurn_NilAuditOrResolver_NotOK(t *testing.T) {
	if _, ok := ResolveForTurn(context.Background(), "chat_1", nil, fakeResolver{}); ok {
		t.Fatal("nil audit must resolve ok=false")
	}
	if _, ok := ResolveForTurn(context.Background(), "chat_1", fakeAudit{row: &persistence.ChatAuditEntry{}}, nil); ok {
		t.Fatal("nil resolver must resolve ok=false")
	}
}

// TestTurnID_MultiHopWalk pins the multi-generation ancestry
// walk: grandchild has no ChatTurnID, its parent has none either, only the
// grandparent does.
func TestTurnID_MultiHopWalk(t *testing.T) {
	grandparentID := "gp"
	parentID := "parent"
	grandparent := &persistence.Task{ID: grandparentID, ChatTurnID: strp("chat_gp")}
	parent := &persistence.Task{ID: parentID, ParentTaskID: &grandparentID}
	child := &persistence.Task{ID: "child", ParentTaskID: &parentID}
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{grandparentID: grandparent, parentID: parent}}

	got := TurnID(context.Background(), child, getter)
	if got != "chat_gp" {
		t.Errorf("TurnID = %q, want chat_gp (walked 2 hops)", got)
	}
}

// TestTurnID_CycleDetection guards against a corrupt
// ParentTaskID cycle spinning forever: a → b → a must terminate with "".
func TestTurnID_CycleDetection(t *testing.T) {
	aID, bID := "a", "b"
	a := &persistence.Task{ID: aID, ParentTaskID: &bID}
	b := &persistence.Task{ID: bID, ParentTaskID: &aID}
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{aID: a, bID: b}}

	got := TurnID(context.Background(), a, getter)
	if got != "" {
		t.Errorf("TurnID = %q, want empty (cycle must terminate)", got)
	}
}

// TestTurnID_MissingAncestor_TerminatesChain pins the "pruned/
// archived ancestor" case: getter.Get returning nil for the parent id ends
// the walk instead of erroring.
func TestTurnID_MissingAncestor_TerminatesChain(t *testing.T) {
	parentID := "gone"
	child := &persistence.Task{ID: "child", ParentTaskID: &parentID}
	getter := fakeTaskGetter{byID: map[string]*persistence.Task{}} // parent not found

	got := TurnID(context.Background(), child, getter)
	if got != "" {
		t.Errorf("TurnID = %q, want empty (missing ancestor)", got)
	}
}

// --- DecodeChatID ---------------------------------------------------------

func TestDecodeChatID(t *testing.T) {
	cases := []struct{ in, wantCh, wantSess string }{
		{"123456789", "telegram", "123456789"},
		{"slack:T1/C2#169.42", "slack", "T1/C2#169.42"},
		{"email:<abc@x.com>", "email", "<abc@x.com>"},
		{"web-chat:cookie-uuid", "web-chat", "cookie-uuid"},
		{"", "", ""},
		{"nocolon-nonnumeric", "", ""},
	}
	for _, c := range cases {
		ch, sess := DecodeChatID(c.in)
		if ch != c.wantCh || sess != c.wantSess {
			t.Errorf("DecodeChatID(%q) = (%q,%q), want (%q,%q)", c.in, ch, sess, c.wantCh, c.wantSess)
		}
	}
}

// --- EmailAddrFromUserID --------------------------------------------------

func TestEmailAddrFromUserID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"email:ops@x.com", "ops@x.com"},
		{"ops@x.com", "ops@x.com"},
		{"", ""},
	}
	for _, c := range cases {
		if got := EmailAddrFromUserID(c.in); got != c.want {
			t.Errorf("EmailAddrFromUserID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"123", true},
		{"12a", false},
	}
	for _, c := range cases {
		if got := isAllDigits(c.in); got != c.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- TurnID ------------------------------------------------------

func TestTurnID_NilTask(t *testing.T) {
	if got := TurnID(context.Background(), nil, nil); got != "" {
		t.Errorf("nil task must yield empty turn id, got %q", got)
	}
}
