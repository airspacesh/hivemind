#!/bin/bash
# End-to-end test of the hivemind and forum clients, the HTTP API and the
# store together, against scratch servers on loopback.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"

# Start from a clean environment, so no live config, workspace,
# timeout, secret or Claude Code session reaches the scratch servers or
# clients.
for name in $(compgen -e); do
    if [[ "$name" == HIVEMIND_* || "$name" == FORUM_* ]]; then
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
    ./cmd/hivemind)
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
    printf 'db: %s\nlisten: 127.0.0.1:0\nworkspaces:\n%s\n' \
        "$test_dir/$name-data/hivemind.db" "$workspaces" >"$config"
    chmod 600 "$config"
    "$repo/bin/hivemind-server" --config "$config" >"$log" 2>&1 &
    server_pids+=("$!")
    local port
    for _ in $(seq 1 50); do
        port="$(sed -nE 's/.*listening on 127\.0\.0\.1:([0-9]+) .*/\1/p' \
            -- "$log")"
        if [[ -n "$port" ]] && curl -fsS \
            -H "Hivemind-API-Version: $api_version" \
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
hivemind_config="$test_dir/hivemind.yml"
cat >"$hivemind_config" <<EOF
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
chmod 600 "$hivemind_config"
hivemind() { HIVEMIND_CONFIG="$hivemind_config" "$repo/bin/hivemind" "$@"; }
# Two Claude Code sessions of the user running hivemind, whose waits each
# leave out their own changes.
lead() { CLAUDE_CODE_SESSION_ID=lead hivemind "$@"; }
peer() { CLAUDE_CODE_SESSION_ID=peer hivemind "$@"; }
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

added() { sed -n 's/^Added post \[\([a-z]*[0-9]*\)\]\.$/\1/p'; }
cursor_of() { sed -n 's/^Cursor: \(@[0-9]*\)$/\1/p'; }
# untimed drops the times from listings, which differ from run to run,
# and writes an exact time as T and a revision as @R.
untimed() {
    sed -E 's# [0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}##g
        s#[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z#T#g
        s#@[0-9]+#@R#g'
}

# Each build reads only its own config, and without one it explains
# how to write one and does nothing else.
check "version is offline" "hivemind $software_version" "$(hivemind --version)"
check "forum reports its version under its name" \
    "forum $software_version" "$(forum --version)"
check "hivemind without a config names its config path" \
    "no config file at $HOME/.config/hivemind/config.yml" \
    "$("$repo/bin/hivemind" recent 2>&1 || true)"
check "forum without a config names its config path" \
    "no config file at $HOME/.config/forum/config.yml, so forum is not set up" \
    "$(HIVEMIND_CONFIG="$hivemind_config" forum recent 2>&1 || true)"
check "forum names the config its override gives" \
    "no config file at $test_dir/elsewhere.yml" \
    "$(FORUM_CONFIG="$test_dir/elsewhere.yml" forum recent 2>&1 ||
        true)"
check_equal "hivemind reads its config and lists the workspaces in order" \
    "general (default): prefix e on $main_url
personal: prefix p on $main_url, with a secret
remote: prefix r on $remote_url" "$(hivemind list-workspaces)"
check "status checks every workspace" "remote: prefix r on $remote_url" \
    "$(hivemind status)"

check_status "the API version is required" "400" \
    "$main_url/api/workspaces"
check_status "a protected workspace needs its secret" "401" \
    -H "Hivemind-API-Version: $api_version" \
    "$main_url/api/workspaces/personal/posts"
check_status "old routes are absent" "404" \
    -H "Hivemind-API-Version: $api_version" "$main_url/api/posts"

# Search filters reach the server as given.
hivemind add "Find a Stock Image of a Duck" --project Duck-Pond >/dev/null # e1
hivemind add "Reviewed the duck PR" --project pond \
    --meta pr=https://example.com/pr/7 --jira QUACK-1 >/dev/null # e2
hivemind add "Handle [duck]? literally" --project pond >/dev/null # e3
check_equal "GLOB reaches the server" "e1" \
    "$(hivemind search --subject '*stock*' --id-only)"
check_equal "GLOB is ASCII case-insensitive" "e2" \
    "$(hivemind search --jira 'quack-*' --id-only)"
check_equal "prose filters match part of the value" "e1" \
    "$(hivemind search Stock --id-only)"
check_equal "whole-value filters do not match a prefix" "" \
    "$(hivemind search --project pon --id-only)"
check_equal "a wildcard widens a whole-value filter" "e3
e2" "$(hivemind search --project 'pon*' --id-only)"
check_equal "bracket class matches a literal wildcard" "e3" \
    "$(hivemind search '[?]' --id-only)"
check_equal "negation reaches the server" "e1" \
    "$(hivemind search --no-project pond --id-only)"
check_equal "the project filter ignores ASCII case" "e1" \
    "$(hivemind search --project duck-pond --id-only)"

# Edits.
hivemind edit e3 --project nest >/dev/null
check_equal "a moved post is found under its new project" "e3" \
    "$(hivemind search --project nest --id-only)"
check "post shows the project" "Project: Duck-Pond" "$(hivemind post e1)"
hivemind edit e2 --jira "" >/dev/null
check_equal "an empty value removes the metadata" "" \
    "$(hivemind search --key jira --id-only)"
check "the project can change but not be removed" "not removed" \
    "$(hivemind edit e1 --project "" 2>&1 || true)"
hivemind edit e1 --body "The Long Form Detail" >/dev/null
check "post shows the body" "The Long Form Detail" "$(hivemind post e1)"
check "a listing hides the body" "0" \
    "$(hivemind search Stock | grep -c 'Long Form Detail' || true)"
check_equal "an add prints only what it added" "Added post [e4]." \
    "$(hivemind add "Fourth duck" --project pond)"

# Projects.
check "add needs a project" "add needs --project" \
    "$(hivemind add "No project" 2>&1 || true)"
check "the project is not metadata" "project is not a metadata key" \
    "$(hivemind add "Meta project" --project pond --meta project=pond \
        2>&1 || true)"
check "the first post in a project is noticed" \
    'first post in project "lake-shore"' \
    "$(hivemind add "Shore duck" --project lake-shore)"
check "list-projects counts a project's recent posts" "1 post" \
    "$(hivemind list-projects | grep lake-shore)"

# Links: one parent, supersession hides the older post, no loops.
topic="$(hivemind add "Topic" --project links | added)"
old_note="$(hivemind add "Old finding" --project links --parent "$topic" \
    | added)"
fix_a="$(hivemind add "Corrected finding" --project links \
    --supersedes "$old_note" --link "see=$topic" | added)"
check "the older post names its successor" "(superseded by $fix_a)" \
    "$(hivemind post "$old_note")"
check "the target shows who links to it" "$fix_a see this post" \
    "$(hivemind post "$topic")"
check_equal "a superseded post is hidden by default" "" \
    "$(hivemind search --project links --subject 'Old finding' --id-only)"
check "a parent loop is refused" "loop" \
    "$(hivemind edit "$topic" --parent "$old_note" 2>&1 || true)"
hivemind edit "$fix_a" --unlink see >/dev/null
check_equal "unlinking by name removes the link" "" \
    "$(hivemind post "$topic" | grep 'see this post' || true)"

# Views and tree cuts.
root="$(hivemind add "Count the ducklings" --project views |
    added)"
pond_a="$(hivemind add "Pond A counted" --project views --parent "$root" \
    | added)"
recount="$(hivemind add "Recount needed" --project views \
    --parent "$pond_a" | added)"
for index in 1 2 3; do
    hivemind add "Recount $index" --project views --parent "$recount" \
        >/dev/null
done
pond_1="$(hivemind add "Pond 1" --project views --parent "$root" \
    | added)"
for index in 2 3; do
    hivemind add "Pond $index" --project views --parent "$root" \
        >/dev/null
done
pond_n="${pond_1#e}"
check_equal "recent shows the newest posts with their paths" \
    "[e$((pond_n + 2))] $(id -un) in $root: Pond 3
[e$((pond_n + 1))] $(id -un) in $root: Pond 2
[$pond_1] $(id -un) in $root: Pond 1
Next: hivemind recent --before @R --workspace general --limit 3 --project views (more older matches)
Cursor: @R" \
    "$(hivemind recent --project views -n 3 | untimed)"
check_equal "recent says on stderr what it ran" \
    "Newest 3 posts matching --project views:" \
    "$(hivemind recent --project views -n 3 2>&1 >/dev/null)"
check "recent --id-only gives the next page on stderr" \
    "Next: hivemind recent --before @" \
    "$(hivemind recent --project views -n 3 --id-only 2>&1 >/dev/null)"
check "recent --id-only gives the cursor on stderr" "Cursor: @" \
    "$(hivemind recent --project views -n 3 --id-only 2>&1 >/dev/null)"
check_equal "search ends with no cursor" "" \
    "$(hivemind search Pond --project views 2>&1 | grep '^Cursor: ' || true)"
older="$(hivemind recent --project views -n 3 | grep '^Next: ' |
    sed -E 's/^Next: (.*) \(more older matches\)$/\1/')"
check_equal "the footer's command lists the next page" \
    "[e$((${recount#e} + 3))] $(id -un) in $root.$pond_a.$recount: Recount 3
[e$((${recount#e} + 2))] $(id -un) in $root.$pond_a.$recount: Recount 2
[e$((${recount#e} + 1))] $(id -un) in $root.$pond_a.$recount: Recount 1" \
    "$(eval "$older" | untimed | head -3)"
check_equal "recent under a subtree stays inside it" "4" \
    "$(hivemind recent --under "$recount" --id-only | wc -l | tr -d ' ')"
check "search shows each hit's path on its line" \
    " in $root.$pond_a: Recount needed" \
    "$(hivemind search 'Recount needed' --project views)"
check_equal "a top-level hit shows no path" "[$root]" \
    "$(hivemind search 'Count the ducklings' --project views | grep '^\[' |
        grep -v ' in ' | cut -d' ' -f1)"
check_equal "search under a subtree stays inside it" "4" \
    "$(hivemind search --under "$recount" --id-only | wc -l | tr -d ' ')"
check_equal "recent not under a subtree leaves it out" \
    "$(hivemind recent --under "$root" -n 100 --id-only | grep -vxF -f <(
        hivemind recent --under "$recount" --id-only))" \
    "$(hivemind recent --under "$root" --no-under "$recount" -n 100 --id-only)"
check "the tree indents grandchildren" "    [$recount] " \
    "$(hivemind tree "$root")"
tree_output="$(hivemind tree "$root" --limit 3)"
check_equal "a tree cut at its limit marks where" "[$root]
  [$pond_a]
    ... 4 more below $pond_a
  [$pond_1]
  ... 2 more under $root" \
    "$(echo "$tree_output" | sed -E 's/^( *(\[[a-z0-9]*\]|\.\.\. .*)).*/\1/')"
check_equal "-n is short for --limit" "$tree_output" \
    "$(hivemind tree "$root" -n 3)"
check_equal "tree cut markers carry no command hints" "" \
    "$(echo "$tree_output" | grep -E '\.\.\..*(hivemind|tree|--)' || true)"

# Two workspaces on one server count their posts apart, and an id picks
# its workspace.
private="$(hivemind add "Private PROTECTEDMARK" --project mine \
    --workspace personal --link "see=$root" | added)"
check_equal "the second workspace counts from 1" "p1" "$private"
check "an id picks its workspace" "Private PROTECTEDMARK" \
    "$(hivemind post p1)"
check_equal "a workspace lists only its own posts" "p1" \
    "$(hivemind recent --workspace personal --id-only)"

# A second server: its ids resolve by asking the servers in turn, and a
# link cannot reach across.
remote="$(hivemind add "Remote post" --project remote --workspace remote \
    | added)"
check_equal "the second server's workspace has its own prefix" "r1" \
    "$remote"
check "an id on the second server resolves" "Remote post" \
    "$(hivemind post r1)"
check "a link cannot join servers" "cannot join posts on different servers" \
    "$(hivemind edit r1 --link "see=$root" 2>&1 || true)"
check "an unknown prefix names the servers asked" \
    "no configured server has a workspace with prefix x" \
    "$(hivemind post x1 2>&1 || true)"
check "a missing post is reported as missing" "post not found: e999" \
    "$(hivemind post e999 2>&1 || true)"

# An open workspace linked to a protected one shows the protected post
# as a bare id at most, through every path that crosses workspaces. A
# parent never crosses them, and add takes its parent's workspace.
check "a parent cannot cross workspaces" \
    "a parent must be in the same workspace" \
    "$(hivemind add "Protected child" --project mine --workspace personal \
        --parent "$root" 2>&1 || true)"
hivemind add "Protected see PROTECTEDMARK" --project mine \
    --workspace personal --link "see=$root" >/dev/null # p2
check "add takes its parent's workspace" "Added post [p3]." \
    "$(hivemind add "Protected child" --project mine --parent p2)"
curl -fsS -X POST -H "Hivemind-API-Version: $api_version" \
    -H "Authorization: Bearer $test_secret" \
    --data "{\"subject\":\"Open linking protected\",\"project\":\"views\",\"origin\":{},\"links\":[{\"name\":\"see\",\"ref\":\"p1\"}]}" \
    "$main_url/api/workspaces/general/posts" >/dev/null
under_protected="$(hivemind search 'Open linking protected' --id-only)"
check "a protected link shows as its bare id" "see p1" \
    "$(hivemind post "$under_protected")"
check "a protected backlink shows as its bare id" "p1 see this post" \
    "$(hivemind post "$root")"
check "a link to an unreadable post is refused" "link target not found" \
    "$(hivemind add "Probe" --project views --link see=p1 2>&1 || true)"
hivemind chat send p2 "Protected message PROTECTEDMARK" >/dev/null
hivemind chat send "$root" "Open message" >/dev/null
open_views="$(hivemind post "$root"; hivemind tree "$root"
    hivemind search --under "$root"; hivemind recent
    hivemind recent --include-chat; hivemind search "$under_protected"
    hivemind post "$under_protected"; hivemind chat recent
    hivemind chat recent "$root"; hivemind wait --after @0 --timeout 1s)"
check_equal "no protected content reaches the open workspace" "" \
    "$(echo "$open_views" | grep PROTECTEDMARK || true)"
check_equal "a tree holds no protected post" "" \
    "$(hivemind tree "$root" | grep -E '\[p[0-9]+\]' || true)"

# Chat.
chat="$(hivemind add "Chat thread" --project chat | added)"
check_equal "a send prints only the message id" "Sent [$chat-m1]." \
    "$(hivemind chat send "$chat" "First QUESTIONMARK")"
hivemind chat send "$chat" "Second" >/dev/null
hivemind chat send "$chat" "Third" >/dev/null
check "a read pages forward from the start with a footer" \
    "Next: hivemind chat read $chat --after $chat-m1 (2 more messages)" \
    "$(hivemind chat read "$chat" --limit 1)"
check_equal "the last page ends with its last message, the cursor" \
    "[$chat-m3]" "$(hivemind chat read "$chat" --after "$chat-m1" | tail -1 |
        sed 's/^\(\[[a-z0-9-]*\]\).*/\1/')"
check_equal "an empty read keeps the cursor" "No messages after $chat-m3." \
    "$(hivemind chat read "$chat" --after "$chat-m3")"
check "an empty read from a time ends with the cursor" \
    "Cursor: $chat-m3" "$(hivemind chat read "$chat" --after 2100-01-01)"
check "a read from a time starts at the first message posted since" \
    "[$chat-m1]" "$(hivemind chat read "$chat" --after 1h --limit 1)"
check "a read after another chat's missing message says it is missing" \
    "message not found: e1-m1" \
    "$(hivemind chat read "$chat" --after e1-m1 2>&1 || true)"
check_equal "chat recent shows the newest, oldest first" "[$chat-m2]
[$chat-m3]" "$(hivemind chat recent "$chat" --limit 2 |
    sed -n 's/^\(\[[a-z0-9-]*\]\).*/\1/p')"
check_equal "chat recent says how to read what it left out" \
    "... 1 earlier (hivemind chat read $chat --after $chat-m0)" \
    "$(hivemind chat recent "$chat" --limit 2 | head -1)"
check_equal "a chat read keeps to the messages its filters match" \
    "[$chat-m2]
Cursor: $chat-m3" "$(hivemind chat read "$chat" --content second |
    sed -E 's/^(\[[a-z0-9-]*\]) .*/\1/')"
check_equal "chat recent filters, and its hint repeats the filters" \
    "... 1 earlier (hivemind chat read $chat --after $chat-m0 --no-content second)
[$chat-m3]" "$(hivemind chat recent "$chat" --no-content second -n 1 |
    sed -E 's/^(\[[a-z0-9-]*\]) .*/\1/')"
check_equal "the chat overview keeps to the messages its filters match" \
    "[$chat-m1]" "$(hivemind chat recent --content questionmark |
    sed -n 's/^  \(\[[a-z0-9-]*\]\).*/\1/p')"
hivemind chat edit "$chat-m1" "First, edited" >/dev/null
check "an edit stays in place" "[$chat-m1]" \
    "$(hivemind chat read "$chat" --after "$chat-m0" --limit 1)"
message_detail="$(hivemind message "$chat-m1")"
check_equal "message shows a message under its post's line" \
    "[$chat] $(id -un): Chat thread
  [$chat-m1] $(id -un) (edited): First, edited" \
    "$(echo "$message_detail" | head -2 |
        sed -E 's/ [0-9-]{10} [0-9]{2}:[0-9]{2}( |$)/\1/')"
check "message shows when a message was edited" "Modified: " \
    "$message_detail"
check "message says a missing message is missing" \
    "message not found: $chat-m99" \
    "$(hivemind message "$chat-m99" 2>&1 || true)"
check_equal "no read returns the old text" "" \
    "$(hivemind chat read "$chat" | grep QUESTIONMARK || true)"
overview="$(hivemind chat recent)"
check_equal "the overview lists the post with the newest message last" \
    "[$chat] $(id -un): Chat thread" \
    "$(echo "$overview" | grep '^\[' | tail -1 |
        sed -E 's/ [0-9-]{10} [0-9]{2}:[0-9]{2}//')"
# Read to 1, a peer sends 2, you send 3 with --after at 1. The send shows
# the peer's message and yours, and a wait from its cursor waits for
# the next message rather than returning yours.
race="$(hivemind add "Cursor race" --project chat | added)"
hivemind chat send "$race" "Read first" >/dev/null
hivemind chat send "$race" "From a peer" >/dev/null
check_equal "a send with --after shows what others sent, then yours" \
    "Sent [$race-m3].
[$race-m2]
[$race-m3]" \
    "$(hivemind chat send "$race" "My own" --after "$race-m1" |
        sed 's/^\(\[[a-z0-9-]*\]\).*/\1/')"
check "a wait from a send's cursor does not return your own message" \
    "No matching messages after $race-m3 within 1s." \
    "$(hivemind chat wait "$race" --after "$race-m3" --timeout 1s)"
check "a message may start with a dash beside --after" \
    ": -1 to that" \
    "$(hivemind chat send --after "$race-m3" "$race" "-1 to that")"
# With 22 messages after the cursor, the send shows the newest 20 and
# how to read the 2 it left out.
for n in $(seq 21); do hivemind chat send "$race" "Peer $n" >/dev/null; done
sent="$(hivemind chat send "$race" "Catching up" --after "$race-m4" |
    sed 's/^\(\[[a-z0-9-]*\]\).*/\1/')"
check_equal "a send shows at most the newest 20 messages after --after" \
    "Sent [$race-m26].
... 2 earlier (hivemind chat read $race --after $race-m4)
[$race-m7]" "$(echo "$sent" | head -3)"
check_equal "a send that left messages out ends with yours, the cursor" \
    "[$race-m26]" "$(echo "$sent" | tail -1)"
check_equal "a send that left messages out shows 20 of them" 20 \
    "$(echo "$sent" | grep -c '^\[')"
check_equal "a send after a time counts what it left out from that time" \
    "... 7 earlier (hivemind chat read $race --after $race-m0)" \
    "$(hivemind chat send "$race" "Within the hour" --after 1h | sed -n 2p)"
(sleep 1.5 && peer chat send "$chat" "Woken" >/dev/null) &
sender_pid=$!
check "a wait returns when a message arrives" "[$chat-m4]" \
    "$(hivemind chat wait "$chat" --after "$chat-m3" --timeout 20s)"
wait "$sender_pid"
check "a wait ends at its timeout" \
    "No matching messages after $chat-m4 within 1s." \
    "$(hivemind chat wait "$chat" --after "$chat-m4" --timeout 1s)"
check "a wait with no --after waits from the newest message" \
    "No matching messages after $chat-m4 within 1s." \
    "$(hivemind chat wait "$chat" --timeout 1s)"
check "a post shows its chat" "Chat: 4 messages, newest $chat-m4" \
    "$(hivemind post "$chat")"
lone="$(hivemind add "Lone message" --project lone | added)"
hivemind chat send "$lone" "Only one" >/dev/null
check "a post counts one message in the singular" \
    "Chat: 1 message, newest $lone-m1" "$(hivemind post "$lone")"
hivemind chat delete "$lone-m1" >/dev/null
check_equal "a post whose messages are all deleted shows no chat" "" \
    "$(hivemind post "$lone" | grep '^Chat:' || true)"
check_equal "recent counts the messages posted since its oldest match" \
    "[$chat] $(id -un): Chat thread (1 new message)" \
    "$(hivemind recent --project chat -n 1 | untimed | head -1)"
check_equal "recent with chat shares the limit with messages" \
    "[$chat-m4] $(id -un) in $chat: Woken
[$race-m27] $(id -un) in $race: Within the hour
Next: hivemind recent --before @R --workspace general --limit 2 --project chat --include-chat (more older matches)
Cursor: @R" \
    "$(hivemind recent --project chat --include-chat -n 2 | untimed)"
check "search shows the matching messages" \
    "[$chat-m4] $(id -un) in $chat: Woken" \
    "$(hivemind search Woken | untimed)"

# Anyone can edit a public post, add under it and chat on it. Only the
# user who wrote a post can delete it or make it private, and only the
# user who wrote a message can edit or delete it. The client always
# sends its own user, so the other user's posts go in by curl.
post_as_heron() {
    curl -fsS -X POST -H "Hivemind-API-Version: $api_version" \
        --data "$2" "$main_url/api/$1" >/dev/null
}
post_as_heron workspaces/general/posts \
    '{"subject":"Heron topic","project":"owners","origin":{"user":"heron"}}'
heron="$(hivemind search 'Heron topic' --id-only)"
check_equal "anyone can edit another user's public post" \
    "Edited post [$heron], now at version 2." \
    "$(hivemind edit "$heron" --body "Detail from another user")"
check "post shows who made the current version" \
    "Version: 2, by $(id -un)@" "$(hivemind post "$heron")"
heron_v1="$(hivemind post "$heron" --version 1)"
check "an earlier version shows who made it" "Version: 1, by heron at " \
    "$heron_v1"
check_equal "an earlier version keeps its own text" "" \
    "$(echo "$heron_v1" | grep 'Detail from another user' || true)"
check "an edit expecting an older version is refused" \
    "$heron is at version 2, not 1. Re-read it with 'hivemind post $heron' before editing it again" \
    "$(hivemind edit "$heron" --body "Stale" --expected-version 1 2>&1 ||
        true)"
check_equal "an edit expecting the current version applies" \
    "Edited post [$heron], now at version 3." \
    "$(hivemind edit "$heron" --body "Fresh" --expected-version 2)"
check "a version past the current one is not found" \
    "$heron has no version 4" \
    "$(hivemind post "$heron" --version 4 2>&1 || true)"
check "only the owner makes a post private" \
    "$heron was posted by heron, so only heron can make it private or public" \
    "$(hivemind edit "$heron" --private 2>&1 || true)"
reply="$(hivemind add "Reply under heron" --project owners --parent "$heron" \
    | added)"
check "anyone can add under another user's post" "[$reply]" \
    "$(hivemind tree "$heron")"
check_equal "anyone can chat on another user's post" "Sent [$heron-m1]." \
    "$(hivemind chat send "$heron" "Please fix the subject")"
post_as_heron "posts/$heron/messages" \
    '{"body":"Fixed soon","origin":{"user":"heron"}}'
check "an origin filter finds that user's messages" \
    "[$heron-m2] heron in $heron: Fixed soon" \
    "$(hivemind search --origin-user heron | untimed)"
check_equal "a message meets the text and origin filters itself" "" \
    "$(hivemind search Please --origin-user heron --id-only)"
heron_changes="$(hivemind wait --after @0 --origin-user heron --include-mine \
    --timeout 1s)"
check "a wait's origin filter matches a message's own origin" \
    "[$heron-m2]" "$heron_changes"
check_equal "a wait's origin filter leaves out others' messages" "" \
    "$(echo "$heron_changes" | grep -F "[$heron-m1]" || true)"
check_equal "a chat wait's origin filter matches each message" \
    "[$heron-m2]" \
    "$(hivemind chat wait "$heron" --after "$heron-m0" --origin-user heron \
        --include-mine --timeout 1s | grep -o "^\[$heron-m[0-9]*\]")"
check_equal "recent finds a post at any level by its origin" \
    "[$reply] $(id -un) in $heron: Reply under heron
Cursor: @R" \
    "$(hivemind recent --project owners --no-origin-user heron | untimed)"
check_equal "recent with chat filters each message itself" \
    "[$heron-m1] $(id -un) in $heron: Please fix the subject
[$reply] $(id -un) in $heron: Reply under heron" \
    "$(hivemind recent --project owners --no-origin-user heron --include-chat |
        untimed | grep -v '^Cursor: ')"
check "a span back from now keeps recent messages" "[$heron-m2]" \
    "$(hivemind search --origin-user heron --after 1h)"
check_equal "a date leaves out what changed after it" "No matches." \
    "$(hivemind search --origin-user heron --before 2000-01-01 | head -1)"
check "recent keeps to what changed in the span" "Heron topic" \
    "$(hivemind recent --project owners --after 10m)"
check_equal "recent leaves out what did not change in the span" \
    "No matches." \
    "$(hivemind recent --project owners --before 2000-01-01 | head -1)"
check "a time flag names the forms it takes" "like 2d, 6h or 90m" \
    "$(hivemind search --after yesterday 2>&1 || true)"
check "an edit of another user's message is refused" \
    "$heron-m2 was posted by heron, so only heron can edit or delete it" \
    "$(hivemind chat edit "$heron-m2" "Taken" 2>&1 || true)"
check "a delete of another user's post names its owner and the next step" \
    "$heron was posted by heron, so only heron can delete it. To suggest a change, send a message in its chat with 'hivemind chat send $heron \"<message>\"'" \
    "$(hivemind delete "$heron" 2>&1 || true)"
check "a delete of another user's message is refused" \
    "only heron can edit or delete it" \
    "$(hivemind chat delete "$heron-m2" 2>&1 || true)"

# A private post, and everything under it, is for its owner alone.
# Everyone else gets post not found for it. The client always reads as
# its own user, so the other user reads by curl.
read_as_heron() {
    curl -sS -H "Hivemind-API-Version: $api_version" \
        -H "Hivemind-User: heron" "$main_url/api/$1"
}
before_private="$(hivemind wait --after @0 --include-mine --timeout 1s |
    cursor_of)"
draft_added="$(hivemind add "Private draft" --project owners \
    --body "Draft text" --private)"
draft="$(echo "$draft_added" | added)"
check_equal "add --private says who alone can read the post" \
    "Added post [$draft].
Only $(id -un) can read it." "$draft_added"
draft_child="$(hivemind add "Draft detail" --project owners --parent "$draft" \
   )"
check "a post under a private post is private too" \
    "Only $(id -un) can read it, as a post above it is private." \
    "$draft_child"
detail="$(echo "$draft_child" | added)"
check_equal "a listing marks private posts" \
    "[$detail] $(id -un) in $draft [private]: Draft detail
[$draft] $(id -un) [private]: Private draft" \
    "$(hivemind recent --project owners --subject 'Draft*' | untimed |
        grep -v '^Cursor: ')"
check_equal "a tree marks private posts" \
    "[$draft] $(id -un) [private]: Private draft
  [$detail] $(id -un) [private]: Draft detail" \
    "$(hivemind tree "$draft" | untimed)"
check "post says who alone can read a private post" \
    "Private: only $(id -un) can read it" "$(hivemind post "$draft")"
check "a wait marks private posts" "$(id -un) [private]: Private draft" \
    "$(hivemind wait --after "$before_private" --include-mine --timeout 1s)"
check "another user gets post not found for a private post" \
    '"code":"post_not_found"' "$(read_as_heron "posts/$draft")"
check "another user gets post not found under a private post" \
    '"code":"post_not_found"' "$(read_as_heron "posts/$detail")"
check_equal "another user's listing leaves out private posts" \
    '{"matches":[],"more":false,' \
    "$(read_as_heron "workspaces/general/posts?subject=draft*" |
        grep -o '^{"matches":\[\],"more":false,')"
check_equal "another user's wait leaves out private posts" "" \
    "$(read_as_heron "workspaces/general/changes?after=$before_private" |
        grep -o 'Private draft' || true)"
check_equal "edit --public shows the post to everyone" \
    "Edited post [$draft], now at version 1.
Everyone can read it." "$(hivemind edit "$draft" --public)"
check "another user reads a post made public, and what is under it" \
    '"subject":"Draft detail"' "$(read_as_heron "posts/$detail")"
looks_good="$(curl -fsS -X POST -H "Hivemind-API-Version: $api_version" \
    --data '{"body":"Looks good","origin":{"user":"heron"}}' \
    "$main_url/api/posts/$draft/messages" |
    grep -o '"number":[0-9]*' | cut -d: -f2)"
check "a post others chatted on cannot be made private" \
    "others have posts or messages under $draft that they have not deleted, so it cannot be made private" \
    "$(hivemind edit "$draft" --private 2>&1 || true)"
curl -fsS -X DELETE -H "Hivemind-API-Version: $api_version" \
    --data '{"user":"heron"}' \
    "$main_url/api/posts/$draft/messages/$looks_good" >/dev/null
check_equal "a post whose other messages are deleted can be made private" \
    "Edited post [$draft], now at version 1.
Only $(id -un) can read it." "$(hivemind edit "$draft" --private)"
check "the other user then loses the deleted message too" \
    '"code":"post_not_found"' \
    "$(read_as_heron "posts/$draft/messages/$looks_good")"
hivemind edit "$draft" --public >/dev/null

# A delete removes a post's text and metadata, or a message's text, and
# keeps its place.
gadwall="$(hivemind add "Gadwall nest" --project owners --body "Code 4417" \
    --jira QUACK-9 --parent "$reply" | added)"
egg="$(hivemind add "Eggs counted" --project owners --parent "$gadwall" |
    added)"
empty="$(hivemind add "Empty nest" --project owners --parent "$reply" | added)"
hivemind delete "$empty" >/dev/null
hivemind chat send "$gadwall" "Before" >/dev/null
hivemind chat send "$gadwall" "The code is 4417" >/dev/null
hivemind chat send "$gadwall" "After" >/dev/null
hivemind chat send "$gadwall" "Oops" >/dev/null
hivemind edit "$gadwall" --body "Code 4418" >/dev/null
before_delete="$(hivemind wait --after @0 --include-mine --timeout 1s |
    cursor_of)"
deleted_post="$(hivemind delete "$gadwall")"
check_equal "you can delete your own post" "Deleted post [$gadwall]." \
    "$deleted_post"
deleted_message="$(hivemind chat delete "$gadwall-m2")"
check_equal "you can delete your own message" "Deleted [$gadwall-m2]." \
    "$deleted_message"
hivemind chat delete "$gadwall-m4" >/dev/null
gadwall_post="$(hivemind post "$gadwall")"
check "a deleted post shows as deleted" "[$gadwall] $(id -un) (deleted)" \
    "$(echo "$gadwall_post" | untimed)"
check "message shows a deleted message as deleted" \
    "  [$gadwall-m2] $(id -un) (deleted)" \
    "$(hivemind message "$gadwall-m2" |
        sed -E 's/ [0-9-]{10} [0-9]{2}:[0-9]{2} / /')"
check "a deleted post shows when it was deleted" "Deleted: " \
    "$gadwall_post"
check_equal "a deleted post keeps no body or metadata" "" \
    "$(echo "$gadwall_post" |
        grep -E 'Gadwall|4417|QUACK|Metadata|Modified' || true)"
check "a deleted post keeps its origin" "From: $(id -un)@" "$gadwall_post"
gadwall_v1="$(hivemind post "$gadwall" --version 1)"
check "a deleted post's earlier version shows as deleted" \
    "[$gadwall] $(id -un) (deleted)" "$(echo "$gadwall_v1" | untimed)"
check_equal "a deleted post's earlier versions keep no text" "" \
    "$(echo "$gadwall_v1" | grep -E 'Gadwall|4417|QUACK|Metadata' || true)"
check "a child's path shows its deleted parent" "> $gadwall (deleted)" \
    "$(hivemind post "$egg")"
untimed() { sed -E 's/\] [0-9-]{10} [0-9]{2}:[0-9]{2}:? /] /'; }
check_equal "a tree keeps a deleted post above the posts under it" \
    "[$reply] $(id -un): Reply under heron
  [$gadwall] $(id -un) (deleted)
    [$egg] $(id -un): Eggs counted" \
    "$(hivemind tree "$reply" | untimed | grep -v '^Path:')"
check_equal "a tree with deleted posts shows them" \
    "[$reply] $(id -un): Reply under heron
  [$gadwall] $(id -un) (deleted)
    [$egg] $(id -un): Eggs counted
  [$empty] $(id -un) (deleted)" \
    "$(hivemind tree "$reply" --include-deleted | untimed | grep -v '^Path:')"
check_equal "a tree of a deleted post shows it and the posts under it" \
    "[$gadwall] $(id -un) (deleted)
  [$egg] $(id -un): Eggs counted" \
    "$(hivemind tree "$gadwall" | untimed | grep -v '^Path:')"
owners="$(hivemind recent --project owners --include-chat | untimed)"
check "a listing leaves out a deleted post but not the posts under it" \
    "[$egg] $(id -un) in $heron.$reply.$gadwall: Eggs counted" "$owners"
check "a listing keeps the chat of a deleted post" \
    "[$gadwall-m1] $(id -un) in $heron.$reply.$gadwall: Before" "$owners"
check_equal "a listing leaves out deleted posts and messages" "" \
    "$(echo "$owners" | grep -F -e "[$gadwall]" -e "[$gadwall-m2]" || true)"
with_deleted="$(hivemind recent --project owners --include-chat \
    --include-deleted 2>&1 | untimed)"
check "a listing says it includes deleted posts" \
    "matching --project owners, deleted included:" "$with_deleted"
check "a listing with deleted posts shows a deleted post as deleted" \
    "[$gadwall] $(id -un) in $heron.$reply (deleted)" "$with_deleted"
check "a listing with deleted posts shows a deleted message as deleted" \
    "[$gadwall-m2] $(id -un) in $heron.$reply.$gadwall (deleted)" \
    "$with_deleted"
check "the next page keeps deleted posts" "--include-deleted" \
    "$(hivemind recent --project owners --include-deleted -n 1 2>/dev/null |
        grep '^Next:')"
check_equal "--exclude-deleted lists as the default does" "$owners" \
    "$(hivemind recent --project owners --include-chat --exclude-deleted |
        untimed)"
check_equal "a chat leaves out deleted messages, but its cursor passes them" \
    "[$gadwall-m1] $(id -un): Before
[$gadwall-m3] $(id -un): After
Cursor: $gadwall-m4" "$(hivemind chat recent "$gadwall" | untimed)"
check_equal "a chat with deleted messages shows them in their place" \
    "[$gadwall-m1] $(id -un): Before
[$gadwall-m2] $(id -un) (deleted)
[$gadwall-m3] $(id -un): After
[$gadwall-m4] $(id -un) (deleted)" \
    "$(hivemind chat read "$gadwall" --include-deleted | untimed)"
check_equal "a read from before a deleted message moves its cursor past it" \
    "No messages after $gadwall-m3.
Cursor: $gadwall-m4" "$(hivemind chat read "$gadwall" --after "$gadwall-m3")"
check_equal "a read from that cursor finds nothing more" \
    "No messages after $gadwall-m4." \
    "$(hivemind chat read "$gadwall" --after "$gadwall-m4")"
check_equal "a chat counts only the messages it would show earlier" \
    "... 1 earlier (hivemind chat read $gadwall --after $gadwall-m0)
[$gadwall-m3] $(id -un): After
Cursor: $gadwall-m4" \
    "$(hivemind chat recent "$gadwall" --limit 1 | untimed)"
check "post counts the messages that are not deleted" \
    "Chat: 2 messages, newest $gadwall-m3" "$gadwall_post"
check "the chat overview names a deleted post as deleted" \
    "[$gadwall] $(id -un) (deleted)" "$(hivemind chat recent | untimed)"
check_equal "search no longer finds deleted text" "No matches." \
    "$(hivemind search 4417 | head -1)"
deletes="$(hivemind wait --after "$before_delete" --include-mine --timeout 1s |
    untimed)"
check "a wait shows the deleted post and message" \
    "[$gadwall] $(id -un) in $heron.$reply (deleted)
[$gadwall-m2] $(id -un) in $heron.$reply.$gadwall (deleted)" "$deletes"
after_delete="$(echo "$deletes" | cursor_of)"
check "an edit of a deleted post is refused" \
    "$gadwall was deleted, so it cannot be edited" \
    "$(hivemind edit "$gadwall" "Gadwall nest" 2>&1 || true)"
check "a link edit of a deleted post is refused" \
    "$gadwall was deleted, so it cannot be edited" \
    "$(hivemind edit "$gadwall" --link see="$egg" 2>&1 || true)"
check "an edit of a deleted message is refused" \
    "$gadwall-m2 was deleted, so it cannot be edited" \
    "$(hivemind chat edit "$gadwall-m2" "The code is 4417" 2>&1 || true)"
check_equal "deleting again succeeds, at the revision of the delete" \
    "$deleted_post
$deleted_message" \
    "$(hivemind delete "$gadwall" && hivemind chat delete "$gadwall-m2")"
check "deleting again changes nothing" "No matching changes" \
    "$(hivemind wait --after "$after_delete" --include-mine --timeout 1s)"
hivemind delete "$(hivemind add "Ghost" --project ghosts | added)" >/dev/null
nest="$(hivemind add "Deleted chat" --project deletions | added)"
hivemind chat send "$nest" "Kept" >/dev/null
hivemind chat send "$nest" "Gone" >/dev/null
hivemind chat delete "$nest-m2" >/dev/null
hivemind chat send "$nest" "Later" >/dev/null
check_equal "a chat wait leaves out deleted messages" \
    "[$nest-m3] $(id -un): Later" \
    "$(hivemind chat wait "$nest" --after "$nest-m1" --include-mine \
        --timeout 1s | untimed)"
check_equal "a chat wait with --include-deleted shows them" \
    "[$nest-m2] $(id -un) (deleted)
[$nest-m3] $(id -un): Later" \
    "$(hivemind chat wait "$nest" --after "$nest-m1" --include-mine \
        --include-deleted --timeout 1s | untimed)"
check_equal "a send with --include-deleted shows them" \
    "Sent [$nest-m4].
[$nest-m2] $(id -un) (deleted)
[$nest-m3] $(id -un): Later
[$nest-m4] $(id -un): Last" \
    "$(hivemind chat send "$nest" "Last" --after "$nest-m1" --include-deleted |
        untimed)"
check_equal "list-projects leaves out a project whose posts are deleted" \
    "" "$(hivemind list-projects | grep ghosts || true)"
check "list-projects with deleted posts lists it" "ghosts" \
    "$(hivemind list-projects --include-deleted)"

# Changes.
private_changes="$(hivemind wait --after @0 --workspace personal \
    --include-mine --timeout 1s)"
check "wait lists a workspace's changes from the start" "[p2-m1]" \
    "$private_changes"
private_cursor="$(echo "$private_changes" | cursor_of)"
check "a wait from the last cursor finds nothing new" \
    "No matching changes after $private_cursor" \
    "$(hivemind wait --after "$private_cursor" --workspace personal \
        --timeout 1s)"
sleep 1
mark="$(date +%Y-%m-%dT%H:%M:%S%:z)"
marked="$(peer add "After the mark" --project marks | added)"
check_equal "a wait from a time shows the changes from that time on" \
    "[$marked] $(id -un): After the mark" \
    "$(hivemind wait --after "$mark" --timeout 1s | grep '^\[' | untimed)"
(sleep 1.5 && peer chat send "$chat" "Changed" >/dev/null) &
sender_pid=$!
check "a wait from now returns when anything changes" "[$chat-m5]" \
    "$(hivemind wait --timeout 20s)"
wait "$sender_pid"
hivemind firehose --timeout 5s >"$test_dir/firehose.out" &
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
hivemind chat send "$chat" "Hosed" >/dev/null
hivemind add "Hosed post" --project chat >/dev/null
wait "$firehose_pid"
firehose_output="$(cat -- "$test_dir/firehose.out")"
check "a firehose prints messages as they arrive" "[$chat-m6]" \
    "$firehose_output"
check "a firehose prints posts as they arrive" "Hosed post" \
    "$firehose_output"

# A listing gives the revision it read at, so a wait from it shows
# exactly what came after. So does a wait from the last message a read
# showed, or from the post a write added.
listed="$(hivemind recent --project chat | cursor_of)"
peer chat send "$chat" "After the listing" >/dev/null
since_listing="$(hivemind wait --after "$listed" --timeout 1s)"
check_equal "a wait from a listing's cursor shows only what came after" \
    "[$chat-m7] $(id -un) in $chat: After the listing" \
    "$(echo "$since_listing" | grep '^\[' | untimed)"
last_read="$(hivemind chat read "$chat" --after "$chat-m6" | tail -1 |
    sed 's/^\[\([a-z0-9-]*\)\].*/\1/')"
written="$(hivemind add "Written" --project chat | added)"
peer chat send "$chat" "After the write" >/dev/null
check_equal "a wait from a chat read's last message shows what came after" \
    "[$written] $(id -un): Written
[$chat-m8] $(id -un) in $chat: After the write" \
    "$(hivemind wait --after "$last_read" --include-mine --timeout 1s |
        grep '^\[' | untimed)"
check_equal "a wait without chat shows only the posts" \
    "[$written] $(id -un): Written" \
    "$(hivemind wait --after "$last_read" --include-mine --exclude-chat \
        --timeout 1s | grep '^\[' | untimed)"
check_equal "a wait without posts shows only the messages" \
    "[$chat-m8] $(id -un) in $chat: After the write" \
    "$(hivemind wait --after "$last_read" --include-mine --exclude-posts \
        --timeout 1s | grep '^\[' | untimed)"
check_equal "a wait from a written post shows only what came after" \
    "[$chat-m8] $(id -un) in $chat: After the write" \
    "$(hivemind wait --after "$written" --include-mine --timeout 1s |
        grep '^\[' | untimed)"

# A post or message id works as a wait's cursor, from when it was added
# or sent, so a wait from it misses nothing that changed after it, even
# a change to what came before it, or a change to the post itself.
sent_id() { sed -n 's/^Sent \[\([a-z0-9-]*\)\].*/\1/p'; }
sent="$(hivemind chat send "$chat" "Waited after" | sent_id)"
peer chat edit "$chat-m1" "Edited after the wait's message" >/dev/null
peer chat send "$written" "In another chat" >/dev/null
check_equal "a wait from a message id shows exactly what came after" \
    "[$chat-m1] $(id -un) in $chat (edited): Edited after the wait's message
[$written-m1] $(id -un) in $written: In another chat" \
    "$(hivemind wait --after "$sent" --include-mine --timeout 1s |
        grep '^\[' | untimed)"
waited="$(hivemind add "Waited post" --project chat | added)"
peer chat send "$chat" "After the post" >/dev/null
peer edit "$waited" --subject "Waited post, edited" >/dev/null
check_equal "a wait from an edited post's id shows what came after it was added" \
    "[$chat-m10] $(id -un) in $chat: After the post
[$waited] $(id -un): Waited post, edited" \
    "$(hivemind wait --after "$waited" --include-mine --timeout 1s |
        grep '^\[' | untimed)"
check "a wait from an id waits in that id's workspace" "[p2-m1]" \
    "$(hivemind wait --after p1 --include-mine --timeout 1s)"
check "a wait from an id in another workspace reads on from its posting" \
    "No matching changes after $sent" \
    "$(hivemind wait --after "$sent" --workspace personal --timeout 1s)"
check_equal "a listing after an id keeps to what changed after its posting" \
    "[$waited] $(id -un): Waited post, edited
[$chat-m10] $(id -un) in $chat: After the post
[$written-m1] $(id -un) in $written: In another chat" \
    "$(hivemind recent --project chat --include-chat --after "$sent" |
        grep '^\[' | untimed)"
check_equal "a chat read after another post keeps to what came after it" \
    "$chat-m8 $chat-m9 $chat-m10" \
    "$(hivemind chat read "$chat" --after "$written" |
        sed -n 's/^\[\([a-z0-9-]*\)\].*/\1/p' | tr '\n' ' ' | sed 's/ $//')"
check_equal "a chat read keeps between its bounds" "[$chat-m8]" \
    "$(hivemind chat read "$chat" --after "$chat-m7" --before "$chat-m9" |
        sed 's/ .*//')"
check_equal "chat recent pages back with --before" \
    "... 1 earlier (hivemind chat read $chat --after $chat-m0 --before $chat-m3)
[$chat-m2]" \
    "$(hivemind chat recent "$chat" --before "$chat-m3" -n 1 |
        sed -E 's/^(\[[a-z0-9-]*\]) .*/\1/')"

# A wait leaves out the changes of its own session, and wakes for
# another session of the same user. Its cursor passes what it left out.
start="$(lead wait --timeout 1ms | cursor_of)"
own="$(lead add "Own changes" --project own | added)"
lead chat send "$own" "Lead message" >/dev/null
mine="$(lead wait --after "$start" --timeout 1s)"
check "a wait sleeps through your own session's changes" \
    "No matching changes after $start within 1s." "$mine"
mine_cursor="$(echo "$mine" | cursor_of)"
check "a wait's cursor passes the changes it left out" \
    "No matching changes after $mine_cursor" \
    "$(lead wait --after "$mine_cursor" --include-mine --timeout 1s)"
others="$(peer wait --after "$start" --timeout 1s)"
check "another session of your user sees your posts" "Own changes" \
    "$others"
check "another session of your user sees your messages" "[$own-m1]" \
    "$others"
check "--include-mine shows your own changes" "[$own-m1]" \
    "$(lead wait --after "$start" --include-mine --timeout 1s)"
hivemind chat send "$own" "Bare message" >/dev/null
check "outside a session, your sessionless changes are yours" \
    "No matching changes after $mine_cursor" \
    "$(hivemind wait --after "$mine_cursor" --timeout 1s)"
check "a session wakes for your sessionless changes" "[$own-m2]" \
    "$(lead wait --after "$mine_cursor" --timeout 1s)"
filtered="$(peer wait --after "$start" --project own --content 'lead MESSAGE' \
    --timeout 1s)"
check "a wait's filters match a message by its own text" "[$own-m1]" \
    "$filtered"
# Only a change to the post itself starts a line with its bare id.
own_post_line="^\[$own\] [0-9]"
check_equal "a wait's filters leave out what does not match" "" \
    "$(echo "$filtered" | grep -e "$own_post_line" -e "\[$own-m2\]" ||
        true)"
check_equal "a firehose with --exclude-mine leaves out your own" "" \
    "$(lead firehose --after "$start" --exclude-mine --timeout 1s |
        grep -e "$own_post_line" -e "\[$own-m1\]" || true)"
check "a firehose with filters shows what matches" "[$own-m2]" \
    "$(hivemind firehose --after "$start" --content bare --timeout 1s)"
lonely="$(lead chat wait "$own" --after "$own-m0" --timeout 1s)"
check_equal "a chat wait shows only messages that are not yours" \
    "[$own-m2]" \
    "$(echo "$lonely" | sed 's/^\(\[[a-z0-9-]*\]\).*/\1/')"
(sleep 1.5 && peer chat send "$own" "Answered" >/dev/null) &
sender_pid=$!
lead chat send "$own" "Asked again" >/dev/null
answered="$(lead chat wait "$own" --after "$own-m2" --timeout 20s)"
wait "$sender_pid"
check "a chat wait sleeps through your message to the reply" "[$own-m4]" \
    "$answered"
check_equal "a chat wait prints none of your own messages" "" \
    "$(echo "$answered" | grep -F "[$own-m3]" || true)"
check "a chat wait with --include-mine shows your own messages" \
    "[$own-m3]" \
    "$(lead chat wait "$own" --after "$own-m2" --include-mine --timeout 1s)"

# The server log keeps no client text, and records each issued id.
hivemind add "Subject OLDSUBJECTMARK" --body "Body OLDBODYMARK" \
    --project pond >/dev/null
hivemind search SEARCHMARK >/dev/null
check_status "an unknown field is refused" "400" \
    -X POST -H "Hivemind-API-Version: $api_version" \
    --data '{"subject":"note","UNKNOWNFIELDMARK":1}' \
    "$main_url/api/workspaces/general/posts"
check_equal "the server log holds no client text" "" \
    "$(grep -oE '[A-Z]*MARK' -- "$test_dir/main.log" || true)"
check "the server log records each issued post id" "issued $chat" \
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
listings:
  include_metadata: [status, missing, jira]
EOF
forum_post="$(cd "$test_dir/private-project" &&
    CLAUDE_CODE_SESSION_ID=private-session-id \
        forum add "Forum question" --project chat | added)"
check_equal "forum records the user alone, in the post and its version" \
    "\"origin\":{\"user\":\"$(id -un)\"}" \
    "$(curl -fsS -H "Hivemind-API-Version: $api_version" \
        "$main_url/api/posts/$forum_post" | grep -o '"origin":{[^}]*}' |
        sort -u)"
forum chat send "$forum_post" "Forum reply" >/dev/null
forum chat send "$forum_post" "Another forum reply" >/dev/null
check_equal "the private context never reached the server" "" \
    "$(curl -fsS -H "Hivemind-API-Version: $api_version" \
        "$main_url/api/posts/$forum_post/messages?after=0" |
        grep -oE 'private-[a-z-]*|"host"|"dir"' || true)"
check "forum help names forum" "  forum add <subject>" \
    "$(forum help)"
check_equal "forum help never suggests hivemind" "" \
    "$(forum help | grep -E "(^|[ '])hivemind " || true)"
check "forum chat continues with forum" \
    "Next: forum chat read $forum_post --after $forum_post-m1" \
    "$(forum chat read "$forum_post" --limit 1)"
listed="$(forum add "Ducks fly south" --project listed --meta status=outdated \
    --jira QUACK-7 | added)"
check_equal "a listing line ends with the configured metadata in order" \
    "[$listed] $(id -un): Ducks fly south [outdated] [QUACK-7]" \
    "$(forum recent --project listed | untimed | grep -v '^Cursor: ')"
check "a tree line ends with the configured metadata" \
    "Ducks fly south [outdated] [QUACK-7]" "$(forum tree "$listed")"
check "forum post points at forum chat" \
    "see 'forum chat recent $forum_post'" "$(forum post "$forum_post")"
check "a forum usage error names forum" "usage: forum post <ref>" \
    "$(forum post 2>&1 || true)"
check "forum knows only its own workspaces" \
    "no configured server has a workspace with prefix r" \
    "$(forum post r1 2>&1 || true)"
cat >"$test_dir/forum-down.yml" <<EOF
workspaces:
  general:
    server: http://127.0.0.1:1
EOF
check "an unreachable server is named" "request to http://127.0.0.1:1 failed" \
    "$(FORUM_CONFIG="$test_dir/forum-down.yml" \
        forum add "Lost" --project chat 2>&1 || true)"

# A write repeated under its request id makes one post or message, and
# an id used for other content is refused.
once="$(hivemind add "Added once" --project repeats --request-id e2e-post \
    | added)"
check "an add repeated under its request id gives the same post" \
    "Added post [$once]." \
    "$(hivemind add "Added once" --project repeats --request-id e2e-post \
       )"
for _ in 1 2; do
    check_equal "a send repeated under its request id gives one message" \
        "Sent [$once-m1]." \
        "$(hivemind chat send "$once" "Sent once" --request-id e2e-m)"
done
check_equal "the chat holds the message once" "1" \
    "$(hivemind chat read "$once" | grep -c '^\[')"
check "a request id used for other text is refused" \
    "request id already used: \"e2e-m\" sent $once-m1 with other text" \
    "$(hivemind chat send "$once" "Sent twice" --request-id e2e-m 2>&1 || true)"

# Subjects: a headline of 72 characters, then an optional kind suffix.
headline="$(printf 'h%.0s' {1..72})"
check "a 72-character headline may end with a kind" "Added post" \
    "$(hivemind add "$headline (answered)" --project limits)"
check "a headline over 72 characters is refused" "subject is too long: 73" \
    "$(hivemind add "${headline}h (answered)" --project limits 2>&1 || true)"
check "a suffix that is not one word counts toward the headline" \
    "subject is too long: 83" \
    "$(hivemind add "$headline (answer 1)" --project limits 2>&1 || true)"

echo
if [[ "$failures" -gt 0 ]]; then
    echo "$failures FAILURES"
    exit 1
fi
echo "All e2e tests passed."
