#!/usr/bin/env bash
# notify-waiting.sh — desktop notification + sound when Claude Code needs input.
MSG="${*:-Claude Code is waiting for your input}"
notify-send -u critical -a "Claude Code" "🤖 Claude Code" "$MSG" 2>/dev/null || true
for s in /usr/share/sounds/freedesktop/stereo/message.oga /usr/share/sounds/freedesktop/stereo/complete.oga; do
  [ -f "$s" ] && { paplay "$s" 2>/dev/null && break; }
done
canberra-gtk-play -i message 2>/dev/null || true
