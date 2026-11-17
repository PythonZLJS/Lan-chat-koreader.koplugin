#!/bin/sh
set -eu
umask 077
CHAT_PLUGIN=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CHAT_BINARY="$CHAT_PLUGIN/bin/neighbour-chat"
CHAT_DATA="$CHAT_PLUGIN/data"
CHAT_LOG="$CHAT_DATA/NeighbourChat.log"
CHAT_LOCK="$CHAT_DATA/.reader-start.lock"
if [ -L "$CHAT_DATA" ] || [ -L "$CHAT_BINARY" ] || [ -L "$CHAT_LOG" ]; then
    exit 1
fi
mkdir -p "$CHAT_DATA"
chmod 700 "$CHAT_DATA"
exec >> "$CHAT_LOG" 2>&1
if [ ! -f "$CHAT_BINARY" ] || [ ! -f "$CHAT_PLUGIN/client.lua" ]; then
    echo "Chat package is incomplete. Reopen KOReader to run repair."
    exit 1
fi
if ! mkdir "$CHAT_LOCK" 2>/dev/null; then exit 0; fi
trap 'rmdir "$CHAT_LOCK"' EXIT
chmod +x "$CHAT_BINARY"
if "$CHAT_BINARY" health >/dev/null 2>&1; then exit 0; fi
nohup "$CHAT_BINARY" serve --root "$CHAT_PLUGIN" < /dev/null &
CHAT_TRIES=0
while ! "$CHAT_BINARY" health >/dev/null 2>&1; do
    CHAT_TRIES=$((CHAT_TRIES + 1))
    if [ "$CHAT_TRIES" -ge 40 ]; then echo "Chat did not start. Restart Kindle and retry."; exit 1; fi
    sleep 1
done
