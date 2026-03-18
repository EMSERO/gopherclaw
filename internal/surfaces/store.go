package surfaces

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/eidetic"
)

// Store handles all database operations for surfaces.
type Store struct {
	pool           *pgxpool.Pool
	eidetic        eidetic.Client
	logger         *zap.SugaredLogger
	OnEideticWrite func() // called after successful Eidetic write-back; optional
}

// NewStore creates a new Store backed by pool.
func NewStore(pool *pgxpool.Pool, eideticClient eidetic.Client, logger *zap.SugaredLogger) *Store {
	return &Store{
		pool:    pool,
		eidetic: eideticClient,
		logger:  logger,
	}
}

// Migrate creates the agenticme_surfaces table if it doesn't exist.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS agenticme_surfaces (
			id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			content           TEXT NOT NULL,
			surface_type      TEXT NOT NULL DEFAULT 'insight'
			                  CHECK (surface_type IN ('insight','question','warning','reminder','connection')),
			priority          INT NOT NULL DEFAULT 3
			                  CHECK (priority BETWEEN 1 AND 5),
			status            TEXT NOT NULL DEFAULT 'active'
			                  CHECK (status IN ('active','dismissed','answered','expired','acted')),
			related_entry_ids UUID[] DEFAULT '{}',
			tags              TEXT[] DEFAULT '{}',
			user_response     TEXT,
			responded_at      TIMESTAMPTZ,
			reasoning_cycle   UUID,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
			expired_at        TIMESTAMPTZ,
			trigger_at        TIMESTAMPTZ
		);

		-- Add trigger_at column if missing (for existing installs).
		ALTER TABLE agenticme_surfaces ADD COLUMN IF NOT EXISTS trigger_at TIMESTAMPTZ;
		CREATE INDEX IF NOT EXISTS agenticme_surfaces_trigger_idx
			ON agenticme_surfaces (trigger_at)
			WHERE status = 'active' AND trigger_at IS NOT NULL;

		CREATE INDEX IF NOT EXISTS agenticme_surfaces_active_idx
			ON agenticme_surfaces (status, priority, created_at DESC)
			WHERE status = 'active';
		CREATE INDEX IF NOT EXISTS agenticme_surfaces_created_idx
			ON agenticme_surfaces (created_at DESC);
		CREATE INDEX IF NOT EXISTS agenticme_surfaces_updated_idx
			ON agenticme_surfaces (updated_at DESC);

		CREATE OR REPLACE FUNCTION agenticme_surfaces_set_updated_at()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			NEW.updated_at := now();
			RETURN NEW;
		END;
		$$;

		DROP TRIGGER IF EXISTS agenticme_surfaces_updated_at ON agenticme_surfaces;
		CREATE TRIGGER agenticme_surfaces_updated_at
			BEFORE UPDATE ON agenticme_surfaces
			FOR EACH ROW EXECUTE FUNCTION agenticme_surfaces_set_updated_at();

		CREATE TABLE IF NOT EXISTS agenticme_surface_messages (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			surface_id UUID NOT NULL REFERENCES agenticme_surfaces(id) ON DELETE CASCADE,
			role       TEXT NOT NULL CHECK (role IN ('user','assistant')),
			content    TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE INDEX IF NOT EXISTS agenticme_surface_messages_surface_idx
			ON agenticme_surface_messages (surface_id, created_at ASC);
	`)
	if err != nil {
		return fmt.Errorf("migrate surfaces: %w", err)
	}
	return nil
}

// Create inserts a new surface and returns it.
func (s *Store) Create(ctx context.Context, req CreateRequest) (*Surface, error) {
	if req.RelatedEntryIDs == nil {
		req.RelatedEntryIDs = []uuid.UUID{}
	}
	if req.Tags == nil {
		req.Tags = []string{}
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO agenticme_surfaces
			(content, surface_type, priority, related_entry_ids, tags, reasoning_cycle, trigger_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, content, surface_type, priority, status,
		          related_entry_ids, tags, user_response, responded_at,
		          reasoning_cycle, trigger_at, created_at, updated_at, expired_at
	`, req.Content, req.SurfaceType, req.Priority, req.RelatedEntryIDs, req.Tags, req.ReasoningCycle, req.TriggerAt)
	return scanSurface(row)
}

// List returns surfaces matching the given filter, ordered by priority then recency.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Surface, error) {
	var conditions []string
	var args []any
	argN := 1

	if f.Status != "" {
		conditions = append(conditions, fmt.Sprintf("status = $%d", argN))
		args = append(args, f.Status)
		argN++
	}
	if f.SurfaceType != "" {
		conditions = append(conditions, fmt.Sprintf("surface_type = $%d", argN))
		args = append(args, f.SurfaceType)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	limit := 50
	if f.Limit > 0 && f.Limit < 200 {
		limit = f.Limit
	}

	query := fmt.Sprintf(`
		SELECT id, content, surface_type, priority, status,
		       related_entry_ids, tags, user_response, responded_at,
		       reasoning_cycle, trigger_at, created_at, updated_at, expired_at
		FROM agenticme_surfaces
		%s
		ORDER BY priority ASC, created_at DESC
		LIMIT %d
	`, where, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list surfaces: %w", err)
	}
	defer rows.Close()

	return scanSurfaces(rows)
}

// Get returns a single surface by ID.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Surface, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, content, surface_type, priority, status,
		       related_entry_ids, tags, user_response, responded_at,
		       reasoning_cycle, trigger_at, created_at, updated_at, expired_at
		FROM agenticme_surfaces
		WHERE id = $1
	`, id)

	surf, err := scanSurface(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get surface %s: %w", id, err)
	}
	return surf, nil
}

// Update applies a partial update to a surface (e.g. dismiss).
func (s *Store) Update(ctx context.Context, id uuid.UUID, req UpdateRequest) (*Surface, error) {
	if req.Status == nil {
		return s.Get(ctx, id)
	}

	row := s.pool.QueryRow(ctx, `
		UPDATE agenticme_surfaces SET status = $1
		WHERE id = $2
		RETURNING id, content, surface_type, priority, status,
		          related_entry_ids, tags, user_response, responded_at,
		          reasoning_cycle, trigger_at, created_at, updated_at, expired_at
	`, *req.Status, id)

	surf, err := scanSurface(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("update surface %s: %w", id, err)
	}
	return surf, nil
}

// Respond records the user's answer to a question surface and writes
// a Q+A entry back to Eidetic.
func (s *Store) Respond(ctx context.Context, id uuid.UUID, req RespondRequest) (*Surface, error) {
	now := time.Now()
	row := s.pool.QueryRow(ctx, `
		UPDATE agenticme_surfaces
		SET status = 'answered', user_response = $1, responded_at = $2
		WHERE id = $3
		RETURNING id, content, surface_type, priority, status,
		          related_entry_ids, tags, user_response, responded_at,
		          reasoning_cycle, trigger_at, created_at, updated_at, expired_at
	`, req.Response, now, id)

	surf, err := scanSurface(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("respond surface %s: %w", id, err)
	}

	// Write Q+A back to Eidetic so it gets flat-file backup + embedding.
	if s.eidetic != nil {
		qaContent := fmt.Sprintf("Q: %s\nA: %s", surf.Content, req.Response)
		qaTags := append([]string{"user-response"}, surf.Tags...)
		if err := s.eidetic.AppendMemory(ctx, eidetic.AppendRequest{
			Content: qaContent,
			AgentID: "agenticme",
			Tags:    qaTags,
		}); err != nil {
			s.logger.Warnf("surfaces: eidetic write-back failed: %v", err)
		} else if s.OnEideticWrite != nil {
			s.OnEideticWrite()
		}
	}

	return surf, nil
}

// AddMessage inserts a chat message for a surface.
func (s *Store) AddMessage(ctx context.Context, surfaceID uuid.UUID, role, content string) (*ChatMessage, error) {
	var msg ChatMessage
	err := s.pool.QueryRow(ctx, `
		INSERT INTO agenticme_surface_messages (surface_id, role, content)
		VALUES ($1, $2, $3)
		RETURNING id, surface_id, role, content, created_at
	`, surfaceID, role, content).Scan(&msg.ID, &msg.SurfaceID, &msg.Role, &msg.Content, &msg.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("add message: %w", err)
	}
	return &msg, nil
}

// ListMessages returns all chat messages for a surface in chronological order.
func (s *Store) ListMessages(ctx context.Context, surfaceID uuid.UUID) ([]ChatMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, surface_id, role, content, created_at
		FROM agenticme_surface_messages
		WHERE surface_id = $1
		ORDER BY created_at ASC
	`, surfaceID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var msgs []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SurfaceID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// WriteConversationToEidetic writes the full surface conversation to Eidetic.
func (s *Store) WriteConversationToEidetic(ctx context.Context, surf *Surface, messages []ChatMessage) error {
	if len(messages) == 0 || s.eidetic == nil {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Surface [%s]: %s\n\n", surf.SurfaceType, surf.Content)
	for _, m := range messages {
		if m.Role == "user" {
			fmt.Fprintf(&b, "User: %s\n", m.Content)
		} else {
			fmt.Fprintf(&b, "Assistant: %s\n", m.Content)
		}
	}
	if surf.UserResponse != nil {
		fmt.Fprintf(&b, "\nResolution: %s\n", *surf.UserResponse)
	}

	tags := append([]string{"surface-conversation", string(surf.SurfaceType)}, surf.Tags...)
	if err := s.eidetic.AppendMemory(ctx, eidetic.AppendRequest{
		Content: b.String(),
		AgentID: "agenticme",
		Tags:    tags,
	}); err != nil {
		return err
	}
	if s.OnEideticWrite != nil {
		s.OnEideticWrite()
	}
	return nil
}

// DueReminders returns active surfaces whose trigger_at has passed.
func (s *Store) DueReminders(ctx context.Context) ([]Surface, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, content, surface_type, priority, status,
		       related_entry_ids, tags, user_response, responded_at,
		       reasoning_cycle, trigger_at, created_at, updated_at, expired_at
		FROM agenticme_surfaces
		WHERE status = 'active' AND trigger_at IS NOT NULL AND trigger_at <= now()
		ORDER BY trigger_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("due reminders: %w", err)
	}
	defer rows.Close()
	return scanSurfaces(rows)
}

// ActiveCount returns the number of active surfaces.
func (s *Store) ActiveCount(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agenticme_surfaces WHERE status = 'active'`).Scan(&count)
	return count, err
}

func scanSurface(row pgx.Row) (*Surface, error) {
	var surf Surface
	err := row.Scan(
		&surf.ID, &surf.Content, &surf.SurfaceType, &surf.Priority, &surf.Status,
		&surf.RelatedEntryIDs, &surf.Tags, &surf.UserResponse, &surf.RespondedAt,
		&surf.ReasoningCycle, &surf.TriggerAt, &surf.CreatedAt, &surf.UpdatedAt, &surf.ExpiredAt,
	)
	if err != nil {
		return nil, err
	}
	return &surf, nil
}

func scanSurfaces(rows pgx.Rows) ([]Surface, error) {
	var out []Surface
	for rows.Next() {
		var surf Surface
		if err := rows.Scan(
			&surf.ID, &surf.Content, &surf.SurfaceType, &surf.Priority, &surf.Status,
			&surf.RelatedEntryIDs, &surf.Tags, &surf.UserResponse, &surf.RespondedAt,
			&surf.ReasoningCycle, &surf.TriggerAt, &surf.CreatedAt, &surf.UpdatedAt, &surf.ExpiredAt,
		); err != nil {
			return nil, fmt.Errorf("scan surface: %w", err)
		}
		out = append(out, surf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return out, nil
}
