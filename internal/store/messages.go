package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/airspacesh/workstream/internal/api"
)

var ErrMessageNotFound = errors.New("message not found")

const messageColumns = `m.workspace_id, m.entry_id, m.id, m.created_utc,
	m.modified_utc, m.body, m.origin_user, m.origin_host, m.origin_dir,
	m.origin_claude_session`

// scanMessage reads the messageColumns, and any extra columns into
// extra.
func (s *Store) scanMessage(
	row rowScanner, extra ...any,
) (api.Message, error) {
	var message api.Message
	var key Key
	var created, modified string
	err := row.Scan(append([]any{
		&key.Workspace, &key.ID, &message.Number, &created, &modified,
		&message.Body, &message.Origin.User, &message.Origin.Host,
		&message.Origin.Dir, &message.Origin.ClaudeSession,
	}, extra...)...)
	if err != nil {
		return api.Message{}, err
	}
	message.Entry = s.mustRef(key)
	if message.Created, err = parseTime(created); err != nil {
		return api.Message{}, err
	}
	if message.Modified, err = parseTime(modified); err != nil {
		return api.Message{}, err
	}
	return message, nil
}

func (s *Store) getMessage(
	ctx context.Context, tx *sql.Tx, entry Key, number int64,
) (api.Message, error) {
	message, err := s.scanMessage(tx.QueryRowContext(ctx, `
		SELECT `+messageColumns+` FROM entry_messages AS m
		WHERE m.workspace_id = ? AND m.entry_id = ? AND m.id = ?`,
		entry.Workspace, entry.ID, number,
	))
	if errors.Is(err, sql.ErrNoRows) {
		ref := api.MessageRef{Entry: s.mustRef(entry), Number: number}
		return api.Message{}, fmt.Errorf("%w: %s", ErrMessageNotFound, ref)
	}
	if err != nil {
		return api.Message{}, fmt.Errorf("reading message: %w", err)
	}
	return message, nil
}

// recordChatActivity moves an entry's activity time to a change in its
// chat, in the transaction that makes the change.
func recordChatActivity(
	ctx context.Context, tx *sql.Tx, entry Key, at string,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE entries SET activity_utc = ?
		WHERE workspace_id = ? AND id = ?`,
		at, entry.Workspace, entry.ID,
	); err != nil {
		return fmt.Errorf("recording chat activity: %w", err)
	}
	return nil
}

// Post adds a message to an entry's chat, numbered one past the
// entry's highest message. Writes are serialised through the store's
// one connection, so no other post can take the same number.
func (s *Store) Post(
	ctx context.Context, entry Key, req api.PostMessageRequest,
) (api.Message, error) {
	v, err := s.begin(ctx, NewAccess(entry.Workspace), false)
	if err != nil {
		return api.Message{}, fmt.Errorf("posting message: %w", err)
	}
	defer v.tx.Rollback()
	if err := v.checkEntryExists(ctx, entry); err != nil {
		return api.Message{}, err
	}

	var number int64
	if err := v.tx.QueryRowContext(ctx, `
		SELECT coalesce(max(id), 0) + 1 FROM entry_messages
		WHERE workspace_id = ? AND entry_id = ?`,
		entry.Workspace, entry.ID,
	).Scan(&number); err != nil {
		return api.Message{}, fmt.Errorf("numbering message: %w", err)
	}
	revision, err := nextRevision(ctx, v.tx)
	if err != nil {
		return api.Message{}, err
	}
	now := nowUTC()
	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO entry_messages (
			workspace_id, entry_id, id, created_utc, modified_utc,
			revision, body, origin_user, origin_host, origin_dir,
			origin_claude_session
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Workspace, entry.ID, number, now, now, revision, req.Body,
		req.Origin.User, req.Origin.Host, req.Origin.Dir,
		req.Origin.ClaudeSession,
	); err != nil {
		return api.Message{}, fmt.Errorf("posting message: %w", err)
	}
	if err := recordChatActivity(ctx, v.tx, entry, now); err != nil {
		return api.Message{}, err
	}
	message, err := s.getMessage(ctx, v.tx, entry, number)
	if err != nil {
		return api.Message{}, err
	}
	if err := v.tx.Commit(); err != nil {
		return api.Message{}, fmt.Errorf("committing message: %w", err)
	}
	return message, nil
}

// EditMessage overwrites a message's text, and reports whether it
// changed. The message keeps its number and its place in the chat, and
// no earlier text is kept. Text that equals the current text changes
// nothing, so repeating an edit is harmless. Only the user who posted
// the message can edit it.
func (s *Store) EditMessage(
	ctx context.Context, entry Key, number int64, req api.EditMessageRequest,
) (api.Message, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, false, fmt.Errorf("editing message: %w", err)
	}
	defer tx.Rollback()
	message, err := s.getMessage(ctx, tx, entry, number)
	if err != nil {
		return api.Message{}, false, err
	}
	if err := checkEditor(message.Ref(), message.Origin.User,
		req.User); err != nil {
		return api.Message{}, false, err
	}
	if message.Body == req.Body {
		return message, false, nil
	}

	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return api.Message{}, false, err
	}
	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE entry_messages SET body = ?, modified_utc = ?, revision = ?
		WHERE workspace_id = ? AND entry_id = ? AND id = ?`,
		req.Body, now, revision, entry.Workspace, entry.ID, number,
	); err != nil {
		return api.Message{}, false, fmt.Errorf("editing message: %w", err)
	}
	if err := recordChatActivity(ctx, tx, entry, now); err != nil {
		return api.Message{}, false, err
	}
	message, err = s.getMessage(ctx, tx, entry, number)
	if err != nil {
		return api.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, false, fmt.Errorf(
			"committing message edit: %w", err)
	}
	return message, true, nil
}

// DeleteMessage overwrites a message's text with api.DeletedText, as
// an edit would. Deleting a deleted message changes nothing. Only the
// user who posted the message can delete it.
func (s *Store) DeleteMessage(
	ctx context.Context, entry Key, number int64, req api.DeleteRequest,
) (api.Message, error) {
	message, _, err := s.EditMessage(ctx, entry, number,
		api.EditMessageRequest{Body: api.DeletedText, User: req.User})
	return message, err
}

// readChatPage reads the messages a query selects into a page, oldest
// first. The query reads at most limit messages after since, so a full
// page ends at its last message, and a page short of it has checked
// every message after since, so its cursor passes them all. Remaining
// counts the messages after the cursor.
func (s *Store) readChatPage(
	ctx context.Context,
	tx *sql.Tx,
	entry Key,
	since int64,
	limit int,
	query string,
	args ...any,
) (api.ChatPage, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
	}
	defer rows.Close()
	page := api.ChatPage{Messages: []api.Message{}}
	for rows.Next() {
		message, err := s.scanMessage(rows)
		if err != nil {
			return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
		}
		page.Messages = append(page.Messages, message)
	}
	if err := rows.Err(); err != nil {
		return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
	}
	slices.SortFunc(page.Messages, func(a, b api.Message) int {
		return cmp.Compare(a.Number, b.Number)
	})

	if len(page.Messages) < limit {
		var last int64
		if err := tx.QueryRowContext(ctx, `
			SELECT coalesce(max(id), 0) FROM entry_messages
			WHERE workspace_id = ? AND entry_id = ?`,
			entry.Workspace, entry.ID,
		).Scan(&last); err != nil {
			return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
		}
		page.Cursor = max(since, last)
		return page, nil
	}
	page.Cursor = page.Messages[len(page.Messages)-1].Number
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM entry_messages
		WHERE workspace_id = ? AND entry_id = ? AND id > ?`,
		entry.Workspace, entry.ID, page.Cursor,
	).Scan(&page.Remaining); err != nil {
		return api.ChatPage{}, fmt.Errorf("counting messages: %w", err)
	}
	return page, nil
}

// Messages returns up to limit messages of an entry's chat after the
// cursor since that match a filter, oldest first, leaving out those
// whose origin has the key exclude, when there is one. The filter's
// text matches each message.
//
// The page's cursor is the highest message number the read checked.
// Every message up to it is in the page or did not match, so a read
// from the cursor misses none, even when the page is empty.
func (s *Store) Messages(
	ctx context.Context,
	entry Key,
	since int64,
	filter Filter,
	exclude *OriginKey,
	limit int,
) (api.ChatPage, error) {
	where, err := buildMessageWhere(filter)
	if err != nil {
		return api.ChatPage{}, err
	}
	where.excludeOrigin("m", exclude)
	where.clauses = append(where.clauses, `m.workspace_id = ?`,
		`m.entry_id = ?`, `m.id > ?`)
	where.args = append(where.args, entry.Workspace, entry.ID, since)

	tx, err := s.reads.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
	}
	defer tx.Rollback()
	v := view{store: s, tx: tx}
	if err := v.checkEntryExists(ctx, entry); err != nil {
		return api.ChatPage{}, err
	}
	return s.readChatPage(ctx, tx, entry, since, limit, `
		SELECT `+messageColumns+`
		FROM entry_messages AS m CROSS JOIN entries AS e
			ON e.workspace_id = m.workspace_id AND e.id = m.entry_id`+
		where.sql()+`
		ORDER BY m.id LIMIT ?`, append(where.args, limit)...)
}

// LatestMessages returns the newest count messages of an entry's chat,
// oldest first.
func (s *Store) LatestMessages(
	ctx context.Context, entry Key, count int,
) (api.ChatPage, error) {
	tx, err := s.reads.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
	}
	defer tx.Rollback()
	v := view{store: s, tx: tx}
	if err := v.checkEntryExists(ctx, entry); err != nil {
		return api.ChatPage{}, err
	}
	return s.readChatPage(ctx, tx, entry, 0, count, `
		SELECT `+messageColumns+` FROM entry_messages AS m
		WHERE m.workspace_id = ? AND m.entry_id = ?
		ORDER BY m.id DESC LIMIT ?`,
		entry.Workspace, entry.ID, count)
}

// Overview returns the newest limit messages across a workspace,
// grouped by entry. Entries come in order of their newest message, the
// most recent last, and each entry's messages oldest first.
func (s *Store) Overview(
	ctx context.Context, workspace int64, limit int,
) (api.ChatOverview, error) {
	v, err := s.begin(ctx, NewAccess(workspace), true)
	if err != nil {
		return api.ChatOverview{}, fmt.Errorf("reading chats: %w", err)
	}
	defer v.tx.Rollback()

	rows, err := v.tx.QueryContext(ctx, `
		SELECT `+messageColumns+` FROM entry_messages AS m
		WHERE m.workspace_id = ?
		ORDER BY m.post_order DESC LIMIT ?`, workspace, limit,
	)
	if err != nil {
		return api.ChatOverview{}, fmt.Errorf("reading chats: %w", err)
	}
	var newestFirst []api.Message
	for rows.Next() {
		message, err := s.scanMessage(rows)
		if err != nil {
			rows.Close()
			return api.ChatOverview{}, fmt.Errorf("reading chats: %w", err)
		}
		newestFirst = append(newestFirst, message)
	}
	if err := rows.Close(); err != nil {
		return api.ChatOverview{}, fmt.Errorf("reading chats: %w", err)
	}

	// Walking newest first meets each entry at its newest message, so
	// reversing the chats afterwards leaves the most recent last.
	overview := api.ChatOverview{Chats: []api.EntryChat{}}
	chatOf := map[api.Ref]int{}
	for _, message := range newestFirst {
		if _, ok := chatOf[message.Entry]; !ok {
			described, err := v.entryRef(ctx, s.mustKey(message.Entry))
			if err != nil {
				return api.ChatOverview{}, err
			}
			chatOf[message.Entry] = len(overview.Chats)
			overview.Chats = append(overview.Chats,
				api.EntryChat{Entry: described})
		}
		chat := &overview.Chats[chatOf[message.Entry]]
		chat.Messages = append(chat.Messages, message)
	}
	slices.Reverse(overview.Chats)
	for i := range overview.Chats {
		slices.Reverse(overview.Chats[i].Messages)
	}
	if err := v.tx.Commit(); err != nil {
		return api.ChatOverview{}, fmt.Errorf("finishing chat read: %w", err)
	}
	return overview, nil
}
