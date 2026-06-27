#!/bin/sh
# Fake Agent Entrypoint
# Simulates an agent by reading task.json and writing result.json
# Exit 0 = success, non-zero = failure

set -e

INPUT_FILE="/app/input/task.json"
OUTPUT_FILE="/app/output/result.json"
CANCEL_FILE="/app/input/CANCEL"
WORKSPACE="/app/workspace"
START_TIME=$(date +%s)

log() {
    echo "[fake-agent] $1"
}

# Check for cancellation marker
check_cancellation() {
    if [ -f "$CANCEL_FILE" ]; then
        log "Cancellation requested, exiting gracefully"
        write_cancelled_result
        exit 0
    fi
}

# Write a success result
write_success_result() {
    local task_id="$1"
    local prompt="$2"
    local duration="$3"
    
    # Create a dummy output artifact
    mkdir -p "$WORKSPACE/artifacts/out"
    echo "# Fake Agent Output\n\nTask: $task_id\nPrompt: $prompt\nCompleted at: $(date -Iseconds)" > "$WORKSPACE/artifacts/out/result.txt"
    
    cat > "$OUTPUT_FILE" <<EOF
{
  "status": "COMPLETED",
  "message": "Fake agent successfully processed task: $task_id",
  "outputArtifacts": [
    {
      "name": "result.txt",
      "path": "/app/workspace/artifacts/out/result.txt"
    }
  ],
  "delegatedTasks": [],
  "diagnostics": {
    "exitCode": 0,
    "durationSeconds": $duration
  }
}
EOF
    log "Wrote success result to $OUTPUT_FILE"
}

# Write a failure result
write_failure_result() {
    local task_id="$1"
    local error_msg="$2"
    local duration="$3"
    
    cat > "$OUTPUT_FILE" <<EOF
{
  "status": "FAILED",
  "message": "Fake agent failed: $error_msg",
  "outputArtifacts": [],
  "delegatedTasks": [],
  "diagnostics": {
    "exitCode": 1,
    "durationSeconds": $duration,
    "error": "$error_msg"
  }
}
EOF
    log "Wrote failure result to $OUTPUT_FILE"
}

# Write a cancelled result
write_cancelled_result() {
    local duration=$(( $(date +%s) - START_TIME ))
    
    cat > "$OUTPUT_FILE" <<EOF
{
  "status": "CANCELLED",
  "message": "Fake agent was cancelled",
  "outputArtifacts": [],
  "delegatedTasks": [],
  "diagnostics": {
    "exitCode": 0,
    "durationSeconds": $duration
  }
}
EOF
    log "Wrote cancelled result to $OUTPUT_FILE"
}

# Calculate duration
get_duration() {
    echo $(( $(date +%s) - START_TIME ))
}

# Main execution
main() {
    log "Starting fake agent"
    
    # Check for cancellation at start
    check_cancellation
    
    # Verify input file exists
    if [ ! -f "$INPUT_FILE" ]; then
        log "ERROR: Input file not found: $INPUT_FILE"
        write_failure_result "unknown" "Input file not found: $INPUT_FILE" "$(get_duration)"
        exit 1
    fi
    
    log "Reading task from $INPUT_FILE"
    
    # Parse task.json
    task_id=$(jq -r '.taskId // "unknown"' "$INPUT_FILE")
    prompt=$(jq -r '.context.prompt // "no prompt"' "$INPUT_FILE")
    timeout_secs=$(jq -r '.config.timeoutSeconds // 3600' "$INPUT_FILE")
    
    log "Task ID: $task_id"
    log "Prompt: $prompt"
    log "Timeout: ${timeout_secs}s"
    
    # Simulate some work (check for cancellation periodically)
    log "Processing task..."
    sleep 1
    
    # Check for cancellation after work
    check_cancellation
    
    # Write success result
    write_success_result "$task_id" "$prompt" "$(get_duration)"
    
    log "Fake agent completed successfully"
    exit 0
}

# Run main
main "$@"
