package concierge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// deskSchema is the desk half's tables and indexes, appended to InitSchema's
// statements so one boot call builds both halves. Every statement is IF NOT
// EXISTS, for the same reason the ticket tables' are: a host runs InitSchema at
// every boot over the same database. The unique indexes are the keys the
// store's refusals and claims rest on: one identity per contact, one ref per
// channel object, one slug per desk partition, one rating per ticket.
var deskSchema = []string{
	`CREATE TABLE IF NOT EXISTS concierge_contacts (
		id TEXT PRIMARY KEY, partition TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '', account_id TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_concierge_contacts_partition ON concierge_contacts(partition)`,
	`CREATE TABLE IF NOT EXISTS concierge_contact_identities (
		contact_id TEXT NOT NULL, partition TEXT NOT NULL, channel TEXT NOT NULL,
		external_id TEXT NOT NULL, verified_at INTEGER, created_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_contact_identities_key ON concierge_contact_identities(partition, channel, external_id)`,
	`CREATE INDEX IF NOT EXISTS idx_concierge_contact_identities_contact ON concierge_contact_identities(contact_id)`,
	`CREATE TABLE IF NOT EXISTS concierge_thread_refs (
		channel TEXT NOT NULL, external_id TEXT NOT NULL, partition TEXT NOT NULL,
		ticket_id TEXT NOT NULL, message_id TEXT NOT NULL DEFAULT '',
		contact_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_thread_refs_key ON concierge_thread_refs(channel, external_id)`,
	`CREATE TABLE IF NOT EXISTS concierge_teams (
		id TEXT PRIMARY KEY, partition TEXT NOT NULL, slug TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
		members TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_teams_slug ON concierge_teams(partition, slug)`,
	`CREATE TABLE IF NOT EXISTS concierge_queues (
		id TEXT PRIMARY KEY, partition TEXT NOT NULL, slug TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
		team_id TEXT NOT NULL DEFAULT '', sla_policy_id TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_queues_slug ON concierge_queues(partition, slug)`,
	`CREATE TABLE IF NOT EXISTS concierge_assignments (
		ticket_id TEXT PRIMARY KEY, partition TEXT NOT NULL, queue_id TEXT NOT NULL DEFAULT '',
		team_id TEXT NOT NULL DEFAULT '', assignee_id TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL DEFAULT 0, updated_by TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_concierge_assignments_queue ON concierge_assignments(queue_id)`,
	`CREATE TABLE IF NOT EXISTS concierge_sla_policies (
		id TEXT PRIMARY KEY, partition TEXT NOT NULL, slug TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
		first_response INTEGER NOT NULL DEFAULT 0, resolution INTEGER NOT NULL DEFAULT 0,
		hours TEXT NOT NULL DEFAULT '{}', pause_statuses TEXT NOT NULL DEFAULT '[]',
		created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_sla_policies_slug ON concierge_sla_policies(partition, slug)`,
	`CREATE TABLE IF NOT EXISTS concierge_ticket_slas (
		ticket_id TEXT PRIMARY KEY, partition TEXT NOT NULL, policy_id TEXT NOT NULL,
		started_at INTEGER NOT NULL DEFAULT 0, pauses TEXT NOT NULL DEFAULT '[]'
	)`,
	`CREATE TABLE IF NOT EXISTS concierge_ratings (
		id TEXT PRIMARY KEY, ticket_id TEXT NOT NULL, partition TEXT NOT NULL,
		score INTEGER NOT NULL, comment TEXT NOT NULL DEFAULT '', rated_by TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_concierge_ratings_ticket ON concierge_ratings(ticket_id)`,
	`CREATE INDEX IF NOT EXISTS idx_concierge_ratings_partition ON concierge_ratings(partition, created_at)`,
}

// execer is what an event insert runs through: the store's *sql.DB, or the
// transaction Assign writes the route and its event in together.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertEvent(ctx context.Context, x execer, e *TicketEvent) error {
	if e.ID == "" {
		e.ID = randomID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	_, err := x.ExecContext(ctx, `INSERT INTO concierge_events
		(id, ticket_id, event_type, actor_type, actor_id, old_value, new_value, metadata, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		e.ID, e.TicketID, e.EventType, e.ActorType, e.ActorID, e.OldValue, e.NewValue, e.Metadata, tnanos(e.CreatedAt))
	return err
}

// ── Contacts ──

const contactCols = `id, partition, name, email, account_id, metadata, created_at, updated_at`

func scanContact(sc interface{ Scan(...any) error }) (*Contact, error) {
	var c Contact
	var meta string
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.Partition, &c.Name, &c.Email, &c.AccountID, &meta, &created, &updated); err != nil {
		return nil, err
	}
	c.Metadata, c.CreatedAt, c.UpdatedAt = readStrMap(meta), fromNanos(created), fromNanos(updated)
	return &c, nil
}

func (s *SQLStore) CreateContact(ctx context.Context, c *Contact) error {
	if c.Partition == "" {
		return ErrEmptyPartition
	}
	id := c.ID
	if id == "" {
		id = randomID()
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_contacts (`+contactCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		id, c.Partition, c.Name, c.Email, c.AccountID, jsonStrMap(c.Metadata), tnanos(now), tnanos(now))
	if err != nil {
		var one int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM concierge_contacts WHERE id = ?`, id).Scan(&one) == nil {
			return ErrDuplicateID
		}
		return err
	}
	c.ID, c.CreatedAt, c.UpdatedAt = id, fromNanos(tnanos(now)), fromNanos(tnanos(now))
	return nil
}

func (s *SQLStore) GetContact(ctx context.Context, partition, id string) (*Contact, error) {
	c, err := scanContact(s.db.QueryRowContext(ctx, `SELECT `+contactCols+` FROM concierge_contacts
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) UpdateContact(ctx context.Context, c *Contact) error {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `UPDATE concierge_contacts SET name = ?, email = ?, account_id = ?, metadata = ?,
		updated_at = ? WHERE id = ? AND (? <> '' AND partition = ?)`,
		c.Name, c.Email, c.AccountID, jsonStrMap(c.Metadata), tnanos(now), c.ID, c.Partition, c.Partition)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	c.UpdatedAt = fromNanos(tnanos(now))
	return nil
}

func (s *SQLStore) LinkIdentity(ctx context.Context, id *ContactIdentity) error {
	if _, err := s.GetContact(ctx, id.Partition, id.ContactID); err != nil {
		return err
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_contact_identities
		(contact_id, partition, channel, external_id, verified_at, created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(partition, channel, external_id) DO NOTHING`,
		id.ContactID, id.Partition, id.Channel, id.ExternalID, ptrNanos(id.VerifiedAt), tnanos(now))
	if err != nil {
		return err
	}
	var holder string
	var created int64
	if err := s.db.QueryRowContext(ctx, `SELECT contact_id, created_at FROM concierge_contact_identities
		WHERE partition = ? AND channel = ? AND external_id = ?`, id.Partition, id.Channel, id.ExternalID).Scan(&holder, &created); err != nil {
		return err
	}
	if holder != id.ContactID {
		return ErrIdentityTaken
	}
	if id.VerifiedAt != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE concierge_contact_identities SET verified_at = ?
			WHERE partition = ? AND channel = ? AND external_id = ?`,
			ptrNanos(id.VerifiedAt), id.Partition, id.Channel, id.ExternalID); err != nil {
			return err
		}
	}
	id.CreatedAt = fromNanos(created)
	return nil
}

func (s *SQLStore) FindContactByIdentity(ctx context.Context, partition, channel, externalID string) (*Contact, error) {
	c, err := scanContact(s.db.QueryRowContext(ctx, `SELECT `+contactCols+` FROM concierge_contacts
		WHERE (? <> '' AND partition = ?) AND id = (SELECT contact_id FROM concierge_contact_identities
		WHERE partition = ? AND channel = ? AND external_id = ?)`, partition, partition, partition, channel, externalID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) ListContactIdentities(ctx context.Context, partition, contactID string) ([]*ContactIdentity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT contact_id, partition, channel, external_id, verified_at, created_at
		FROM concierge_contact_identities WHERE (? <> '' AND partition = ?) AND contact_id = ?
		ORDER BY channel, external_id`, partition, partition, contactID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*ContactIdentity
	for rows.Next() {
		var id ContactIdentity
		var verified sql.NullInt64
		var created int64
		if err := rows.Scan(&id.ContactID, &id.Partition, &id.Channel, &id.ExternalID, &verified, &created); err != nil {
			return nil, err
		}
		id.VerifiedAt, id.CreatedAt = fromNullNanos(verified), fromNanos(created)
		out = append(out, &id)
	}
	return out, rows.Err()
}

// ── Thread refs ──

const threadRefCols = `channel, external_id, partition, ticket_id, message_id, contact_id, created_at`

func (s *SQLStore) ClaimThreadRef(ctx context.Context, r *ThreadRef) (bool, error) {
	if !validThreadRef(r) {
		return false, ErrInvalidThreadRef
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO concierge_thread_refs (`+threadRefCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(channel, external_id) DO NOTHING`,
		r.Channel, r.ExternalID, r.Partition, r.TicketID, r.MessageID, r.ContactID, tnanos(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		r.CreatedAt = fromNanos(tnanos(now))
		return true, nil
	}
	have, err := s.GetThreadRef(ctx, r.Channel, r.ExternalID)
	if err != nil {
		return false, err
	}
	*r = *have
	return false, nil
}

func (s *SQLStore) GetThreadRef(ctx context.Context, channel, externalID string) (*ThreadRef, error) {
	var r ThreadRef
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT `+threadRefCols+` FROM concierge_thread_refs
		WHERE channel = ? AND external_id = ?`, channel, externalID).
		Scan(&r.Channel, &r.ExternalID, &r.Partition, &r.TicketID, &r.MessageID, &r.ContactID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = fromNanos(created)
	return &r, nil
}

// ── Teams, queues and assignment ──

func jsonStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func readStrings(s string) []string {
	var out []string
	if s == "" || s == "[]" {
		return nil
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// upsertBySlug inserts a row keyed (partition, slug) or, when the key is held,
// runs update over it, then reads back the stored id and creation time.
func (s *SQLStore) upsertBySlug(ctx context.Context, table, partition, slug, id string, insert func(id string) error, update func() error) (string, time.Time, error) {
	var have string
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, created_at FROM `+table+` WHERE partition = ? AND slug = ?`, partition, slug).Scan(&have, &created)
	switch {
	case err == nil:
		if err := update(); err != nil {
			return "", time.Time{}, err
		}
		return have, fromNanos(created), nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", time.Time{}, err
	}
	if id == "" {
		id = randomID()
	}
	if err := insert(id); err != nil {
		var one int
		if s.db.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one) == nil {
			return "", time.Time{}, ErrDuplicateID
		}
		return "", time.Time{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT created_at FROM `+table+` WHERE id = ?`, id).Scan(&created); err != nil {
		return "", time.Time{}, err
	}
	return id, fromNanos(created), nil
}

func (s *SQLStore) UpsertTeam(ctx context.Context, t *Team) error {
	if t.Partition == "" {
		return ErrEmptyPartition
	}
	now := tnanos(time.Now())
	id, created, err := s.upsertBySlug(ctx, "concierge_teams", t.Partition, t.Slug, t.ID,
		func(id string) error {
			_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_teams (id, partition, slug, name, members, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?)`, id, t.Partition, t.Slug, t.Name, jsonStrings(t.Members), now, now)
			return err
		},
		func() error {
			_, err := s.db.ExecContext(ctx, `UPDATE concierge_teams SET name = ?, members = ?, updated_at = ?
				WHERE partition = ? AND slug = ?`, t.Name, jsonStrings(t.Members), now, t.Partition, t.Slug)
			return err
		})
	if err != nil {
		return err
	}
	t.ID, t.CreatedAt, t.UpdatedAt = id, created, fromNanos(now)
	return nil
}

func (s *SQLStore) ListTeams(ctx context.Context, partition string) ([]*Team, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, partition, slug, name, members, created_at, updated_at
		FROM concierge_teams WHERE (? <> '' AND partition = ?) ORDER BY slug`, partition, partition)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*Team
	for rows.Next() {
		var t Team
		var members string
		var created, updated int64
		if err := rows.Scan(&t.ID, &t.Partition, &t.Slug, &t.Name, &members, &created, &updated); err != nil {
			return nil, err
		}
		t.Members, t.CreatedAt, t.UpdatedAt = readStrings(members), fromNanos(created), fromNanos(updated)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *SQLStore) UpsertQueue(ctx context.Context, q *Queue) error {
	if q.Partition == "" {
		return ErrEmptyPartition
	}
	now := tnanos(time.Now())
	id, created, err := s.upsertBySlug(ctx, "concierge_queues", q.Partition, q.Slug, q.ID,
		func(id string) error {
			_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_queues
				(id, partition, slug, name, team_id, sla_policy_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
				id, q.Partition, q.Slug, q.Name, q.TeamID, q.SLAPolicyID, now, now)
			return err
		},
		func() error {
			_, err := s.db.ExecContext(ctx, `UPDATE concierge_queues SET name = ?, team_id = ?, sla_policy_id = ?, updated_at = ?
				WHERE partition = ? AND slug = ?`, q.Name, q.TeamID, q.SLAPolicyID, now, q.Partition, q.Slug)
			return err
		})
	if err != nil {
		return err
	}
	q.ID, q.CreatedAt, q.UpdatedAt = id, created, fromNanos(now)
	return nil
}

func (s *SQLStore) ListQueues(ctx context.Context, partition string) ([]*Queue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, partition, slug, name, team_id, sla_policy_id, created_at, updated_at
		FROM concierge_queues WHERE (? <> '' AND partition = ?) ORDER BY slug`, partition, partition)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*Queue
	for rows.Next() {
		var q Queue
		var created, updated int64
		if err := rows.Scan(&q.ID, &q.Partition, &q.Slug, &q.Name, &q.TeamID, &q.SLAPolicyID, &created, &updated); err != nil {
			return nil, err
		}
		q.CreatedAt, q.UpdatedAt = fromNanos(created), fromNanos(updated)
		out = append(out, &q)
	}
	return out, rows.Err()
}

func (s *SQLStore) exists(ctx context.Context, table, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *SQLStore) Assign(ctx context.Context, a *Assignment, actorType, actorID string) error {
	owns, err := s.partitionOwnsTicket(ctx, a.Partition, a.TicketID)
	if err != nil {
		return err
	}
	if !owns {
		return ErrNotFound
	}
	for table, id := range map[string]string{"concierge_queues": a.QueueID, "concierge_teams": a.TeamID} {
		if id == "" {
			continue
		}
		ok, err := s.exists(ctx, table, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnknownRoute
		}
	}
	old, err := s.GetAssignment(ctx, a.Partition, a.TicketID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO concierge_assignments
		(ticket_id, partition, queue_id, team_id, assignee_id, updated_at, updated_by) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(ticket_id) DO UPDATE SET queue_id = excluded.queue_id, team_id = excluded.team_id,
		assignee_id = excluded.assignee_id, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		a.TicketID, a.Partition, a.QueueID, a.TeamID, a.AssigneeID, tnanos(now), actorID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := insertEvent(ctx, tx, &TicketEvent{TicketID: a.TicketID, EventType: EventAssignment, ActorType: actorType,
		ActorID: actorID, OldValue: assignmentValue(old), NewValue: assignmentValue(a), CreatedAt: now}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.UpdatedAt, a.UpdatedBy = fromNanos(tnanos(now)), actorID
	return nil
}

func (s *SQLStore) GetAssignment(ctx context.Context, partition, ticketID string) (*Assignment, error) {
	var a Assignment
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT ticket_id, partition, queue_id, team_id, assignee_id, updated_at, updated_by
		FROM concierge_assignments WHERE ticket_id = ? AND (? <> '' AND partition = ?)`, ticketID, partition, partition).
		Scan(&a.TicketID, &a.Partition, &a.QueueID, &a.TeamID, &a.AssigneeID, &updated, &a.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.UpdatedAt = fromNanos(updated)
	return &a, nil
}

// discloseAcross writes EventCrossPartitionQueue on every ticket in page before
// the page is returned.
func (s *SQLStore) discloseAcross(ctx context.Context, page []*Ticket, actorType, actorID, reason string) error {
	for _, t := range page {
		if err := insertEvent(ctx, s.db, &TicketEvent{TicketID: t.ID, EventType: EventCrossPartitionQueue,
			ActorType: actorType, ActorID: actorID, NewValue: t.Partition, Metadata: reason}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLStore) ListQueueTickets(ctx context.Context, actorType, actorID, reason, queueID string, limit, offset int) ([]*Ticket, error) {
	if err := requireCrossPartitionTerms(actorType, actorID, reason, limit); err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+ticketCols+` FROM concierge_tickets
		WHERE id IN (SELECT ticket_id FROM concierge_assignments WHERE queue_id = ?)
		AND status NOT IN ('closed', 'resolved') ORDER BY created_at, id LIMIT ? OFFSET ?`, queueID, limit, offset)
	if err != nil {
		return nil, err
	}
	page, err := collectTickets(rows)
	if err != nil {
		return nil, err
	}
	if err := s.discloseAcross(ctx, page, actorType, actorID, reason); err != nil {
		return nil, err
	}
	return page, nil
}

func collectTickets(rows *sql.Rows) ([]*Ticket, error) {
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

// ── Service levels ──

func (s *SQLStore) UpsertSLAPolicy(ctx context.Context, p *SLAPolicy) error {
	if p.Partition == "" {
		return ErrEmptyPartition
	}
	if !validSLAPolicy(p) {
		return ErrInvalidSLAPolicy
	}
	hours, err := json.Marshal(p.Hours)
	if err != nil {
		return err
	}
	now := tnanos(time.Now())
	id, created, err := s.upsertBySlug(ctx, "concierge_sla_policies", p.Partition, p.Slug, p.ID,
		func(id string) error {
			_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_sla_policies
				(id, partition, slug, name, first_response, resolution, hours, pause_statuses, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?)`, id, p.Partition, p.Slug, p.Name, int64(p.FirstResponse),
				int64(p.Resolution), string(hours), jsonStrings(p.PauseStatuses), now, now)
			return err
		},
		func() error {
			_, err := s.db.ExecContext(ctx, `UPDATE concierge_sla_policies SET name = ?, first_response = ?, resolution = ?,
				hours = ?, pause_statuses = ?, updated_at = ? WHERE partition = ? AND slug = ?`, p.Name, int64(p.FirstResponse),
				int64(p.Resolution), string(hours), jsonStrings(p.PauseStatuses), now, p.Partition, p.Slug)
			return err
		})
	if err != nil {
		return err
	}
	p.ID, p.CreatedAt, p.UpdatedAt = id, created, fromNanos(now)
	return nil
}

const slaPolicyCols = `id, partition, slug, name, first_response, resolution, hours, pause_statuses, created_at, updated_at`

func scanSLAPolicy(sc interface{ Scan(...any) error }) (*SLAPolicy, error) {
	var p SLAPolicy
	var first, resolution, created, updated int64
	var hours, pauses string
	if err := sc.Scan(&p.ID, &p.Partition, &p.Slug, &p.Name, &first, &resolution, &hours, &pauses, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(hours), &p.Hours); err != nil {
		return nil, err
	}
	p.FirstResponse, p.Resolution = time.Duration(first), time.Duration(resolution)
	p.PauseStatuses, p.CreatedAt, p.UpdatedAt = readStrings(pauses), fromNanos(created), fromNanos(updated)
	return &p, nil
}

func (s *SQLStore) GetSLAPolicy(ctx context.Context, id string) (*SLAPolicy, error) {
	p, err := scanSLAPolicy(s.db.QueryRowContext(ctx, `SELECT `+slaPolicyCols+` FROM concierge_sla_policies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *SQLStore) ListSLAPolicies(ctx context.Context, partition string) ([]*SLAPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+slaPolicyCols+` FROM concierge_sla_policies
		WHERE (? <> '' AND partition = ?) ORDER BY slug`, partition, partition)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*SLAPolicy
	for rows.Next() {
		p, err := scanSLAPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pauseRow is one SLAPause as the pauses column stores it, in Unix nanoseconds.
type pauseRow struct {
	From int64  `json:"from"`
	To   *int64 `json:"to,omitempty"`
}

func encodePauses(ps []SLAPause) string {
	rows := make([]pauseRow, len(ps))
	for i, p := range ps {
		rows[i].From = tnanos(p.From)
		if p.To != nil {
			to := tnanos(*p.To)
			rows[i].To = &to
		}
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func decodePauses(s string) ([]SLAPause, error) {
	var rows []pauseRow
	if err := json.Unmarshal([]byte(s), &rows); err != nil {
		return nil, err
	}
	out := make([]SLAPause, len(rows))
	for i, r := range rows {
		out[i].From = fromNanos(r.From)
		if r.To != nil {
			to := fromNanos(*r.To)
			out[i].To = &to
		}
	}
	return out, nil
}

func (s *SQLStore) StartSLA(ctx context.Context, partition, ticketID, policyID string, at time.Time) error {
	owns, err := s.partitionOwnsTicket(ctx, partition, ticketID)
	if err != nil {
		return err
	}
	ok, err := s.exists(ctx, "concierge_sla_policies", policyID)
	if err != nil {
		return err
	}
	if !owns || !ok {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO concierge_ticket_slas (ticket_id, partition, policy_id, started_at, pauses)
		VALUES (?,?,?,?,'[]') ON CONFLICT(ticket_id) DO UPDATE SET policy_id = excluded.policy_id,
		started_at = excluded.started_at, pauses = '[]'`, ticketID, partition, policyID, tnanos(at))
	return err
}

func (s *SQLStore) GetTicketSLA(ctx context.Context, partition, ticketID string) (*TicketSLA, error) {
	var t TicketSLA
	var started int64
	var pauses string
	err := s.db.QueryRowContext(ctx, `SELECT ticket_id, partition, policy_id, started_at, pauses FROM concierge_ticket_slas
		WHERE ticket_id = ? AND (? <> '' AND partition = ?)`, ticketID, partition, partition).
		Scan(&t.TicketID, &t.Partition, &t.PolicyID, &started, &pauses)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSLANotStarted
	}
	if err != nil {
		return nil, err
	}
	t.StartedAt = fromNanos(started)
	if t.Pauses, err = decodePauses(pauses); err != nil {
		return nil, err
	}
	return &t, nil
}

// rewritePauses applies fn to the ticket's pauses and stores the result when
// fn reports a change. A pause and a resume of one ticket from two callers at
// once can lose one of the two; the desk drives both from the one adapter that
// sees the ticket's status change.
func (s *SQLStore) rewritePauses(ctx context.Context, partition, ticketID string, fn func(*TicketSLA) bool) error {
	t, err := s.GetTicketSLA(ctx, partition, ticketID)
	if err != nil {
		return err
	}
	if !fn(t) {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE concierge_ticket_slas SET pauses = ? WHERE ticket_id = ? AND partition = ?`,
		encodePauses(t.Pauses), ticketID, partition)
	return err
}

func (s *SQLStore) PauseSLA(ctx context.Context, partition, ticketID string, at time.Time) error {
	return s.rewritePauses(ctx, partition, ticketID, func(t *TicketSLA) bool {
		if t.Paused() {
			return false
		}
		t.Pauses = append(t.Pauses, SLAPause{From: at})
		return true
	})
}

func (s *SQLStore) ResumeSLA(ctx context.Context, partition, ticketID string, at time.Time) error {
	return s.rewritePauses(ctx, partition, ticketID, func(t *TicketSLA) bool {
		if !t.Paused() {
			return false
		}
		to := at
		t.Pauses[len(t.Pauses)-1].To = &to
		return true
	})
}

func (s *SQLStore) ListSLABreaches(ctx context.Context, actorType, actorID, reason string, now time.Time, limit int) ([]*Ticket, error) {
	if err := requireCrossPartitionTerms(actorType, actorID, reason, limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+ticketCols+` FROM concierge_tickets
		WHERE id IN (SELECT ticket_id FROM concierge_ticket_slas) AND status NOT IN ('closed', 'resolved')
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	open, err := collectTickets(rows)
	if err != nil {
		return nil, err
	}
	policies := map[string]*SLAPolicy{}
	var page []*Ticket
	for _, t := range open {
		if len(page) == limit {
			break
		}
		running, err := s.GetTicketSLA(ctx, t.Partition, t.ID)
		if err != nil {
			return nil, err
		}
		p := policies[running.PolicyID]
		if p == nil {
			if p, err = s.GetSLAPolicy(ctx, running.PolicyID); errors.Is(err, ErrNotFound) {
				continue
			} else if err != nil {
				return nil, err
			}
			policies[running.PolicyID] = p
		}
		st, err := EvaluateSLA(p, running, t, now)
		if err != nil {
			return nil, err
		}
		if st.FirstResponseBreached || st.ResolutionBreached {
			page = append(page, t)
		}
	}
	if err := s.discloseAcross(ctx, page, actorType, actorID, reason); err != nil {
		return nil, err
	}
	return page, nil
}

// ── Ratings ──

func (s *SQLStore) RecordRating(ctx context.Context, r *Rating) error {
	if !validRating(r) {
		return ErrInvalidRating
	}
	owns, err := s.partitionOwnsTicket(ctx, r.Partition, r.TicketID)
	if err != nil {
		return err
	}
	if !owns {
		return ErrNotFound
	}
	now := time.Now()
	id := r.ID
	if id == "" {
		id = randomID()
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = now
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO concierge_ratings
		(id, ticket_id, partition, score, comment, rated_by, source, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ticket_id) DO UPDATE SET score = excluded.score, comment = excluded.comment,
		rated_by = excluded.rated_by, source = excluded.source, updated_at = excluded.updated_at`,
		id, r.TicketID, r.Partition, r.Score, r.Comment, r.RatedBy, r.Source, tnanos(created), tnanos(now)); err != nil {
		return err
	}
	stored, err := s.GetRating(ctx, r.Partition, r.TicketID)
	if err != nil {
		return err
	}
	r.ID, r.CreatedAt, r.UpdatedAt = stored.ID, stored.CreatedAt, stored.UpdatedAt
	return nil
}

func (s *SQLStore) GetRating(ctx context.Context, partition, ticketID string) (*Rating, error) {
	var r Rating
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id, ticket_id, partition, score, comment, rated_by, source, created_at, updated_at
		FROM concierge_ratings WHERE ticket_id = ? AND (? <> '' AND partition = ?)`, ticketID, partition, partition).
		Scan(&r.ID, &r.TicketID, &r.Partition, &r.Score, &r.Comment, &r.RatedBy, &r.Source, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt, r.UpdatedAt = fromNanos(created), fromNanos(updated)
	return &r, nil
}

func (s *SQLStore) SummariseRatings(ctx context.Context, partition string, since time.Time) (*RatingSummary, error) {
	// The zero time is before every stored stamp and outside UnixNano's range,
	// so it reads as no lower bound.
	lo := int64(math.MinInt64)
	if !since.IsZero() {
		lo = tnanos(since)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT score, COUNT(*) FROM concierge_ratings
		WHERE (? <> '' AND partition = ?) AND created_at >= ? GROUP BY score`, partition, partition, lo)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var sum RatingSummary
	total := 0
	for rows.Next() {
		var score, n int
		if err := rows.Scan(&score, &n); err != nil {
			return nil, err
		}
		if score >= MinRatingScore && score <= MaxRatingScore {
			sum.Distribution[score] += n
			sum.Count += n
			total += score * n
		}
	}
	if sum.Count > 0 {
		sum.Average = float64(total) / float64(sum.Count)
	}
	return &sum, rows.Err()
}
