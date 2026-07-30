#!/bin/bash
# Helm plugin entry point

# DEBUG: Log the arguments received
echo "DEBUG: Received arguments: $@" > ~/.helm-tui-debug.log

# Helm passes the command as the first argument
COMMAND=$1
shift

# Find the directory where the plugin is installed
PLUGIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Path to the compiled binary
BINARY_PATH="$PLUGIN_DIR/bin/helm-tui"

if [ "$COMMAND" == "tui" ]; then
    if [ ! -f "$BINARY_PATH" ]; then
        echo "Error: Binary not found at $BINARY_PATH"
        exit 1
    fi
    exec "$BINARY_PATH" "$@"
else
    echo "no plugin command is applicable"
    exit 1
fi 
