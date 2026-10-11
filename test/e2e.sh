#!/bin/bash
# End-to-end test of the ws and forum clients, the HTTP API and the
# store together, against scratch servers on loopback.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"

# Start from a clean environment, so no live config, workspace,
# timeout, secret or Claude Code session reaches the scratch servers or
# clients.
for name in $(compgen -e); do
    if [[ "$name" == WORKSTREAM_* || "$name" == FORUM_* ||
        "$name" == WORK_STREAM_* ]]; then
        unset "$name"
    fi
done
unset CLAUDE_CODE_SESSION_ID
test_dir="$(mktemp -d)"
server_pids=()
test_secret="quack-test-secret"

# Read the versions from the source rather than mirroring them, so a
# bump does not silently strand this script on the old contract.
version_file="$repo/internal/version/version.go"
api_version="$(sed -nE \
    's/^[[:space:]]*API[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p' \
    -- "$version_file")"
software_version="$(sed -nE \
    's/^[[:space:]]*Software[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' \
    -- "$version_file")"
if [[ -z "$api_version" || -z "$software_version" ]]; then
    echo "could not read the versions from $version_file" >&2
    exit 1
fi

cleanup() {
    for pid in "${server_pids[@]}"; do
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    rm -rf "$test_dir"
}
trap cleanup EXIT

"$repo/scripts/build.sh" >/dev/null
(cd "$repo" && CGO_ENABLED=0 go build -o "$test_dir/bin/forum" \
    -ldflags "-X main.programName=forum -X main.envPrefix=FORUM -X main.defaultConfigPath=~/.config/forum/config.yml" \
    ./cmd/ws)
# Only now, after Go has used the real home for its caches, does the
# home move, so the clients find no real config.
export HOME="$test_dir/home"
mkdir -p "$HOME"

# start_server <name> <workspaces yaml> starts a scratch server on a
# free loopback port, with its log at $test_dir/<name>.log, and sets
# server_url.
start_server() {
    local name="$1" workspaces="$2"
    local config="$test_dir/$name-server.yml" log="$test_dir/$name.log"
    printf 'data: %s\nlisten: 127.0.0.1:0\nworkspaces:\n%s\n' \
        "$test_dir/$name-data" "$workspaces" >"$config"
    chmod 600 "$config"
    "$repo/bin/ws-server" --config "$config" >"$log" 2>&1 &
    server_pids+=("$!")
    local port
    for _ in $(seq 1 50); do
        port="$(sed -nE 's/.*listening on 127\.0\.0\.1:([0-9]+) .*/\1/p' \
            -- "$log")"
        if [[ -n "$port" ]] && curl -fsS \
            -H "Workstream-API-Version: $api_version" \
            "http://127.0.0.1:$port/api/status" >/dev/null 2>&1; then
            server_url="http://127.0.0.1:$port"
            return
        fi
        sleep 0.1
    done
    cat "$log" >&2
    exit 1
}

start_server main "  general:
    prefix: e
  personal:
    prefix: p
    auth:
      secret: $test_secret"
main_url="$server_url"
start_server remote "  remote:
    prefix: r"
remote_url="$server_url"

# No workspace here sets a prefix, so every id is resolved by asking
# the servers in turn.
ws_config="$test_dir/ws.yml"
cat >"$ws_config" <<EOF
workspaces:
  general:
    server: $main_url
  personal:
    server: $main_url
    auth:
      secret: $test_secret
  remote:
    server: $remote_url/
EOF
chmod 600 "$ws_config"
ws() { WORKSTREAM_CONFIG="$ws_config" "$repo/bin/ws" "$@"; }
# Two Claude Code sessions of the user running ws, whose waits each
# leave out their own changes.
lead() { CLAUDE_CODE_SESSION_ID=lead ws "$@"; }
peer() { CLAUDE_CODE_SESSION_ID=peer ws "$@"; }
forum() { "$test_dir/bin/forum" "$@"; }

failures=0
check() {
    local desc="$1" expected="$2" actual="$3"
    if [[ "$actual" == *"$expected"* ]]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"
        echo "  expected to contain: $expected"
        echo "  actual: $actual"
        failures=$((failures + 1))
    fi
}

check_equal() {
    local desc="$1" expected="$2" actual="$3"
    if [[ "$actual" == "$expected" ]]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"
        echo "  expected: $expected"
        echo "  actual: $actual"
        failures=$((failures + 1))
    fi
}

check_status() {
    local desc="$1" expected="$2"
    shift 2
    local actual
    actual="$(curl -sS -o /dev/null -w '%{http_code}' "$@")"
    check "$desc" "$expected" "$actual"
}

added() { sed -n 's/^Added entry \[\([a-z]*[0-9]*\)\]\.$/\1/p'; }
cursor_of() { sed -n 's/^Cursor: \([0-9]*\) .*/\1/p'; }

# Each build reads only its own config, and without one it explains
# how to write one and does nothing else.
check "version is offline" "ws $software_version" "$(ws --version)"
check "forum reports its version under its name" \
    "forum $software_version" "$(forum --version)"
check "ws without a config names its config path" \
    "no config file at $HOME/.config/workstream/config.yml" \
    "$("$repo/bin/ws" recent 2>&1 || true)"
check "forum without a config names its config path" \
    "no config file at $HOME/.config/forum/config.yml, so forum is not set up" \
    "$(WORKSTREAM_CONFIG="$ws_config" forum recent 2>&1 || true)"
check "forum names the config its override gives" \
    "no config file at $test_dir/elsewhere.yml" \
    "$(FORUM_CONFIG="$test_dir/elsewhere.yml" forum recent 2>&1 ||
        true)"
check_equal "ws reads its config and lists the workspaces in order" \
    "general (default): prefix e on $main_url
personal: prefix p on $main_url, with a secret
remote: prefix r on $remote_url" "$(ws list-workspaces)"
check "status checks every workspace" "remote: prefix r on $remote_url" \
    "$(ws status)"

check_status "the API version is required" "400" \
    "$main_url/api/workspaces"
check_status "a protected workspace needs its secret" "401" \
    -H "Workstream-API-Version: $api_version" \
    "$main_url/api/workspaces/personal/entries"
check_status "old routes are absent" "404" \
    -H "Workstream-API-Version: $api_version" "$main_url/api/entries"

# Search filters reach the server as given.
ws add "Find a Stock Image of a Duck" --project Duck-Pond >/dev/null # e1
ws add "Reviewed the duck PR" --project pond \
    --meta pr=https://example.com/pr/7 --jira QUACK-1 >/dev/null # e2
ws add "Handle [duck]? literally" --project pond >/dev/null # e3
check_equal "GLOB reaches the server" "e1" \
    "$(ws search --subject '*stock*' --id-only)"
check_equal "GLOB is ASCII case-insensitive" "e2" \
    "$(ws search --jira 'quack-*' --id-only)"
check_equal "prose filters match part of the value" "e1" \
    "$(ws search Stock --id-only)"
check_equal "whole-value filters do not match a prefix" "" \
    "$(ws search --project pon --id-only)"
check_equal "a wildcard widens a whole-value filter" "e3
e2" "$(ws search --project 'pon*' --id-only)"
check_equal "bracket class matches a literal wildcard" "e3" \
    "$(ws search '[?]' --id-only)"
check_equal "negation reaches the server" "e1" \
    "$(ws search --no-project pond --id-only)"
check_equal "the project filter ignores ASCII case" "e1" \
    "$(ws search --project duck-pond --id-only)"

# Edits.
ws edit e3 --project nest --no-recent >/dev/null
check_equal "a moved entry is found under its new project" "e3" \
    "$(ws search --project nest --id-only)"
check "entry shows the project" "Project: Duck-Pond" "$(ws entry e1)"
ws edit e2 --jira "" --no-recent >/dev/null
check_equal "an empty value removes the metadata" "" \
    "$(ws search --key jira --id-only)"
check "the project can change but not be removed" "not removed" \
    "$(ws edit e1 --project "" 2>&1 || true)"
ws edit e1 --body "The Long Form Detail" --no-recent >/dev/null
check "entry shows the body" "The Long Form Detail" "$(ws entry e1)"
check "a listing hides the body" "0" \
    "$(ws search Stock | grep -c 'Long Form Detail' || true)"
after_add="$(ws add "Fourth duck" --project pond)"
check_equal "a change lists the other recent entries" "e1
e2
e3" "$(echo "$after_add" | sed -n 's/^\[\(e[0-9]*\)\].*/\1/p')"

# Projects.
check "add needs a project" "add needs --project" \
    "$(ws add "No project" 2>&1 || true)"
check "the project is not metadata" "project is not a metadata key" \
    "$(ws add "Meta project" --project pond --meta project=pond \
        2>&1 || true)"
check "the first entry in a project is noticed" \
    'first entry in project "lake-shore"' \
    "$(ws add "Shore duck" --project lake-shore --no-recent)"
check "list-projects counts a project's recent entries" "1 entry" \
    "$(ws list-projects | grep lake-shore)"

# Links: one parent, supersession hides the older entry, no loops.
topic="$(ws add "Topic" --project links --no-recent | added)"
old_note="$(ws add "Old finding" --project links --parent "$topic" \
    --no-recent | added)"
fix_a="$(ws add "Corrected finding" --project links \
    --supersedes "$old_note" --link "see=$topic" --no-recent | added)"
check "the older entry names its successor" "Superseded by $fix_a" \
    "$(ws entry "$old_note")"
check "the target shows who links to it" "$fix_a see this entry" \
    "$(ws entry "$topic")"
check_equal "a superseded entry is hidden by default" "" \
    "$(ws search --project links --subject 'Old finding' --id-only)"
check "a parent loop is refused" "loop" \
    "$(ws edit "$topic" --parent "$old_note" 2>&1 || true)"
ws edit "$fix_a" --unlink see --no-recent >/dev/null
check_equal "unlinking by name removes the link" "" \
    "$(ws entry "$topic" | grep 'see this entry' || true)"

# Views and tree cuts.
root="$(ws add "Count the ducklings" --project views --no-recent |
    added)"
pond_a="$(ws add "Pond A counted" --project views --parent "$root" \
    --no-recent | added)"
recount="$(ws add "Recount needed" --project views \
    --parent "$pond_a" --no-recent | added)"
for index in 1 2 3; do
    ws add "Recount $index" --project views --parent "$recount" \
        --no-recent >/dev/null
done
pond_1="$(ws add "Pond 1" --project views --parent "$root" \
    --no-recent | added)"
for index in 2 3; do
    ws add "Pond $index" --project views --parent "$root" \
        --no-recent >/dev/null
done
check "a top-level entry counts what is below it" \
    "Count the ducklings (+8 below)" "$(ws recent --project views)"
check "search shows each hit's path on its line" \
    " in $root.$pond_a: Recount needed" \
    "$(ws search 'Recount needed' --project views)"
check_equal "a top-level hit shows no path" "[$root]" \
    "$(ws search 'Count the ducklings' --project views | grep -v ' in ' |
        cut -d' ' -f1)"
check_equal "search under a subtree stays inside it" "4" \
    "$(ws search --under "$recount" --id-only | wc -l | tr -d ' ')"
check "the tree indents grandchildren" "    [$recount] " \
    "$(ws tree "$root")"
tree_output="$(ws tree "$root" --limit 3)"
check_equal "a tree cut at its limit marks where" "[$root]
  [$pond_a]
    ... 4 more below $pond_a
  [$pond_1]
  ... 2 more under $root" \
    "$(echo "$tree_output" | sed -E 's/^( *(\[[a-z0-9]*\]|\.\.\. .*)).*/\1/')"
check_equal "tree cut markers carry no command hints" "" \
    "$(echo "$tree_output" | grep -E '\.\.\..*(ws|tree|--)' || true)"
check_equal "times on one day show the clock alone" "" \
    "$(echo "$tree_output" | grep -E '[0-9]{2}/[0-9]{2}/[0-9]{4}' || true)"

# Two workspaces on one server count their entries apart, and an id
# picks its workspace.
private="$(ws add "Private PROTECTEDMARK" --project mine \
    --workspace personal --link "see=$root" --no-recent | added)"
check_equal "the second workspace counts from 1" "p1" "$private"
check "an id picks its workspace" "Private PROTECTEDMARK" \
    "$(ws entry p1)"
check_equal "a workspace lists only its own entries" "p1" \
    "$(ws recent --workspace personal --id-only)"

# A second server: its ids resolve by asking the servers in turn, and a
# link cannot reach across.
remote="$(ws add "Remote entry" --project remote --workspace remote \
    --no-recent | added)"
check_equal "the second server's workspace has its own prefix" "r1" \
    "$remote"
check "an id on the second server resolves" "Remote entry" \
    "$(ws entry r1)"
check "a link cannot join servers" "cannot join entries on different servers" \
    "$(ws edit r1 --link "see=$root" 2>&1 || true)"
check "an unknown prefix names the servers asked" \
    "no configured server has a workspace with prefix x" \
    "$(ws entry x1 2>&1 || true)"
check "a missing entry is reported as missing" "entry not found: e999" \
    "$(ws entry e999 2>&1 || true)"

# An open workspace linked to a protected one shows the protected entry
# as a bare id at most, through every path that crosses workspaces. A
# parent never crosses them, and add takes its parent's workspace.
check "a parent cannot cross workspaces" \
    "a parent must be in the same workspace" \
    "$(ws add "Protected child" --project mine --workspace personal \
        --parent "$root" 2>&1 || true)"
ws add "Protected see PROTECTEDMARK" --project mine \
    --workspace personal --link "see=$root" --no-recent >/dev/null # p2
check "add takes its parent's workspace" "Added entry [p3]." \
    "$(ws add "Protected child" --project mine --parent p2 --no-recent)"
curl -fsS -X POST -H "Workstream-API-Version: $api_version" \
    -H "Authorization: Bearer $test_secret" \
    --data "{\"subject\":\"Open linking protected\",\"project\":\"views\",\"origin\":{},\"links\":[{\"name\":\"see\",\"ref\":\"p1\"}]}" \
    "$main_url/api/workspaces/general/entries" >/dev/null
under_protected="$(ws search 'Open linking protected' --id-only)"
check "a protected link shows as its bare id" "see p1" \
    "$(ws entry "$under_protected")"
check "a protected backlink shows as its bare id" "p1 see this entry" \
    "$(ws entry "$root")"
check "a link to an unreadable entry is refused" "link target not found" \
    "$(ws add "Probe" --project views --link see=p1 2>&1 || true)"
ws chat send p2 "Protected message PROTECTEDMARK" >/dev/null
ws chat send "$root" "Open message" >/dev/null
open_views="$(ws entry "$root"; ws tree "$root"; ws search --under "$root"
    ws recent; ws recent --flat; ws search "$under_protected"
    ws entry "$under_protected"; ws chat recent; ws chat recent "$root"
    ws wait --since 0 --timeout 1s)"
check_equal "no protected content reaches the open workspace" "" \
    "$(echo "$open_views" | grep PROTECTEDMARK || true)"
check_equal "a tree holds no protected entry" "" \
    "$(ws tree "$root" | grep -E '\[p[0-9]+\]' || true)"

# Chat.
chat="$(ws add "Chat thread" --project chat --no-recent | added)"
check_equal "a send prints only the message id" "Sent [$chat-m1]." \
    "$(ws chat send "$chat" "First QUESTIONMARK")"
ws chat send "$chat" "Second" >/dev/null
ws chat send "$chat" "Third" >/dev/null
check "a read pages forward with a footer" \
    "Next: ws chat read $chat --since 1 (2 more messages)" \
    "$(ws chat read "$chat" --since 0 --limit 1)"
check "the last page ends with the cursor" \
    "Cursor: 3 (wait for more: ws chat wait $chat --since 3)" \
    "$(ws chat read "$chat" --since 1)"
check "an empty read keeps the cursor" "No messages after 3." \
    "$(ws chat read "$chat" --since 3)"
check_equal "chat recent shows the newest, oldest first" "[$chat-m2]
[$chat-m3]" "$(ws chat recent "$chat" --limit 2 |
    sed -n 's/^\(\[[a-z0-9-]*\]\).*/\1/p')"
ws chat edit "$chat-m1" "First, edited" >/dev/null
check "an edit stays in place" "[$chat-m1]" \
    "$(ws chat read "$chat" --since 0 --limit 1)"
check_equal "no read returns the old text" "" \
    "$(ws chat read "$chat" --since 0 | grep QUESTIONMARK || true)"
overview="$(ws chat recent)"
check_equal "the overview lists the entry with the newest message last" \
    "[$chat] Chat thread" "$(echo "$overview" | grep '^\[' | tail -1)"
# Read to 1, a peer sends 2, you send 3 with --since 1. The send shows
# the peer's message and yours, and a wait from its cursor waits for
# the next message rather than returning yours.
race="$(ws add "Cursor race" --project chat --no-recent | added)"
ws chat send "$race" "Read first" >/dev/null
ws chat send "$race" "From a peer" >/dev/null
check_equal "a send with --since shows what others sent, then yours" \
    "Sent [$race-m3].
[$race-m2]
[$race-m3]
Cursor: 3 (wait for more: ws chat wait $race --since 3)" \
    "$(ws chat send "$race" "My own" --since 1 |
        sed 's/^\(\[[a-z0-9-]*\]\).*/\1/')"
check "a wait from a send's cursor does not return your own message" \
    "No matching messages after 3 within 1s." \
    "$(ws chat wait "$race" --since 3 --timeout 1s)"
check "a message may start with a dash beside --since" \
    ": -1 to that" "$(ws chat send --since 3 "$race" "-1 to that")"
# With 22 messages after the cursor, the send shows the newest 20 and
# how to read the 2 it left out.
for n in $(seq 21); do ws chat send "$race" "Peer $n" >/dev/null; done
sent="$(ws chat send "$race" "Catching up" --since 4 |
    sed 's/^\(\[[a-z0-9-]*\]\).*/\1/')"
check_equal "a send shows at most the newest 20 messages after --since" \
    "Sent [$race-m26].
... 2 earlier (ws chat read $race --since 4)
[$race-m7]" "$(echo "$sent" | head -3)"
check_equal "a send that left messages out ends with yours and its cursor" \
    "[$race-m26]
Cursor: 26 (wait for more: ws chat wait $race --since 26)" \
    "$(echo "$sent" | tail -2)"
check_equal "a send that left messages out shows 20 of them" 20 \
    "$(echo "$sent" | grep -c '^\[')"
(sleep 1.5 && peer chat send "$chat" "Woken" >/dev/null) &
sender_pid=$!
check "a wait returns when a message arrives" "[$chat-m4]" \
    "$(ws chat wait "$chat" --since 3 --timeout 20s)"
wait "$sender_pid"
check "a wait ends at its timeout" "No matching messages after 4 within 1s." \
    "$(ws chat wait "$chat" --since 4 --timeout 1s)"
check "an entry shows its chat" "Chat: 4 messages, newest $chat-m4" \
    "$(ws entry "$chat")"
check "recent shows a tree's newest messages, not its edited ones" \
    "  ... 2 earlier
  [$chat-m3]" "$(ws recent --project chat)"
check "search shows the matching messages" "  [$chat-m4]" \
    "$(ws search Woken)"

# Only the user who posted an entry or message can edit or delete it,
# and anyone can add under it and chat on it. The client always sends
# its own user, so the other user's posts go in by curl.
post_as_heron() {
    curl -fsS -X POST -H "Workstream-API-Version: $api_version" \
        --data "$2" "$main_url/api/$1" >/dev/null
}
post_as_heron workspaces/general/entries \
    '{"subject":"Heron topic","project":"owners","origin":{"user":"heron"}}'
heron="$(ws search 'Heron topic' --id-only)"
check "an edit of another user's entry names its owner and the next step" \
    "$heron was posted by heron, so only heron can edit or delete it. To suggest a change, send a message in its chat with 'ws chat send $heron \"<message>\"'" \
    "$(ws edit "$heron" --subject "Taken" 2>&1 || true)"
check "a link alone is refused too" "only heron can edit or delete it" \
    "$(ws edit "$heron" --link "see=$chat" 2>&1 || true)"
reply="$(ws add "Reply under heron" --project owners --parent "$heron" \
    --no-recent | added)"
check "anyone can add under another user's entry" "[$reply]" \
    "$(ws tree "$heron")"
check_equal "anyone can chat on another user's entry" "Sent [$heron-m1]." \
    "$(ws chat send "$heron" "Please fix the subject")"
post_as_heron "entries/$heron/messages" \
    '{"body":"Fixed soon","origin":{"user":"heron"}}'
check "an origin filter finds that user's messages" "  [$heron-m2]" \
    "$(ws search --origin-user heron)"
check_equal "a message meets the text and origin filters itself" "" \
    "$(ws search Please --origin-user heron --id-only)"
heron_changes="$(ws wait --since 0 --origin-user heron --include-mine \
    --timeout 1s)"
check "a wait's origin filter matches a message's own origin" \
    "[$heron-m2]" "$heron_changes"
check_equal "a wait's origin filter leaves out others' messages" "" \
    "$(echo "$heron_changes" | grep -F "[$heron-m1]" || true)"
check_equal "a chat wait's origin filter matches each message" \
    "[$heron-m2]" \
    "$(ws chat wait "$heron" --since 0 --origin-user heron --include-mine \
        --timeout 1s | grep -o "^\[$heron-m[0-9]*\]")"
check "a span back from now keeps recent messages" "  [$heron-m2]" \
    "$(ws search --origin-user heron --after 1h)"
check_equal "a date leaves out what changed after it" "No matching entries." \
    "$(ws search --origin-user heron --before 2000-01-01)"
check "recent keeps to trees changed in the span" "Heron topic" \
    "$(ws recent --project owners --after 10m)"
check_equal "recent leaves out trees with no change in the span" \
    "No matching entries." "$(ws recent --project owners --before 2000-01-01)"
check "a time flag names the forms it takes" "like 2d, 6h or 90m" \
    "$(ws search --after yesterday 2>&1 || true)"
check "an edit of another user's message is refused" \
    "$heron-m2 was posted by heron, so only heron can edit or delete it" \
    "$(ws chat edit "$heron-m2" "Taken" 2>&1 || true)"
check "a delete of another user's entry is refused" \
    "only heron can edit or delete it" "$(ws delete "$heron" 2>&1 || true)"
check "a delete of another user's message is refused" \
    "only heron can edit or delete it" \
    "$(ws chat delete "$heron-m2" 2>&1 || true)"

# A delete removes an entry's text and metadata, or a message's text,
# and keeps its place.
gadwall="$(ws add "Gadwall nest" --project owners --body "Code 4417" \
    --jira QUACK-9 --parent "$reply" --no-recent | added)"
egg="$(ws add "Eggs counted" --project owners --parent "$gadwall" \
    --no-recent | added)"
ws chat send "$gadwall" "Before" >/dev/null
ws chat send "$gadwall" "The code is 4417" >/dev/null
ws chat send "$gadwall" "After" >/dev/null
before_delete="$(ws wait --since 0 --include-mine --timeout 1s | cursor_of)"
check_equal "you can delete your own entry" "Deleted entry [$gadwall]." \
    "$(ws delete "$gadwall")"
check_equal "you can delete your own message" "Deleted [$gadwall-m2]." \
    "$(ws chat delete "$gadwall-m2")"
gadwall_entry="$(ws entry "$gadwall")"
check "a deleted entry shows as deleted" "[$gadwall] (deleted)" \
    "$gadwall_entry"
check "a deleted entry shows when it was deleted" "Deleted: " \
    "$gadwall_entry"
check_equal "a deleted entry keeps no body or metadata" "" \
    "$(echo "$gadwall_entry" |
        grep -E 'Gadwall|4417|QUACK|Metadata|Modified' || true)"
check "a deleted entry keeps its origin" "From: $(id -un)@" "$gadwall_entry"
check "a child's path shows its deleted parent" "> $gadwall (deleted)" \
    "$(ws entry "$egg")"
untimed() { sed -E 's/\] ([0-9/]+ )?[0-9]{2}:[0-9]{2}:? /] /'; }
check "a deleted entry keeps its place in the tree" \
    "  [$gadwall] (deleted)
    [$egg] Eggs counted" "$(ws tree "$reply" | untimed)"
check "a listing shows a deleted entry as deleted" \
    "[$gadwall] in $heron.$reply: (deleted)" \
    "$(ws recent --flat --project owners | untimed)"
check "a deleted message keeps its place and shows no text" \
    "[$gadwall-m1] $(id -un): Before
[$gadwall-m2] $(id -un) (deleted)
[$gadwall-m3] $(id -un): After" "$(ws chat recent "$gadwall" | untimed)"
check "the chat overview names a deleted entry as deleted" \
    "[$gadwall] (deleted)" "$(ws chat recent)"
check_equal "search no longer finds deleted text" "No matching entries." \
    "$(ws search 4417)"
deletes="$(ws wait --since "$before_delete" --include-mine --timeout 1s |
    untimed)"
check "a wait shows the deleted entry and message" \
    "[$gadwall] in $heron.$reply: (deleted)
  [$gadwall-m2] $(id -un) (deleted)" "$deletes"
after_delete="$(echo "$deletes" | cursor_of)"
check "an edit of a deleted entry is refused" \
    "$gadwall was deleted, so it cannot be edited" \
    "$(ws edit "$gadwall" "Gadwall nest" 2>&1 || true)"
check "a link edit of a deleted entry is refused" \
    "$gadwall was deleted, so it cannot be edited" \
    "$(ws edit "$gadwall" --link see="$egg" 2>&1 || true)"
check "an edit of a deleted message is refused" \
    "$gadwall-m2 was deleted, so it cannot be edited" \
    "$(ws chat edit "$gadwall-m2" "The code is 4417" 2>&1 || true)"
check_equal "deleting again succeeds" "Deleted entry [$gadwall].
Deleted [$gadwall-m2]." \
    "$(ws delete "$gadwall" && ws chat delete "$gadwall-m2")"
check "deleting again changes nothing" "No matching changes" \
    "$(ws wait --since "$after_delete" --include-mine --timeout 1s)"

# Changes.
private_changes="$(ws wait --since 0 --workspace personal --include-mine \
    --timeout 1s)"
check "wait lists a workspace's changes from the start" "[p2-m1]" \
    "$private_changes"
check "wait names the workspace to wait on in" \
    "--workspace personal --include-mine" \
    "$private_changes"
private_cursor="$(echo "$private_changes" |
    sed -n 's/^Cursor: \([0-9]*\) .*/\1/p')"
check "a wait from the last cursor finds nothing new" \
    "No matching changes after $private_cursor" \
    "$(ws wait --since "$private_cursor" --workspace personal --timeout 1s)"
(sleep 1.5 && peer chat send "$chat" "Changed" >/dev/null) &
sender_pid=$!
check "a wait from now returns when anything changes" "[$chat-m5]" \
    "$(ws wait --timeout 20s)"
wait "$sender_pid"
ws firehose --timeout 5s >"$test_dir/firehose.out" &
firehose_pid=$!
for _ in $(seq 1 50); do
    if [[ -s "$test_dir/firehose.out" ]]; then
        break
    fi
    sleep 0.1
done
if [[ ! -s "$test_dir/firehose.out" ]]; then
    echo "the firehose printed nothing within 5s" >&2
    exit 1
fi
ws chat send "$chat" "Hosed" >/dev/null
ws add "Hosed entry" --project chat --no-recent >/dev/null
wait "$firehose_pid"
firehose_output="$(cat -- "$test_dir/firehose.out")"
check "a firehose prints messages as they arrive" "[$chat-m6]" \
    "$firehose_output"
check "a firehose prints entries as they arrive" "Hosed entry" \
    "$firehose_output"

# A wait leaves out the changes of its own session, and wakes for
# another session of the same user. Its cursor passes what it left out.
start="$(lead wait --timeout 1ms | cursor_of)"
own="$(lead add "Own changes" --project own --no-recent | added)"
lead chat send "$own" "Lead message" >/dev/null
mine="$(lead wait --since "$start" --timeout 1s)"
check "a wait sleeps through your own session's changes" \
    "No matching changes after $start within 1s." "$mine"
mine_cursor="$(echo "$mine" | cursor_of)"
check "a wait's cursor passes the changes it left out" \
    "No matching changes after $mine_cursor" \
    "$(lead wait --since "$mine_cursor" --include-mine --timeout 1s)"
others="$(peer wait --since "$start" --timeout 1s)"
check "another session of your user sees your entries" "Own changes" \
    "$others"
check "another session of your user sees your messages" "[$own-m1]" \
    "$others"
check "--include-mine shows your own changes" "[$own-m1]" \
    "$(lead wait --since "$start" --include-mine --timeout 1s)"
ws chat send "$own" "Bare message" >/dev/null
check "outside a session, your sessionless changes are yours" \
    "No matching changes after $mine_cursor" \
    "$(ws wait --since "$mine_cursor" --timeout 1s)"
check "a session wakes for your sessionless changes" "[$own-m2]" \
    "$(lead wait --since "$mine_cursor" --timeout 1s)"
filtered="$(peer wait --since "$start" --project own --content 'lead MESSAGE' \
    --timeout 1s)"
check "a wait's filters match a message by its own text" "[$own-m1]" \
    "$filtered"
# An entry's own change lists it with its time, and a message's change
# names it above the message without one.
own_entry_line="^\[$own\] [0-9]"
check_equal "a wait's filters leave out what does not match" "" \
    "$(echo "$filtered" | grep -e "$own_entry_line" -e "\[$own-m2\]" ||
        true)"
check "a suggested wait repeats the filters" \
    "--content 'lead MESSAGE' --project own)" "$filtered"
check_equal "a firehose with --exclude-mine leaves out your own" "" \
    "$(lead firehose --since "$start" --exclude-mine --timeout 1s |
        grep -e "$own_entry_line" -e "\[$own-m1\]" || true)"
check "a firehose with filters shows what matches" "[$own-m2]" \
    "$(ws firehose --since "$start" --content bare --timeout 1s)"
lonely="$(lead chat wait "$own" --since 0 --timeout 1s)"
check_equal "a chat wait shows only messages that are not yours" \
    "[$own-m2]
Cursor: 2 (wait for more: ws chat wait $own --since 2)" \
    "$(echo "$lonely" | sed 's/^\(\[[a-z0-9-]*\]\).*/\1/')"
(sleep 1.5 && peer chat send "$own" "Answered" >/dev/null) &
sender_pid=$!
lead chat send "$own" "Asked again" >/dev/null
answered="$(lead chat wait "$own" --since 2 --timeout 20s)"
wait "$sender_pid"
check "a chat wait sleeps through your message to the reply" "[$own-m4]" \
    "$answered"
check_equal "a chat wait prints none of your own messages" "" \
    "$(echo "$answered" | grep -F "[$own-m3]" || true)"
check "a chat wait with --include-mine shows your own messages" \
    "[$own-m3]" \
    "$(lead chat wait "$own" --since 2 --include-mine --timeout 1s)"

# The server log keeps no client text, and records each issued id.
ws add "Subject OLDSUBJECTMARK" --body "Body OLDBODYMARK" \
    --project pond --no-recent >/dev/null
ws search SEARCHMARK >/dev/null
check_status "an unknown field is refused" "400" \
    -X POST -H "Workstream-API-Version: $api_version" \
    --data '{"subject":"note","UNKNOWNFIELDMARK":1}' \
    "$main_url/api/workspaces/general/entries"
check_equal "the server log holds no client text" "" \
    "$(grep -oE '[A-Z]*MARK' -- "$test_dir/main.log" || true)"
check "the server log records each issued entry id" "issued $chat" \
    "$(cat -- "$test_dir/main.log")"
check "the server log records each issued message id" "issued $chat-m4" \
    "$(cat -- "$test_dir/main.log")"

# forum reads its own config, records what that config says, and
# names itself in every command it suggests.
mkdir -p "$HOME/.config/forum" "$test_dir/private-project"
cat >"$HOME/.config/forum/config.yml" <<EOF
workspaces:
  general:
    server: $main_url
    metadata_capture: none
EOF
forum_entry="$(cd "$test_dir/private-project" &&
    CLAUDE_CODE_SESSION_ID=private-session-id \
        forum add "Forum question" --project chat --no-recent | added)"
check_equal "forum records the user alone" \
    "\"origin\":{\"user\":\"$(id -un)\"}" \
    "$(curl -fsS -H "Workstream-API-Version: $api_version" \
        "$main_url/api/entries/$forum_entry" | grep -o '"origin":{[^}]*}')"
forum chat send "$forum_entry" "Forum reply" >/dev/null
check_equal "the private context never reached the server" "" \
    "$(curl -fsS -H "Workstream-API-Version: $api_version" \
        "$main_url/api/entries/$forum_entry/messages?since=0" |
        grep -oE 'private-[a-z-]*|"host"|"dir"' || true)"
check "forum help names forum" "  forum add <subject>" \
    "$(forum help)"
check_equal "forum help never suggests ws" "" \
    "$(forum help | grep -E "(^|[ '])ws " || true)"
check "forum chat continues with forum" \
    "Cursor: 1 (wait for more: forum chat wait $forum_entry --since 1)" \
    "$(forum chat recent "$forum_entry")"
check "forum entry points at forum chat" \
    "see 'forum chat recent $forum_entry'" "$(forum entry "$forum_entry")"
check "a forum usage error names forum" "usage: forum entry <ref>" \
    "$(forum entry 2>&1 || true)"
check "forum knows only its own workspaces" \
    "no configured server has a workspace with prefix r" \
    "$(forum entry r1 2>&1 || true)"
cat >"$test_dir/forum-down.yml" <<EOF
workspaces:
  general:
    server: http://127.0.0.1:1
EOF
check "an unreachable server is named" "request to http://127.0.0.1:1 failed" \
    "$(FORUM_CONFIG="$test_dir/forum-down.yml" \
        forum add "Lost" --project chat 2>&1 || true)"

echo
if [[ "$failures" -gt 0 ]]; then
    echo "$failures FAILURES"
    exit 1
fi
echo "All e2e tests passed."
