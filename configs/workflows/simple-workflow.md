---
workflowId: "simple-workflow"
displayName: "Simple Development Workflow"
description: "Lightweight plan → implement → review pipeline for small one-shot development tasks; the reviewer can loop the coder once if the work isn't approved."
version: "1.1.0"
# Loop protection: max times any step can be revisited (rework cycles)
maxStepVisits: 3
# Hard ceiling on wall-clock duration. Linear plan → implement →
# review pipeline; 1h is generous for the small features this
# workflow targets while bounding a stuck implement step.
maxWallClock: "1h"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    on_success: "implement"
    on_fail: "failed"
    timeout: "15m"
  implement:
    type: "agent"
    role: "coder"
    on_success: "review"
    on_fail: "failed"
    timeout: "60m"
    retryPolicy:
      maxRetries: 2
      backoff: "exponential"
  review:
    type: "agent"
    role: "reviewer"
    on_fail: "failed"
    timeout: "30m"
    gates:
      - condition: "review.approved == true"
        target: "complete"
      - condition: "review.approved == false"
        target: "implement"
terminals:
  complete:
    status: "COMPLETED"
    message: "Task completed successfully"
  failed:
    status: "FAILED"
    message: "Task failed"
  cancelled:
    status: "CANCELLED"
    message: "Task was cancelled"
---

# Simple Development Workflow

A basic linear pipeline: plan → implement → review → complete.

## Prompts

### plan

Analyze the task and create an implementation plan. Break down the work
into actionable steps.

### implement

Implement the feature or fix according to the plan. Follow coding
standards and best practices.

### review

Review the implementation for correctness, quality, and adherence to
requirements.

## Error handling

`implement` retries up to `retryPolicy.maxRetries=2` on transient
failures. Reviewer rejections route back to `implement` up to
`maxStepVisits=3` times before the workflow terminates as `failed`.
