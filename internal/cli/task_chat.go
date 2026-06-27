package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// Phase 29 of the conversational task lifecycle. vornikctl gains
// the operator-side commands for talking to a task:
//
//   vornikctl task message <taskId>   --content "..."
//   vornikctl task directive <taskId> --content "..."   (course-correct + re-queue)
//   vornikctl task answer <taskId>    --checkpoint <id> --content "..." [--choice X]
//   vornikctl task amend <taskId>     --new-brief "..."
//   vornikctl task pause <taskId>
//   vornikctl task resume <taskId>
//   vornikctl task close <taskId>     [--reason "..."]
//   vornikctl task messages <taskId>  [--json]
//
// All commands wrap the api package's endpoints. -p / --project
// is required everywhere because the API path is project-scoped.

var (
	taskChatProject      string
	taskChatContent      string
	taskChatCheckpointID string
	taskChatChoice       string
	taskChatNewBrief     string
	taskChatReason       string
	taskChatAuthor       string
	taskChatJSON         bool
)

var taskMessageCmd = &cobra.Command{
	Use:   "message <taskId>",
	Short: "Post a message to a task's conversation thread",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskMessage,
}

var taskDirectiveCmd = &cobra.Command{
	Use:   "directive <taskId>",
	Short: "Post a directive (course correction) — re-queues the task on non-running state",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskDirective,
}

var taskAnswerCmd = &cobra.Command{
	Use:   "answer <taskId>",
	Short: "Reply to an open checkpoint and re-queue the task",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskAnswer,
}

var taskAmendCmd = &cobra.Command{
	Use:   "amend <taskId>",
	Short: "Amend the task brief; re-queues from non-running state",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskAmend,
}

var taskPauseCmd = &cobra.Command{
	Use:   "pause <taskId>",
	Short: "Pause an active task",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runTaskFlip(args[0], "pause") },
}

var taskResumeCmd = &cobra.Command{
	Use:   "resume <taskId>",
	Short: "Resume a paused task",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runTaskFlip(args[0], "resume") },
}

var taskCloseCmd = &cobra.Command{
	Use:   "close <taskId>",
	Short: "Close a task (operator-confirmed terminal)",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskClose,
}

var taskMessagesCmd = &cobra.Command{
	Use:   "messages <taskId>",
	Short: "List a task's conversation messages",
	Args:  cobra.ExactArgs(1),
	RunE:  runTaskMessages,
}

func init() {
	for _, c := range []*cobra.Command{
		taskMessageCmd, taskDirectiveCmd, taskAnswerCmd,
		taskAmendCmd, taskPauseCmd, taskResumeCmd,
		taskCloseCmd, taskMessagesCmd,
	} {
		c.Flags().StringVarP(&taskChatProject, "project", "p", "", "Project ID (required)")
		_ = c.MarkFlagRequired("project")
	}
	for _, c := range []*cobra.Command{taskMessageCmd, taskDirectiveCmd, taskAnswerCmd} {
		c.Flags().StringVar(&taskChatContent, "content", "", "Message content")
	}
	taskAnswerCmd.Flags().StringVar(&taskChatCheckpointID, "checkpoint", "", "Checkpoint message id (required)")
	taskAnswerCmd.Flags().StringVar(&taskChatChoice, "choice", "", "Selected option id (for decision checkpoints)")
	_ = taskAnswerCmd.MarkFlagRequired("checkpoint")
	taskAmendCmd.Flags().StringVar(&taskChatNewBrief, "new-brief", "", "New brief text")
	taskAmendCmd.Flags().StringVar(&taskChatReason, "reason", "", "Optional reason for the amendment")
	_ = taskAmendCmd.MarkFlagRequired("new-brief")
	taskCloseCmd.Flags().StringVar(&taskChatReason, "reason", "", "Optional closure reason")

	for _, c := range []*cobra.Command{taskMessageCmd, taskDirectiveCmd, taskAnswerCmd, taskAmendCmd, taskCloseCmd} {
		c.Flags().StringVar(&taskChatAuthor, "author", "", "Author identity (operator handle, defaults to OS user)")
	}
	taskMessagesCmd.Flags().BoolVar(&taskChatJSON, "json", false, "Output in JSON format")

	taskCmd.AddCommand(taskMessageCmd, taskDirectiveCmd, taskAnswerCmd,
		taskAmendCmd, taskPauseCmd, taskResumeCmd, taskCloseCmd, taskMessagesCmd)
}

func runTaskMessage(cmd *cobra.Command, args []string) error {
	return postChat(args[0], "messages", map[string]any{
		"kind":     "message",
		"content":  taskChatContent,
		"authorId": taskChatAuthor,
	})
}

func runTaskDirective(cmd *cobra.Command, args []string) error {
	return postChat(args[0], "messages", map[string]any{
		"kind":     "directive",
		"content":  taskChatContent,
		"authorId": taskChatAuthor,
	})
}

func runTaskAnswer(cmd *cobra.Command, args []string) error {
	if taskChatContent == "" && taskChatChoice == "" {
		return fmt.Errorf("answer requires --content, --choice, or both")
	}
	endpoint := fmt.Sprintf("messages/%s/answer", taskChatCheckpointID)
	return postChat(args[0], endpoint, map[string]any{
		"content":  taskChatContent,
		"choice":   taskChatChoice,
		"authorId": taskChatAuthor,
	})
}

func runTaskAmend(cmd *cobra.Command, args []string) error {
	return postChat(args[0], "amend", map[string]any{
		"newBrief": taskChatNewBrief,
		"reason":   taskChatReason,
		"authorId": taskChatAuthor,
	})
}

func runTaskFlip(taskID, verb string) error {
	return postChat(taskID, verb, nil)
}

func runTaskClose(cmd *cobra.Command, args []string) error {
	return postChat(args[0], "close", map[string]any{
		"reason":   taskChatReason,
		"authorId": taskChatAuthor,
	})
}

func runTaskMessages(cmd *cobra.Command, args []string) error {
	taskID := args[0]
	client := ClientFromEnv()
	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/messages", taskChatProject, taskID)
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	if taskChatJSON {
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	var out struct {
		Messages []struct {
			ID          string `json:"id"`
			AuthorKind  string `json:"author_kind"`
			MessageKind string `json:"message_kind"`
			Content     string `json:"content"`
			CreatedAt   string `json:"created_at"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if len(out.Messages) == 0 {
		fmt.Println("(no messages)")
		return nil
	}
	for _, m := range out.Messages {
		ts := m.CreatedAt
		if len(ts) > 19 {
			ts = ts[:19]
		}
		fmt.Printf("%s  %-12s %-15s  %s\n", ts, m.AuthorKind, m.MessageKind, summariseLine(m.Content, 80))
	}
	return nil
}

// postChat is the shared shape for every state-mutating chat
// command. action is the trailing path segment after /tasks/<id>/.
func postChat(taskID, action string, body any) error {
	client := ClientFromEnv()
	path := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/%s", taskChatProject, taskID, action)
	resp, err := client.Post(path, body)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return ParseAPIError(resp)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		// Empty/non-JSON success bodies are fine.
		fmt.Println("ok")
		return nil
	}
	if raw["error"] != nil {
		return fmt.Errorf("%v", raw["error"])
	}
	for k, v := range raw {
		fmt.Printf("%s: %v\n", k, v)
	}
	return nil
}

func summariseLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}
