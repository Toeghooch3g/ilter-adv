package db

import (
	"context"

	"github.com/ilter-ai/ilter/internal/db/sqlc"
)

// ConversationRow is a conversations row for the dashboard chat UI.
// CreatedAt/UpdatedAt use the raw SQLite timestamp layout (see timefmt.go),
// matching what the dashboard API has always returned.
type ConversationRow struct {
	ID        string
	Title     string
	CreatedAt string
	UpdatedAt string
}

// ConversationSummary is a ConversationRow plus its last-message preview and
// message count, for the thread list view.
type ConversationSummary struct {
	ConversationRow
	LastMessage  string
	MessageCount int
}

// ListConversations returns all conversations ordered by most recently updated.
func (s *SQLiteStore) ListConversations(ctx context.Context) ([]ConversationSummary, error) {
	rows, err := s.queries.ListConversations(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ConversationSummary, 0, len(rows))
	for _, r := range rows {
		result = append(result, ConversationSummary{
			ConversationRow: ConversationRow{
				ID:        r.ID,
				Title:     r.Title,
				CreatedAt: r.CreatedAt.UTC().Format(sqliteTimestampLayout),
				UpdatedAt: r.UpdatedAt.UTC().Format(sqliteTimestampLayout),
			},
			LastMessage:  r.LastMessage,
			MessageCount: int(r.MessageCount),
		})
	}
	return result, nil
}

// CreateConversation inserts a new conversation with the given id and title.
func (s *SQLiteStore) CreateConversation(ctx context.Context, id, title string) error {
	return s.queries.CreateConversation(ctx, sqlc.CreateConversationParams{ID: id, Title: title})
}

// GetConversation returns a single conversation by id.
func (s *SQLiteStore) GetConversation(ctx context.Context, id string) (*ConversationRow, error) {
	r, err := s.queries.GetConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ConversationRow{
		ID:        r.ID,
		Title:     r.Title,
		CreatedAt: r.CreatedAt.UTC().Format(sqliteTimestampLayout),
		UpdatedAt: r.UpdatedAt.UTC().Format(sqliteTimestampLayout),
	}, nil
}

// ConversationExists reports whether a conversation with the given id exists.
func (s *SQLiteStore) ConversationExists(ctx context.Context, id string) (bool, error) {
	n, err := s.queries.ConversationExists(ctx, id)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateConversationTitle sets a conversation's title and bumps updated_at.
// Returns false if no conversation matched id.
func (s *SQLiteStore) UpdateConversationTitle(ctx context.Context, id, title string) (bool, error) {
	n, err := s.queries.UpdateConversationTitle(ctx, sqlc.UpdateConversationTitleParams{Title: title, ID: id})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetConversationTitle sets a conversation's title without bumping updated_at
// (used by the auto-title-from-first-message heuristic).
func (s *SQLiteStore) SetConversationTitle(ctx context.Context, id, title string) error {
	return s.queries.SetConversationTitle(ctx, sqlc.SetConversationTitleParams{Title: title, ID: id})
}

// TouchConversation bumps a conversation's updated_at to now.
func (s *SQLiteStore) TouchConversation(ctx context.Context, id string) error {
	return s.queries.TouchConversation(ctx, id)
}

// DeleteConversation removes a conversation and its messages (CASCADE).
// Returns false if no conversation matched id.
func (s *SQLiteStore) DeleteConversation(ctx context.Context, id string) (bool, error) {
	n, err := s.queries.DeleteConversation(ctx, id)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MessageRow is a messages row for the dashboard chat UI.
type MessageRow struct {
	ID               int
	ConversationID   string
	Role             string
	Content          string
	Model            *string
	TokenCount       *int
	Cost             *float64
	ReasoningContent *string
	ToolCalls        *string
	UsageCost        *float64
	BillingKey       *string
	CreatedAt        string
}

func messageRowFromSQLC(m sqlc.Message) MessageRow {
	return MessageRow{
		ID:               int(m.ID),
		ConversationID:   m.ConversationID,
		Role:             m.Role,
		Content:          m.Content,
		Model:            m.Model,
		TokenCount:       int64PtrToIntPtr(m.TokenCount),
		Cost:             m.Cost,
		ReasoningContent: m.ReasoningContent,
		ToolCalls:        m.ToolCalls,
		UsageCost:        m.UsageCost,
		BillingKey:       m.BillingKey,
		CreatedAt:        m.CreatedAt.UTC().Format(sqliteTimestampLayout),
	}
}

// ListMessagesByConversation returns every message for a conversation, oldest first.
func (s *SQLiteStore) ListMessagesByConversation(ctx context.Context, conversationID string) ([]MessageRow, error) {
	rows, err := s.queries.ListMessagesByConversation(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	result := make([]MessageRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, messageRowFromSQLC(r))
	}
	return result, nil
}

// NewMessageParams holds the fields needed to insert a new message.
type NewMessageParams struct {
	ConversationID   string
	Role             string
	Content          string
	Model            *string
	TokenCount       *int
	Cost             *float64
	ReasoningContent *string
	ToolCalls        *string
	UsageCost        *float64
	BillingKey       *string
}

// InsertMessage inserts a new message and returns its assigned id.
func (s *SQLiteStore) InsertMessage(ctx context.Context, p NewMessageParams) (int, error) {
	id, err := s.queries.InsertMessage(ctx, sqlc.InsertMessageParams{
		ConversationID:   p.ConversationID,
		Role:             p.Role,
		Content:          p.Content,
		Model:            p.Model,
		TokenCount:       intToInt64Ptr(p.TokenCount),
		Cost:             p.Cost,
		ReasoningContent: p.ReasoningContent,
		ToolCalls:        p.ToolCalls,
		UsageCost:        p.UsageCost,
		BillingKey:       p.BillingKey,
	})
	if err != nil {
		return 0, err
	}
	return int(id), nil
}

// GetMessageCreatedAt returns the created_at timestamp for a message, in the
// same raw layout as other timestamp fields in this package.
func (s *SQLiteStore) GetMessageCreatedAt(ctx context.Context, id int) (string, error) {
	t, err := s.queries.GetMessageCreatedAt(ctx, int64(id))
	if err != nil {
		return "", err
	}
	return t.UTC().Format(sqliteTimestampLayout), nil
}

// ListMessagesPaginated returns up to limit messages for a conversation,
// newest first, optionally starting before beforeID (nil for the first page).
func (s *SQLiteStore) ListMessagesPaginated(ctx context.Context, conversationID string, beforeID *int, limit int) ([]MessageRow, error) {
	rows, err := s.queries.ListMessagesPaginated(ctx, sqlc.ListMessagesPaginatedParams{
		ConversationID: conversationID,
		BeforeID:       intToInt64Ptr(beforeID),
		Limit:          int64(limit),
	})
	if err != nil {
		return nil, err
	}
	result := make([]MessageRow, 0, len(rows))
	for _, r := range rows {
		result = append(result, messageRowFromSQLC(r))
	}
	return result, nil
}
