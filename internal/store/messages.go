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

const messageColumns = `m.workspace_id, m.post_id, m.id, m.created_utc,
	m.modified_utc, m.revision, m.body, m.deleted, m.origin_user,
	m.origin_host, m.origin_dir, m.origin_claude_session`

// scanMessage reads the messageColumns.
func (s *Store) scanMessage(row rowScanner) (api.Message, error) {
	var message api.Message
	var key Key
	var created, modified string
	err := row.Scan(&key.Workspace, &key.ID, &message.Number, &created,
		&modified, &message.Revision, &message.Body, &message.Deleted, &message.Origin.User,
		&message.Origin.Host, &message.Origin.Dir,
		&message.Origin.ClaudeSession)
	if err != nil {
		return api.Message{}, err
	}
	message.Post = s.mustRef(key)
	if message.Created, err = parseTime(created); err != nil {
		return api.Message{}, err
	}
	if message.Modified, err = parseTime(modified); err != nil {
		return api.Message{}, err
	}
	return message, nil
}

func (s *Store) getMessage(
	ctx context.Context, tx *sql.Tx, post Key, number int64,
) (api.Message, error) {
	message, err := s.scanMessage(tx.QueryRowContext(ctx, `
		SELECT `+messageColumns+` FROM post_messages AS m
		WHERE m.workspace_id = ? AND m.post_id = ? AND m.id = ?`,
		post.Workspace, post.ID, number,
	))
	if errors.Is(err, sql.ErrNoRows) {
		ref := api.MessageRef{Post: s.mustRef(post), Number: number}
		return api.Message{}, fmt.Errorf("%w: %s", ErrMessageNotFound, ref)
	}
	if err != nil {
		return api.Message{}, fmt.Errorf("reading message: %w", err)
	}
	return message, nil
}

// MessageDetail returns one message of a post's chat, with the post and
// the path to it.
func (s *Store) MessageDetail(
	ctx context.Context, access Access, post Key, number int64,
) (api.MessageDetail, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.MessageDetail{}, fmt.Errorf("reading message: %w", err)
	}
	defer v.tx.Rollback()
	if err := v.checkPostExists(ctx, post); err != nil {
		return api.MessageDetail{}, err
	}

	message, err := s.getMessage(ctx, v.tx, post, number)
	if err != nil {
		return api.MessageDetail{}, err
	}
	detail := api.MessageDetail{Message: message}
	if detail.Post, err = v.postRef(ctx, post); err != nil {
		return api.MessageDetail{}, err
	}
	if detail.Ancestors, err = v.ancestors(ctx, post); err != nil {
		return api.MessageDetail{}, err
	}
	if err := v.tx.Commit(); err != nil {
		return api.MessageDetail{}, fmt.Errorf("finishing message read: %w",
			err)
	}
	return detail, nil
}

// recordChatActivity moves a post's activity time and revision to a
// change in its chat, in the transaction that makes the change.
func recordChatActivity(
	ctx context.Context, tx *sql.Tx, post Key, at string, revision int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE posts SET activity_utc = ?, activity_revision = ?
		WHERE workspace_id = ? AND id = ?`,
		at, revision, post.Workspace, post.ID,
	); err != nil {
		return fmt.Errorf("recording chat activity: %w", err)
	}
	return nil
}

// requestedMessage finds the number of the message a request with the
// same id sent in a post's chat, if any, and refuses an id whose
// message has another digest.
func (v view) requestedMessage(
	ctx context.Context, post Key, requestID string, digest int64,
) (int64, bool, error) {
	var number, stored int64
	err := v.tx.QueryRowContext(ctx, `
		SELECT id, request_digest FROM post_messages
		WHERE workspace_id = ? AND post_id = ? AND request_id = ?
			AND request_id <> ''`,
		post.Workspace, post.ID, requestID,
	).Scan(&number, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("finding the message of request "+
			"%q: %w", requestID, err)
	}
	if stored != digest {
		ref := api.MessageRef{Post: v.store.mustRef(post), Number: number}
		return 0, false, fmt.Errorf("%w: request id %q sent %s, with "+
			"other text", ErrRequestReused, requestID, ref)
	}
	return number, true, nil
}

// PostMessage adds a message to a post's chat, numbered one past the
// post's highest message, and reports whether it did. A request whose
// id already sent a message in the chat sends nothing, and returns that
// message. Writes are serialised through the store's one connection, so
// no other message can take the same number or request id.
func (s *Store) PostMessage(
	ctx context.Context, post Key, req api.PostMessageRequest,
) (api.Message, bool, error) {
	v, err := s.begin(ctx, NewAccess(post.Workspace), false)
	if err != nil {
		return api.Message{}, false, fmt.Errorf("posting message: %w", err)
	}
	defer v.tx.Rollback()
	if err := v.checkPostExists(ctx, post); err != nil {
		return api.Message{}, false, err
	}

	digest := messageDigest(req)
	if req.RequestID != "" {
		number, found, err := v.requestedMessage(ctx, post, req.RequestID,
			digest)
		if err != nil {
			return api.Message{}, false, err
		}
		if found {
			message, err := s.getMessage(ctx, v.tx, post, number)
			return message, false, err
		}
	}

	var number int64
	if err := v.tx.QueryRowContext(ctx, `
		SELECT coalesce(max(id), 0) + 1 FROM post_messages
		WHERE workspace_id = ? AND post_id = ?`,
		post.Workspace, post.ID,
	).Scan(&number); err != nil {
		return api.Message{}, false, fmt.Errorf("numbering message: %w", err)
	}
	revision, err := nextRevision(ctx, v.tx)
	if err != nil {
		return api.Message{}, false, err
	}
	now := nowUTC()
	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO post_messages (
			workspace_id, post_id, id, created_utc, modified_utc,
			revision, posted_revision, body, origin_user, origin_host,
			origin_dir, origin_claude_session, request_id, request_digest
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		post.Workspace, post.ID, number, now, now, revision, revision,
		req.Body,
		req.Origin.User, req.Origin.Host, req.Origin.Dir,
		req.Origin.ClaudeSession, req.RequestID, digest,
	); err != nil {
		return api.Message{}, false, fmt.Errorf("posting message: %w", err)
	}
	if err := recordChatActivity(ctx, v.tx, post, now,
		revision); err != nil {
		return api.Message{}, false, err
	}
	message, err := s.getMessage(ctx, v.tx, post, number)
	if err != nil {
		return api.Message{}, false, err
	}
	if err := v.tx.Commit(); err != nil {
		return api.Message{}, false, fmt.Errorf("committing message: %w",
			err)
	}
	return message, true, nil
}

// overwriteMessage writes a message's text and whether it is deleted,
// as one change.
func overwriteMessage(
	ctx context.Context, tx *sql.Tx, post Key, number int64, body string,
	deleted bool,
) error {
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		return err
	}
	now := nowUTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE post_messages
		SET body = ?, deleted = ?, modified_utc = ?, revision = ?
		WHERE workspace_id = ? AND post_id = ? AND id = ?`,
		body, deleted, now, revision, post.Workspace, post.ID, number,
	); err != nil {
		return fmt.Errorf("overwriting message: %w", err)
	}
	return recordChatActivity(ctx, tx, post, now, revision)
}

// EditMessage overwrites a message's text, and reports whether it
// changed. The message keeps its number and its place in the chat, and
// no earlier text is kept. Text that equals the current text changes
// nothing, so repeating an edit is harmless. Only the user who posted
// the message can edit it, and nobody can edit a deleted message.
func (s *Store) EditMessage(
	ctx context.Context, post Key, number int64, req api.EditMessageRequest,
) (api.Message, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, false, fmt.Errorf("editing message: %w", err)
	}
	defer tx.Rollback()
	message, err := s.getMessage(ctx, tx, post, number)
	if err != nil {
		return api.Message{}, false, err
	}
	if err := checkEditor(message.Ref(), message.Origin.User,
		req.User); err != nil {
		return api.Message{}, false, err
	}
	if message.Deleted {
		return api.Message{}, false,
			&DeletedError{deleted: message.Ref().String()}
	}
	if message.Body == req.Body {
		return message, false, nil
	}

	if err := overwriteMessage(ctx, tx, post, number, req.Body,
		false); err != nil {
		return api.Message{}, false, err
	}
	message, err = s.getMessage(ctx, tx, post, number)
	if err != nil {
		return api.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, false, fmt.Errorf(
			"committing message edit: %w", err)
	}
	return message, true, nil
}

// DeleteMessage clears a message's text and marks it deleted, and no
// earlier text is kept. The message keeps its number and its place in
// the chat. Deleting a deleted message changes nothing. Only the user
// who posted the message can delete it.
func (s *Store) DeleteMessage(
	ctx context.Context, post Key, number int64, req api.DeleteRequest,
) (api.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, fmt.Errorf("deleting message: %w", err)
	}
	defer tx.Rollback()
	message, err := s.getMessage(ctx, tx, post, number)
	if err != nil {
		return api.Message{}, err
	}
	if err := checkEditor(message.Ref(), message.Origin.User,
		req.User); err != nil {
		return api.Message{}, err
	}
	if message.Deleted {
		return message, nil
	}

	if err := overwriteMessage(ctx, tx, post, number, "",
		true); err != nil {
		return api.Message{}, err
	}
	message, err = s.getMessage(ctx, tx, post, number)
	if err != nil {
		return api.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Message{}, fmt.Errorf("committing message delete: %w",
			err)
	}
	return message, nil
}

// readChatPage reads the messages a query selects into a page, oldest
// first, with the latest revision the read saw. The query reads at most
// limit messages after since, so a full page ends at its last message,
// and a page short of it has checked every message after since, so its
// cursor passes them all. Remaining counts the messages after the
// cursor.
func (s *Store) readChatPage(
	ctx context.Context,
	tx *sql.Tx,
	post Key,
	since int64,
	limit int,
	query string,
	args ...any,
) (api.ChatPage, error) {
	revision, err := revisionSeen(ctx, tx)
	if err != nil {
		return api.ChatPage{}, err
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
	}
	defer rows.Close()
	page := api.ChatPage{Messages: []api.Message{}, Revision: revision}
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
			SELECT coalesce(max(id), 0) FROM post_messages
			WHERE workspace_id = ? AND post_id = ?`,
			post.Workspace, post.ID,
		).Scan(&last); err != nil {
			return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
		}
		page.Cursor = max(since, last)
		return page, nil
	}
	page.Cursor = page.Messages[len(page.Messages)-1].Number
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM post_messages
		WHERE workspace_id = ? AND post_id = ? AND id > ?`,
		post.Workspace, post.ID, page.Cursor,
	).Scan(&page.Remaining); err != nil {
		return api.ChatPage{}, fmt.Errorf("counting messages: %w", err)
	}
	return page, nil
}

// Messages returns up to limit messages of a post's chat from the bound
// after on that match a filter, oldest first, leaving out those whose
// origin has the key exclude, when there is one. A message matches as
// buildMessageWhere has it.
//
// The page's cursor is the highest message number the read checked.
// Every message up to it is in the page or did not match, so a read
// from the cursor misses none, even when the page is empty.
func (s *Store) Messages(
	ctx context.Context,
	post Key,
	after api.ChatBound,
	filter Filter,
	exclude *OriginKey,
	limit int,
) (api.ChatPage, error) {
	where, err := buildMessageWhere(filter)
	if err != nil {
		return api.ChatPage{}, err
	}
	where.excludeOrigin("m", exclude)
	where.addSpan(`m.created_utc`, Span{After: after.Time})
	where.clauses = append(where.clauses, `m.workspace_id = ?`,
		`m.post_id = ?`, `m.id > ?`)
	where.args = append(where.args, post.Workspace, post.ID, after.Number)

	tx, err := s.reads.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
	}
	defer tx.Rollback()
	v := view{store: s, tx: tx}
	if err := v.checkPostExists(ctx, post); err != nil {
		return api.ChatPage{}, err
	}
	return s.readChatPage(ctx, tx, post, after.Number, limit, `
		SELECT `+messageColumns+`
		FROM post_messages AS m CROSS JOIN posts AS e
			ON e.workspace_id = m.workspace_id AND e.id = m.post_id`+
		where.sql()+`
		ORDER BY m.id LIMIT ?`, append(where.args, limit)...)
}

// LatestMessages returns the newest count messages of a post's chat,
// oldest first.
func (s *Store) LatestMessages(
	ctx context.Context, post Key, count int,
) (api.ChatPage, error) {
	tx, err := s.reads.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.ChatPage{}, fmt.Errorf("reading chat: %w", err)
	}
	defer tx.Rollback()
	v := view{store: s, tx: tx}
	if err := v.checkPostExists(ctx, post); err != nil {
		return api.ChatPage{}, err
	}
	return s.readChatPage(ctx, tx, post, 0, count, `
		SELECT `+messageColumns+` FROM post_messages AS m
		WHERE m.workspace_id = ? AND m.post_id = ?
		ORDER BY m.id DESC LIMIT ?`,
		post.Workspace, post.ID, count)
}

// Overview returns the newest limit messages across a workspace,
// grouped by post. Posts come in order of their newest message, the
// most recent last, and each post's messages oldest first.
func (s *Store) Overview(
	ctx context.Context, workspace int64, limit int,
) (api.ChatOverview, error) {
	v, err := s.begin(ctx, NewAccess(workspace), true)
	if err != nil {
		return api.ChatOverview{}, fmt.Errorf("reading chats: %w", err)
	}
	defer v.tx.Rollback()
	overview := api.ChatOverview{Chats: []api.PostChat{}}
	if overview.Revision, err = revisionSeen(ctx, v.tx); err != nil {
		return api.ChatOverview{}, err
	}

	rows, err := v.tx.QueryContext(ctx, `
		SELECT `+messageColumns+` FROM post_messages AS m
		WHERE m.workspace_id = ?
		ORDER BY m.posted_revision DESC LIMIT ?`, workspace, limit,
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

	// Walking newest first meets each post at its newest message, so
	// reversing the chats afterwards leaves the most recent last.
	chatOf := map[api.Ref]int{}
	for _, message := range newestFirst {
		if _, ok := chatOf[message.Post]; !ok {
			described, err := v.postRef(ctx, s.mustKey(message.Post))
			if err != nil {
				return api.ChatOverview{}, err
			}
			chatOf[message.Post] = len(overview.Chats)
			overview.Chats = append(overview.Chats,
				api.PostChat{Post: described})
		}
		chat := &overview.Chats[chatOf[message.Post]]
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
