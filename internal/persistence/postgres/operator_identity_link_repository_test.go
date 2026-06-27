package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

func TestOperatorIdentityLink_Get_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	now := time.Date(2026, 5, 24, 22, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT channel_speaker_id, operator_id, linked_at, linked_by")).
		WithArgs("tg:42").
		WillReturnRows(sqlmock.NewRows([]string{"channel_speaker_id", "operator_id", "linked_at", "linked_by"}).
			AddRow("tg:42", "web:abc", now, "self"))

	got, err := repo.Get(context.Background(), "tg:42")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OperatorID != "web:abc" {
		t.Errorf("operator_id: %q", got.OperatorID)
	}
	if got.LinkedBy != "self" {
		t.Errorf("linked_by: %q", got.LinkedBy)
	}
}

func TestOperatorIdentityLink_Get_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT channel_speaker_id, operator_id, linked_at, linked_by")).
		WithArgs("tg:nobody").
		WillReturnRows(sqlmock.NewRows([]string{"channel_speaker_id", "operator_id", "linked_at", "linked_by"}))

	_, err := repo.Get(context.Background(), "tg:nobody")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestOperatorIdentityLink_Get_EmptyID(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)
	_, err := repo.Get(context.Background(), "")
	if err == nil {
		t.Errorf("empty id must error")
	}
}

func TestOperatorIdentityLink_ListForOperator(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	now := time.Date(2026, 5, 24, 22, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT channel_speaker_id, operator_id, linked_at, linked_by")).
		WithArgs("web:abc").
		WillReturnRows(sqlmock.NewRows([]string{"channel_speaker_id", "operator_id", "linked_at", "linked_by"}).
			AddRow("tg:42", "web:abc", now, "self").
			AddRow("slack:U1", "web:abc", now.Add(time.Hour), "cli"))

	rows, err := repo.ListForOperator(context.Background(), "web:abc")
	if err != nil {
		t.Fatalf("ListForOperator: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].ChannelSpeakerID != "tg:42" {
		t.Errorf("row 0 speaker: %q", rows[0].ChannelSpeakerID)
	}
}

func TestOperatorIdentityLink_Upsert(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO operator_identity_link")).
		WithArgs("tg:42", "web:abc", "self").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Upsert(context.Background(), &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "tg:42",
		OperatorID:       "web:abc",
		LinkedBy:         "self",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func TestOperatorIdentityLink_Upsert_DefaultsLinkedBy(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO operator_identity_link")).
		WithArgs("tg:42", "web:abc", "self"). // empty linked_by → default "self"
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Upsert(context.Background(), &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "tg:42",
		OperatorID:       "web:abc",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func TestOperatorIdentityLink_Upsert_Empty(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	if err := repo.Upsert(context.Background(), nil); err == nil {
		t.Error("nil link should error")
	}
	if err := repo.Upsert(context.Background(), &persistence.OperatorIdentityLink{}); err == nil {
		t.Error("empty link should error")
	}
}

func TestOperatorIdentityLink_Delete(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM operator_identity_link WHERE channel_speaker_id")).
		WithArgs("tg:42").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Delete(context.Background(), "tg:42"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestOperatorIdentityLink_DeleteAllForOperator(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewOperatorIdentityLinkRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM operator_identity_link WHERE operator_id")).
		WithArgs("web:abc").
		WillReturnResult(sqlmock.NewResult(0, 3))

	if err := repo.DeleteAllForOperator(context.Background(), "web:abc"); err != nil {
		t.Errorf("DeleteAllForOperator: %v", err)
	}
}
