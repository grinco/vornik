package memory

import (
	"context"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
)

// overridableProvider is a titlerFakeProvider that also implements
// chat.ModelOverridable so pickModelForTitler exercises its
// type-assertion branch.
type overridableProvider struct {
	*titlerFakeProvider
	withModelArg string
}

func (o *overridableProvider) WithModel(model string) chat.Provider {
	o.withModelArg = model
	return o
}

func TestPickModelForTitler_NoOverride(t *testing.T) {
	fp := &titlerFakeProvider{}
	if got := pickModelForTitler(fp, ""); got != fp {
		t.Fatal("empty model: expect same provider")
	}
}

func TestPickModelForTitler_NonOverridableReturnedAsIs(t *testing.T) {
	fp := &titlerFakeProvider{}
	if got := pickModelForTitler(fp, "x"); got != fp {
		t.Fatal("non-overridable: expect same provider")
	}
}

func TestPickModelForTitler_OverridableInvoked(t *testing.T) {
	op := &overridableProvider{titlerFakeProvider: &titlerFakeProvider{}}
	got := pickModelForTitler(op, "alt-model")
	if got != op {
		t.Fatal("expect overridable clone returned")
	}
	if op.withModelArg != "alt-model" {
		t.Fatalf("WithModel arg: %q", op.withModelArg)
	}
}

func TestTitle_NilReceiverOrClient(t *testing.T) {
	var nilT *Titler
	got, err := nilT.Title(context.Background(), "x", "", "")
	if got != "" || err == nil {
		t.Fatalf("nil receiver: %q %v", got, err)
	}
	tr := &Titler{}
	got, err = tr.Title(context.Background(), "x", "", "")
	if got != "" || err == nil {
		t.Fatalf("nil client: %q %v", got, err)
	}
}

func TestTitle_TruncatesLongContent(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "Short Topic"}}}
	tr := NewTitler(fp, "model-x")
	tr.MaxPreviewBytes = 16
	long := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu"
	if _, err := tr.Title(context.Background(), long, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateUTF8BytesDoesNotSplitRune(t *testing.T) {
	got := truncateUTF8Bytes("abc€def", 5)
	if got != "abc" {
		t.Fatalf("got %q, want abc", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated string is invalid UTF-8: %q", got)
	}
}
