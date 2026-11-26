package concierge

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// refMember is the JSON path of one ExternalRefs key as a single member of the
// object, its name quoted so that a dotted key such as "discord.channel" names
// that member and not a "channel" member of a nested "discord" object. It takes
// the key as one bound parameter.
const refMember = `'$."' || ? || '"'`

// refNested is the unquoted path of a dotted key, the nested member an earlier
// write of the key produced; reads and writes also address it while such rows
// remain. It takes the key as one bound parameter, and is used only for a key
// nestedRefKey admits, whose unquoted path is well formed.
const refNested = `'$.' || ?`

// nestedRefKey reports whether key is a dotted key whose unquoted JSON path
// parses as nested members.
var nestedRefKey = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)+$`).MatchString

// validRefKey reports whether key can name an ExternalRefs member: non-empty,
// and without the double quote that would end its quoted path.
func validRefKey(key string) bool { return key != "" && !strings.Contains(key, `"`) }

// SQLStore is the durable default Store over any database/sql driver — the shipped standalone store.
// The package imports only database/sql (stdlib); the APP supplies the opened *sql.DB and registers
// the driver (go-libsql/Turso, modernc, Postgres), so concierge pulls no driver dependency yet works
// out of the box. Partition is an opaque column the store filters on but never interprets; an empty
// partition on a partition-scoped read matches nothing, and a caller that needs a ticket whose
// partition it does not know goes through ResolveTicketPartition, which records the request. Times are
// stored as INTEGER Unix nanoseconds; a NULL means "not yet" for a *time.Time field. The Store
// interface remains the escape-hatch for a mesh-gateway or the in-memory MemStore.
type SQLStore struct{ db *sql.DB }

// NewSQLStore builds a store over an opened *sql.DB. Call InitSchema once before use.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// InitSchema creates every table + index (idempotent); run once at boot.
func (s *SQLStore) InitSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS concierge_tickets (
			id TEXT PRIMARY KEY, number TEXT NOT NULL DEFAULT '', partition TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT '',
			subject TEXT NOT NULL DEFAULT '', category TEXT NOT NULL DEFAULT '',
			priority TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '',
			form_data TEXT NOT NULL DEFAULT '{}', group_id TEXT NOT NULL DEFAULT '',
			app_id TEXT NOT NULL DEFAULT '', external_refs TEXT NOT NULL DEFAULT '{}',
			source TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '',
			ip_address TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0, confirmed_at INTEGER, first_response_at INTEGER,
			resolved_at INTEGER, closed_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_concierge_tickets_partition ON concierge_tickets(partition)`,
		`CREATE INDEX IF NOT EXISTS idx_concierge_tickets_user ON concierge_tickets(partition, user_id)`,
		`CREATE TABLE IF NOT EXISTS concierge_messages (
			id TEXT PRIMARY KEY, ticket_id TEXT NOT NULL, author_type TEXT NOT NULL DEFAULT '',
			author_id TEXT NOT NULL DEFAULT '', author_name TEXT NOT NULL DEFAULT '',
			author_email TEXT NOT NULL DEFAULT '', content TEXT NOT NULL DEFAULT '',
			content_html TEXT NOT NULL DEFAULT '', external_refs TEXT NOT NULL DEFAULT '{}',
			synced TEXT NOT NULL DEFAULT '{}', is_internal INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_concierge_messages_ticket ON concierge_messages(ticket_id)`,
		`CREATE TABLE IF NOT EXISTS concierge_events (
			id TEXT PRIMARY KEY, ticket_id TEXT NOT NULL, event_type TEXT NOT NULL DEFAULT '',
			actor_type TEXT NOT NULL DEFAULT '', actor_id TEXT NOT NULL DEFAULT '',
			old_value TEXT NOT NULL DEFAULT '', new_value TEXT NOT NULL DEFAULT '',
			metadata TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_concierge_events_ticket ON concierge_events(ticket_id)`,
		`CREATE TABLE IF NOT EXISTS concierge_attachments (
			id TEXT PRIMARY KEY, ticket_id TEXT NOT NULL DEFAULT '', message_id TEXT NOT NULL DEFAULT '',
			filename TEXT NOT NULL DEFAULT '', content_type TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL DEFAULT 0, storage_key TEXT NOT NULL DEFAULT '',
			storage_url TEXT NOT NULL DEFAULT '', uploaded_by TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_concierge_attachments_ticket ON concierge_attachments(ticket_id)`,
		`CREATE TABLE IF NOT EXISTS concierge_kb_categories (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
			slug TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
			parent_id TEXT NOT NULL DEFAULT '', sort_order INTEGER NOT NULL DEFAULT 0,
			article_count INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_concierge_kb_categories_partition ON concierge_kb_categories(partition)`,
		`CREATE TABLE IF NOT EXISTS concierge_kb_articles (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '',
			slug TEXT NOT NULL DEFAULT '', content TEXT NOT NULL DEFAULT '',
			content_html TEXT NOT NULL DEFAULT '', category_id TEXT NOT NULL DEFAULT '',
			category_name TEXT NOT NULL DEFAULT '', category_slug TEXT NOT NULL DEFAULT '',
			author_id TEXT NOT NULL DEFAULT '', author_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '', view_count INTEGER NOT NULL DEFAULT 0,
			helpful_count INTEGER NOT NULL DEFAULT 0, metadata TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_concierge_kb_articles_partition ON concierge_kb_articles(partition)`,
		// The slug keys CreateCategory and CreateArticle refuse a duplicate of and
		// UpsertCategory and UpsertArticle name as their ON CONFLICT target. They
		// are indexes rather than table constraints because a host runs InitSchema
		// at every boot over the same database file, and CREATE TABLE IF NOT EXISTS
		// leaves a table that already exists as it is. CREATE UNIQUE INDEX IF NOT
		// EXISTS reaches a fresh table and an existing one that holds no two rows
		// under one key; over a table that does, the index is not built and
		// InitSchema returns the engine's constraint error.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_kb_categories_slug ON concierge_kb_categories(partition, slug)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_kb_articles_slug ON concierge_kb_articles(partition, category_slug, slug)`,
	}
	for _, q := range append(stmts, deskSchema...) {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// ── time / json codecs ──

func tnanos(t time.Time) int64 { return t.UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func ptrNanos(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func fromNullNanos(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n.Int64)
	return &t
}

func jsonStrMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func jsonBoolMap(m map[string]bool) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func jsonAnyMap(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// readStrMap decodes a JSON object of string values. A member whose value is an
// object, as an earlier write of a dotted ExternalRefs key nested it, is read
// back under its dotted name ("discord": {"channel": "x"} as "discord.channel"),
// and a member stored under the dotted name itself wins over the nested one.
func readStrMap(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal([]byte(s), &raw) != nil {
		return nil
	}
	m := map[string]string{}
	flattenNested(m, "", raw)
	for k, v := range raw {
		if str, ok := v.(string); ok {
			m[k] = str
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// flattenNested copies the string members of raw's nested objects into m under
// their dotted names.
func flattenNested(m map[string]string, prefix string, raw map[string]any) {
	for k, v := range raw {
		switch val := v.(type) {
		case map[string]any:
			flattenNested(m, prefix+k+".", val)
		case string:
			if prefix != "" {
				m[prefix+k] = val
			}
		}
	}
}

func readBoolMap(s string) map[string]bool {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]bool
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func readAnyMap(s string) map[string]any {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// ── Tickets ──

const ticketCols = `id, number, partition, user_id, email, subject, category, priority, status, form_data,
	group_id, app_id, external_refs, source, user_agent, ip_address, created_at, updated_at,
	confirmed_at, first_response_at, resolved_at, closed_at`

func scanTicket(sc interface{ Scan(...any) error }) (*Ticket, error) {
	var t Ticket
	var formData, extRefs string
	var created, updated int64
	var confirmed, firstResp, resolved, closed sql.NullInt64
	if err := sc.Scan(&t.ID, &t.Number, &t.Partition, &t.UserID, &t.Email, &t.Subject, &t.Category,
		&t.Priority, &t.Status, &formData, &t.GroupID, &t.AppID, &extRefs, &t.Source,
		&t.UserAgent, &t.IPAddress, &created, &updated, &confirmed, &firstResp, &resolved, &closed); err != nil {
		return nil, err
	}
	t.FormData = readAnyMap(formData)
	t.ExternalRefs = readStrMap(extRefs)
	t.CreatedAt = fromNanos(created)
	t.UpdatedAt = fromNanos(updated)
	t.ConfirmedAt = fromNullNanos(confirmed)
	t.FirstResponseAt = fromNullNanos(firstResp)
	t.ResolvedAt = fromNullNanos(resolved)
	t.ClosedAt = fromNullNanos(closed)
	return &t, nil
}

// CreateTicket inserts t under its id, minting one when t names none. An insert
// the engine refuses while a ticket holds that id answers ErrDuplicateID, the
// sentinel MemStore returns for the same write, rather than the driver's
// constraint text: the id is the table's only key, and reading the row back
// rather than the error's wording keeps the answer the same under any driver. Any
// other refusal returns the engine's error. A refused insert changes nothing, and
// the id, number and times are written back to t only once the row is stored.
func (s *SQLStore) CreateTicket(ctx context.Context, t *Ticket) error {
	if t.Partition == "" {
		return ErrEmptyPartition
	}
	id := t.ID
	if id == "" {
		id = randomID()
	}
	number := t.Number
	if number == "" {
		number = ticketNumber(id)
	}
	now := time.Now()
	created := t.CreatedAt
	if created.IsZero() {
		created = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_tickets (`+ticketCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, number, t.Partition, t.UserID, t.Email, t.Subject, t.Category, t.Priority, t.Status,
		jsonAnyMap(t.FormData), t.GroupID, t.AppID, jsonStrMap(t.ExternalRefs), t.Source,
		t.UserAgent, t.IPAddress, tnanos(created), tnanos(now), ptrNanos(t.ConfirmedAt),
		ptrNanos(t.FirstResponseAt), ptrNanos(t.ResolvedAt), ptrNanos(t.ClosedAt))
	if err != nil {
		var one int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM concierge_tickets WHERE id = ?`, id).Scan(&one) == nil {
			return ErrDuplicateID
		}
		return err
	}
	t.ID, t.Number, t.CreatedAt, t.UpdatedAt = id, number, created, now
	return nil
}

func (s *SQLStore) GetTicket(ctx context.Context, partition, id string) (*Ticket, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ticketCols+` FROM concierge_tickets
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition)
	t, err := scanTicket(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return t, err
}

func (s *SQLStore) GetTicketByExternalRef(ctx context.Context, refKey, refValue string) (*Ticket, error) {
	if refValue == "" || !validRefKey(refKey) {
		return nil, ErrNotFound
	}
	// external_refs is a JSON object; match the exact key:value pair. json_extract keeps the store
	// free of a side table while the adapter chooses the opaque refKey.
	where, args := `json_extract(external_refs, `+refMember+`) = ?`, []any{refKey, refValue}
	if nestedRefKey(refKey) {
		where += ` OR json_extract(external_refs, ` + refNested + `) = ?`
		args = append(args, refKey, refValue)
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+ticketCols+` FROM concierge_tickets
		WHERE `+where+` LIMIT 1`, args...)
	t, err := scanTicket(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return t, err
}

func (s *SQLStore) queryTickets(ctx context.Context, where string, args ...any) ([]*Ticket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ticketCols+` FROM concierge_tickets WHERE `+where+
		` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLStore) ListTicketsByUser(ctx context.Context, partition, userID, statusFilter string) ([]*Ticket, error) {
	return s.queryTickets(ctx, `(? <> '' AND partition = ?) AND user_id = ? AND (? = '' OR status = ?)`,
		partition, partition, userID, statusFilter, statusFilter)
}

func (s *SQLStore) ListTicketsByEmail(ctx context.Context, partition, email string) ([]*Ticket, error) {
	return s.queryTickets(ctx, `(? <> '' AND partition = ?) AND lower(email) = lower(?)`, partition, partition, email)
}

func (s *SQLStore) ListTicketsByGroup(ctx context.Context, partition, groupID, statusFilter string) ([]*Ticket, error) {
	return s.queryTickets(ctx, `(? <> '' AND partition = ?) AND group_id = ? AND (? = '' OR status = ?)`,
		partition, partition, groupID, statusFilter, statusFilter)
}

func (s *SQLStore) ListAllTickets(ctx context.Context, actorType, actorID, reason, statusFilter, categoryFilter string, limit, offset int) ([]*Ticket, int, error) {
	if actorType == "" || actorID == "" || reason == "" {
		return nil, 0, ErrActorRequired
	}
	if limit <= 0 {
		return nil, 0, ErrUnboundedAdminList
	}
	where := `(? = '' OR status = ?) AND (? = '' OR category = ?)`
	args := []any{statusFilter, statusFilter, categoryFilter, categoryFilter}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM concierge_tickets WHERE `+where, args...).
		Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT ` + ticketCols + ` FROM concierge_tickets WHERE ` + where +
		` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer closeRows(rows)
	var out []*Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	// Written before the page is returned, and one per ticket, so a partition's own
	// event log names every cross-partition read that reached one of its tickets.
	for _, t := range out {
		if err := s.AddTicketEvent(ctx, &TicketEvent{
			TicketID:  t.ID,
			EventType: EventCrossPartitionList,
			ActorType: actorType,
			ActorID:   actorID,
			NewValue:  t.Partition,
			Metadata:  reason,
		}); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

func (s *SQLStore) ListBreachedTickets(ctx context.Context, firstResponse, resolution time.Duration, now time.Time) ([]*Ticket, error) {
	// A breach is an open (not-closed) ticket whose age passes an unmet first-response or resolution
	// threshold; a zero duration disables that leg. Age is now - created_at, both in Unix nanos.
	nowN := now.UnixNano()
	return s.queryTickets(ctx,
		`closed_at IS NULL AND (
			(first_response_at IS NULL AND ? > 0 AND (? - created_at) > ?) OR
			(resolved_at IS NULL AND ? > 0 AND (? - created_at) > ?))`,
		int64(firstResponse), nowN, int64(firstResponse),
		int64(resolution), nowN, int64(resolution))
}

func (s *SQLStore) touchTicket(ctx context.Context, partition, id, setClause string, args ...any) error {
	full := append(args, time.Now().UnixNano(), id, partition, partition)
	res, err := s.db.ExecContext(ctx, `UPDATE concierge_tickets SET `+setClause+`, updated_at = ?
		WHERE id = ? AND (? <> '' AND partition = ?)`, full...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// touchTicketUnscoped updates a ticket without a partition predicate. Only the two
// Store methods that take no partition use it — SetFirstResponseAt, stamping an SLA
// clock, and SetExternalRef, linking a channel ref the ticket's partition did not
// supply. Each acts on a ticket the caller already has an id for rather than
// searching, so neither widens what a caller can reach.
//
// It is separate from touchTicket because that one's predicate now requires a
// non-empty partition: passing "" through it would restore an unset partition
// meaning every partition, which is the property being removed. MemStore's
// mutateTicketUnscoped is the same split, so the two stores stay comparable.
func (s *SQLStore) touchTicketUnscoped(ctx context.Context, id, setClause string, args ...any) error {
	full := append(args, time.Now().UnixNano(), id)
	res, err := s.db.ExecContext(ctx, `UPDATE concierge_tickets SET `+setClause+`, updated_at = ?
		WHERE id = ?`, full...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) UpdateTicketStatus(ctx context.Context, partition, id, status string) error {
	// Reaching a terminal status stamps its timestamp once (COALESCE keeps a prior stamp).
	set := `status = ?`
	args := []any{status}
	switch status {
	case "resolved":
		set += `, resolved_at = COALESCE(resolved_at, ?)`
		args = append(args, time.Now().UnixNano())
	case "closed":
		set += `, closed_at = COALESCE(closed_at, ?)`
		args = append(args, time.Now().UnixNano())
	}
	return s.touchTicket(ctx, partition, id, set, args...)
}

func (s *SQLStore) UpdateTicketPriority(ctx context.Context, partition, id, priority string) error {
	return s.touchTicket(ctx, partition, id, `priority = ?`, priority)
}

func (s *SQLStore) SetFirstResponseAt(ctx context.Context, id string, at time.Time) error {
	return s.touchTicketUnscoped(ctx, id, `first_response_at = COALESCE(first_response_at, ?)`, at.UnixNano())
}

func (s *SQLStore) SetExternalRef(ctx context.Context, ticketID, refKey, refValue string) error {
	if !validRefKey(refKey) {
		return ErrInvalidRefKey
	}
	// Mutate the JSON in-DB so concurrent adapters don't clobber each other's keys. An empty value
	// removes the key (json_remove), matching the MemStore delete-on-empty contract. A dotted key's
	// nested member, written before keys were quoted, is removed in the same statement, so the row
	// holds the key once.
	refs, args := `external_refs`, []any{}
	if nestedRefKey(refKey) {
		refs, args = `json_remove(external_refs, `+refNested+`)`, append(args, refKey)
	}
	if refValue == "" {
		return s.touchTicketUnscoped(ctx, ticketID, `external_refs = json_remove(`+refs+`, `+refMember+`)`, append(args, refKey)...)
	}
	return s.touchTicketUnscoped(ctx, ticketID, `external_refs = json_set(`+refs+`, `+refMember+`, ?)`, append(args, refKey, refValue)...)
}

func (s *SQLStore) GetTicketStats(ctx context.Context, partition, userID string) (*TicketStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM concierge_tickets
		WHERE (? <> '' AND partition = ?) AND (? = '' OR user_id = ?) GROUP BY status`,
		partition, partition, userID, userID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var st TicketStats
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		st.Total += n
		switch status {
		case "closed", "resolved":
			st.Closed += n
		case "waiting":
			st.Waiting += n
		default:
			st.Open += n
		}
	}
	return &st, rows.Err()
}

// closeRows is the deferred Close of every query's rows in this package.
// database/sql closes rows itself once Next reports the end, so a normal return
// leaves nothing to close, and an early return already carries the error that
// stopped the read; its result is dropped.
func closeRows(rows *sql.Rows) { _ = rows.Close() }
