#!/bin/bash

# SSH connectivity test script
set -e

PORT=2223
HOST="localhost"
USER="clusteros"
PASS="clusteros"

echo "SSH Connectivity Test"
echo "===================="
echo ""

# Check if port is listening
echo "1. Checking if port $PORT is listening..."
if nc -z -w2 $HOST $PORT 2>/dev/null; then
    echo "   ✓ Port $PORT is accepting connections"
else
    echo "   ✗ Port $PORT is NOT accepting connections"
    exit 1
fi

echo ""
echo "2. Testing SSH banner..."
if timeout 3 bash -c "cat < /dev/null > /dev/tcp/$HOST/$PORT" 2>/dev/null; then
    echo "   ✓ TCP connection established"
else
    echo "   ✗ TCP connection failed"
    exit 1
fi

echo ""
echo "3. Testing SSH authentication..."
if sshpass -p "$PASS" ssh -p $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 $USER@$HOST "echo OK" 2>/dev/null | grep -q OK; then
    echo "   ✓ SSH authentication successful!"
    echo ""
    echo "SSH is working correctly on port $PORT"
    exit 0
else
    echo "   ✗ SSH authentication or command failed"
    echo ""
    echo "Trying with password auth disabled..."
    ssh -p $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -vvv $USER@$HOST "echo test" 2>&1 | head -20
    exit 1
fi
