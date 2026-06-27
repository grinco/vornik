package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func taskMessageRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "task_id", "execution_id", "parent_id",
		"author_kind", "author_id", "message_kind", "content",
		"metadata", "created_at",
	})
}

func TestTaskMessageRepositoryInsertValidationAndTransaction(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskMessageRepository(db)

	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("expected nil message error")
	}
	if err := repo.Insert(context.Background(), &persistence.TaskMessage{}); err == nil {
		t.Fatal("expected validation error")
	}

	execID := "exec-1"
	authorID := "user-1"
	msg := &persistence.TaskMessage{
		TaskID: "task-1", ExecutionID: &execID, AuthorKind: "operator", AuthorID: &authorID,
		MessageKind: persistence.TaskMessageKindCheckpoint, Content: "need approval",
		Metadata: []byte(`{"resolved":false}`),
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_messages")).
		WithArgs(sqlmock.AnyArg(), msg.TaskID, msg.ExecutionID, msg.ParentID, msg.AuthorKind, msg.AuthorID, msg.MessageKind, msg.Content, `{"resolved":false}`, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks")).
		WithArgs(sqlmock.AnyArg(), msg.TaskID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := repo.Insert(context.Background(), msg); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if msg.ID == "" || msg.CreatedAt.IsZero() {
		t.Fatalf("Insert() did not set defaults: %#v", msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestTaskMessageRepositoryListAndCheckpointFlow(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskMessageRepository(db)

	if _, err := repo.List(context.Background(), persistence.TaskMessageFilter{}); err == nil {
		t.Fatal("expected missing task id error")
	}
	if _, err := repo.GetOpenCheckpoint(context.Background(), ""); err == nil {
		t.Fatal("expected missing task id error")
	}
	if err := repo.MarkCheckpointResolved(context.Background(), "", "msg-1"); err == nil {
		t.Fatal("expected missing ids error")
	}

	execID := "exec-1"
	parentID := "parent-1"
	authorID := "lead"
	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	after := "msg-0"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, execution_id, parent_id")).
		WithArgs("task-1", after, sqlmock.AnyArg(), 25).
		WillReturnRows(taskMessageRows().AddRow(
			"msg-1", "task-1", execID, parentID,
			"assistant", authorID, persistence.TaskMessageKindMessage, "hello",
			`{"k":1}`, created,
		))
	msgs, err := repo.List(context.Background(), persistence.TaskMessageFilter{
		TaskID: "task-1", After: &after, MessageKinds: []string{persistence.TaskMessageKindMessage}, Limit: 25,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "msg-1" || string(msgs[0].Metadata) != `{"k":1}` {
		t.Fatalf("List() = %#v", msgs)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT open_checkpoint_id FROM tasks WHERE id = $1")).
		WithArgs("task-empty").
		WillReturnRows(sqlmock.NewRows([]string{"open_checkpoint_id"}).AddRow(nil))
	open, err := repo.GetOpenCheckpoint(context.Background(), "task-empty")
	if err != nil || open != nil {
		t.Fatalf("GetOpenCheckpoint(empty) = %#v, %v", open, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT open_checkpoint_id FROM tasks WHERE id = $1")).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{"open_checkpoint_id"}).AddRow("msg-2"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, execution_id, parent_id")).
		WithArgs("msg-2").
		WillReturnRows(taskMessageRows().AddRow(
			"msg-2", "task-1", nil, nil,
			"assistant", nil, persistence.TaskMessageKindCheckpoint, "approve?",
			`{"resolved":false}`, created,
		))
	open, err = repo.GetOpenCheckpoint(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetOpenCheckpoint() error = %v", err)
	}
	if open == nil || open.ID != "msg-2" || string(open.Metadata) != `{"resolved":false}` {
		t.Fatalf("GetOpenCheckpoint() = %#v", open)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT open_checkpoint_id FROM tasks WHERE id = $1")).
		WithArgs("task-dangling").
		WillReturnRows(sqlmock.NewRows([]string{"open_checkpoint_id"}).AddRow("missing-msg"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, execution_id, parent_id")).
		WithArgs("missing-msg").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks SET open_checkpoint_id = NULL WHERE id = $1")).
		WithArgs("task-dangling").
		WillReturnResult(sqlmock.NewResult(0, 1))
	open, err = repo.GetOpenCheckpoint(context.Background(), "task-dangling")
	if err != nil || open != nil {
		t.Fatalf("GetOpenCheckpoint(dangling) = %#v, %v", open, err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE task_messages")).
		WithArgs("msg-2", "task-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks")).
		WithArgs("task-1", "msg-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkCheckpointResolved(context.Background(), "task-1", "msg-2"); err != nil {
		t.Fatalf("MarkCheckpointResolved() error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestJSONOrNullValidation(t *testing.T) {
	if got := jsonOrNull(nil); got != nil {
		t.Fatalf("jsonOrNull(nil) = %#v, want nil", got)
	}
	if got := jsonOrNull([]byte(`{`)); got != nil {
		t.Fatalf("jsonOrNull(invalid) = %#v, want nil", got)
	}
	if got := jsonOrNull([]byte(`{"ok":true}`)); got != `{"ok":true}` {
		t.Fatalf("jsonOrNull(valid) = %#v", got)
	}
}
