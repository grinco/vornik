package api

import (
	"context"
	"fmt"
)

func (s *Server) validateExecutionTaskBinding(ctx context.Context, taskID, executionID string) error {
	if taskID == "" || executionID == "" || s == nil || s.executionRepo == nil {
		return nil
	}
	exec, err := s.executionRepo.Get(ctx, executionID)
	if err != nil || exec == nil {
		return fmt.Errorf("execution_id does not resolve")
	}
	if exec.TaskID != taskID {
		return fmt.Errorf("execution_id does not belong to task_id")
	}
	return nil
}
