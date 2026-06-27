# Fake Agent Image

A minimal container image that simulates an agent by reading input and writing output according to the [vornik runtime contract](../../https://docs.vornik.io).

## Purpose

This image is used for testing the runtime manager's first vertical slice. It:

- Reads `/app/input/task.json`
- Writes `/app/output/result.json`
- Creates a dummy output artifact
- Handles cancellation via `/app/input/CANCEL` marker file

## Building

```bash
# From the vornik source root
cd src/vornik/images/fake-agent

# Build with podman
podman build -t fake-agent:latest .
```

## Testing

### Manual Test

```bash
# Create test input
mkdir -p /tmp/fake-agent-test/app/{input,output,workspace}

cat > /tmp/fake-agent-test/app/input/task.json <<'EOF'
{
  "taskId": "test-task-001",
  "projectId": "test-project",
  "swarm": {
    "swarmId": "test-swarm",
    "role": "coder"
  },
  "workflow": {
    "workflowId": "test-workflow",
    "stepId": "test-step",
    "executionId": "test-exec-001"
  },
  "context": {
    "prompt": "Test prompt for fake agent"
  },
  "config": {
    "timeoutSeconds": 3600,
    "permissions": {
      "delegationAllowed": false,
      "allowedTools": ["file_read", "file_write"]
    }
  },
  "correlation": {
    "traceId": "test-trace",
    "spanId": "test-span"
  }
}
EOF

# Run the container
podman run --rm \
  -v /tmp/fake-agent-test/vornik:/vornik \
  fake-agent:latest

# Check the output
cat /tmp/fake-agent-test/app/output/result.json
```

### Expected Output

On success, `result.json` will contain:

```json
{
  "status": "COMPLETED",
  "message": "Fake agent successfully processed task: test-task-001",
  "outputArtifacts": [
    {
      "name": "result.txt",
      "path": "/app/workspace/artifacts/out/result.txt"
    }
  ],
  "delegatedTasks": [],
  "diagnostics": {
    "exitCode": 0,
    "durationSeconds": 1
  }
}
```

### Testing Cancellation

```bash
# Create a CANCEL file during execution
podman run --rm \
  -v /tmp/fake-agent-test/vornik:/vornik \
  fake-agent:latest &

# Wait a moment, then create cancel marker
sleep 0.5
touch /tmp/fake-agent-test/app/input/CANCEL

# Wait for container to finish
wait

# Check output shows CANCELLED status
cat /tmp/fake-agent-test/app/output/result.json
```

### Testing Missing Input

```bash
# Remove the input file
rm /tmp/fake-agent-test/app/input/task.json

# Run - should exit with code 1 and write FAILED status
podman run --rm \
  -v /tmp/fake-agent-test/vornik:/vornik \
  fake-agent:latest

echo "Exit code: $?"
```

## Image Size

The image is based on Alpine 3.19 for minimal footprint:

```bash
podman images fake-agent:latest
```

Expected size: ~10-15 MB

## Contract Compliance

| Requirement | Status |
|-------------|--------|
| Reads `/app/input/task.json` | ✓ |
| Writes `/app/output/result.json` | ✓ |
| Exit code 0 on success | ✓ |
| Exit code non-zero on failure | ✓ |
| Handles `/app/input/CANCEL` | ✓ |
| Minimal footprint (alpine base) | ✓ |
| Buildable with `podman build` | ✓ |