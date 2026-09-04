#!/bin/bash
# Start Lightstack in background for CI testing
# Use proper job control to fully detach from shell

set +e  # Don't exit on error during detachment

# Start Lightstack, capture PID immediately, then disown
./bin/lightstack serve </dev/null >/tmp/lightstack.log 2>&1 &
LIGHTSTACK_PID=$!
disown

# Store PID
echo "$LIGHTSTACK_PID" > /tmp/lightstack.pid

# Wait for process to initialize
sleep 3

# Verify process is running
if ! kill -0 "$LIGHTSTACK_PID" 2>/dev/null; then
    echo "❌ Lightstack process died (PID: $LIGHTSTACK_PID)"
    echo "Last 20 lines of log:"
    tail -20 /tmp/lightstack.log 2>/dev/null || echo "No log file"
    exit 1
fi

echo "✅ Lightstack started successfully (PID: $LIGHTSTACK_PID)"
exit 0
