#!/bin/sh
set -eu
umask 077
exec >> /mnt/us/NeighbourChat.log 2>&1
CHAT_PLUGIN=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CHAT_BINARY="$CHAT_PLUGIN/bin/neighbour-chat"
CHAT_LOCK=/tmp/neighbour-chat-reader-start.lock
if ! mkdir "$CHAT_LOCK" 2>/dev/null; then exit 0; fi
trap 'rmdir "$CHAT_LOCK"' EXIT
chmod +x "$CHAT_BINARY"
if "$CHAT_BINARY" health >/dev/null 2>&1; then exit 0; fi
nohup "$CHAT_BINARY" serve --root /mnt/us/lan_chat < /dev/null &
CHAT_TRIES=0
while ! "$CHAT_BINARY" health >/dev/null 2>&1; do
    CHAT_TRIES=$((CHAT_TRIES + 1))
    if [ "$CHAT_TRIES" -ge 40 ]; then echo "Chat did not start. Restart Kindle and retry."; exit 1; fi
    sleep 1
done
