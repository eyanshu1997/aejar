#!/bin/sh
# Starts Claude Code in tmux window "main" (attach with: aejar attach <task>)
# and keeps the container alive while the session runs.
set -eu

tmux new-session -d -s main -x 220 -y 50 "node /opt/aejar/bin/launch.mjs; echo; echo '[aejar] claude exited'; sleep 3"

# Unattended start: answer first-run confirmations that settings could not
# pre-accept. Each answer is logged so a wording change shows up in
# `docker logs`. Patterns are pinned to the Claude Code version in the image.
answer() {
    pattern="$1"
    screen=$(tmux capture-pane -p -t main 2>/dev/null || true)
    case "$screen" in
        *"$pattern"*)
            # Pick the numbered option on the matching line, else press Enter.
            n=$(printf '%s\n' "$screen" | grep -F "$pattern" | grep -oE '[0-9]+\.' | head -1 | tr -d .)
            if [ -n "$n" ]; then tmux send-keys -t main "$n"; else tmux send-keys -t main Enter; fi
            echo "[aejar] answered startup prompt: $pattern" >&2
            return 0 ;;
    esac
    return 1
}
report() {
    # $1: channel-ready | channel-unavailable, $2: what was seen
    printf '{"message":"%s"}' "$2" | node /opt/aejar/bin/hook-post.mjs "$1" >/dev/null
    echo "[aejar] $1: $2" >&2
}

# Only Claude Code knows whether it registered the channel, and it says so
# on its startup screen. Report what the screen shows so aejard either
# delivers through the channel or types into the terminal instead.
reported=""
i=0
while [ $i -lt 90 ] && tmux has-session -t main 2>/dev/null; do
    answer "I am using this for local development" || true
    answer "Yes, I accept" || true
    answer "Yes, I trust this folder" || true
    if [ -z "$reported" ]; then
        screen=$(tmux capture-pane -p -t main 2>/dev/null || true)
        case "$screen" in
            *"Channels are not currently available"*|*"development-channels ignored"*)
                report channel-unavailable "claude did not register the channel"; reported=1 ;;
            *"bypass permissions"*|*"? for shortcuts"*)
                # The prompt UI is up; a refusal would already be on screen.
                if [ -S /run/aejar/channel.sock ]; then
                    sleep 2
                    case "$(tmux capture-pane -p -t main 2>/dev/null || true)" in
                        *"Channels are not currently available"*|*"development-channels ignored"*)
                            report channel-unavailable "claude did not register the channel" ;;
                        *) report channel-ready "channel registered" ;;
                    esac
                    reported=1
                fi ;;
        esac
    fi
    i=$((i + 1))
    sleep 1
done
[ -n "$reported" ] || report channel-unavailable "no channel status within 90s"

while tmux has-session -t main 2>/dev/null; do
    sleep 5
done
