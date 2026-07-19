package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/priyavratuniyal/tuskbase/internal/domain"
	"github.com/priyavratuniyal/tuskbase/internal/ports"
)

//go:embed schema.sql
var schemaFS embed.FS

type Store struct {
	db *sql.DB
}

// Open configures SQLite for one owning process. Local Basic should share this through the daemon instead of many agents writing the file directly.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate(ctx context.Context) error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, string(schema)); err != nil {
		return err
	}
	columns := map[string]string{
		"valid_from":         "TEXT NOT NULL DEFAULT ''",
		"valid_to":           "TEXT",
		"transaction_time":   "TEXT NOT NULL DEFAULT ''",
		"precedent_ref":      "TEXT NOT NULL DEFAULT ''",
		"supersedes_id":      "TEXT NOT NULL DEFAULT ''",
		"completeness_score": "REAL NOT NULL DEFAULT 0",
		"metadata_json":      "TEXT NOT NULL DEFAULT '{}'",
		"alternatives_json":  "TEXT NOT NULL DEFAULT '[]'",
	}
	for column, definition := range columns {
		if err := s.ensureColumn(ctx, "decisions", column, definition); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE decisions SET valid_from = created_at WHERE valid_from = '';
UPDATE decisions SET transaction_time = created_at WHERE transaction_time = '';
UPDATE decisions SET valid_to = updated_at WHERE status IN ('superseded', 'deprecated') AND valid_to IS NULL;
`); err != nil {
		return err
	}
	return s.backfillDecisionChildren(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func (s *Store) backfillDecisionChildren(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, alternatives_json, claims_json, evidence_json, relationships_json
FROM decisions
`)
	if err != nil {
		return err
	}
	var decisions []domain.Decision
	for rows.Next() {
		var d domain.Decision
		var alternativesJSON, claimsJSON, evidenceJSON, relationshipsJSON string
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &alternativesJSON, &claimsJSON, &evidenceJSON, &relationshipsJSON); err != nil {
			_ = rows.Close()
			return err
		}
		if err := unmarshalJSON(alternativesJSON, &d.Alternatives); err != nil {
			_ = rows.Close()
			return err
		}
		if err := unmarshalJSON(claimsJSON, &d.Claims); err != nil {
			_ = rows.Close()
			return err
		}
		if err := unmarshalJSON(evidenceJSON, &d.Evidence); err != nil {
			_ = rows.Close()
			return err
		}
		if err := unmarshalJSON(relationshipsJSON, &d.Relationships); err != nil {
			_ = rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, d := range decisions {
		if err := s.insertMissingDecisionChildren(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertMissingDecisionChildren(ctx context.Context, d domain.Decision) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(d.Alternatives) > 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_alternatives WHERE decision_id = ?)`, d.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			for _, alternative := range d.Alternatives {
				if alternative.ID == "" {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decision_alternatives(id, decision_id, title, outcome, reason) VALUES (?, ?, ?, ?, ?)`, alternative.ID, d.ID, alternative.Title, alternative.Outcome, alternative.Reason); err != nil {
					return err
				}
			}
		}
	}
	if len(d.Claims) > 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_claims WHERE decision_id = ?)`, d.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			for _, claim := range d.Claims {
				if claim.ID == "" || strings.TrimSpace(claim.Text) == "" {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decision_claims(id, decision_id, text) VALUES (?, ?, ?)`, claim.ID, d.ID, claim.Text); err != nil {
					return err
				}
			}
		}
	}
	if len(d.Evidence) > 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_evidence WHERE decision_id = ?)`, d.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			for _, evidence := range d.Evidence {
				if evidence.ID == "" {
					continue
				}
				metadata, err := marshalJSON(evidence.Metadata, "{}")
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decision_evidence(id, decision_id, kind, uri, title, snippet, metadata_json) VALUES (?, ?, ?, ?, ?, ?, ?)`, evidence.ID, d.ID, evidence.Kind, evidence.URI, evidence.Title, evidence.Snippet, metadata); err != nil {
					return err
				}
			}
		}
	}
	if len(d.Relationships) > 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_relationships WHERE from_decision_id = ?)`, d.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			for _, rel := range d.Relationships {
				if rel.ID == "" {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO decision_relationships(id, workspace_id, from_decision_id, to_decision_id, type, confidence, reason) VALUES (?, ?, ?, ?, ?, ?, ?)`, rel.ID, rel.WorkspaceID, rel.FromDecisionID, rel.ToDecisionID, rel.Type, rel.Confidence, rel.Reason); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertWorkspace(ctx context.Context, w domain.Workspace) (domain.Workspace, error) {
	if err := w.Validate(); err != nil {
		return domain.Workspace{}, err
	}
	existing, err := s.GetWorkspace(ctx, w.ID)
	if err == nil && !existing.CreatedAt.IsZero() {
		w.CreatedAt = existing.CreatedAt
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO workspaces(id, repo_root, display_name, repo_fingerprint, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    repo_root = excluded.repo_root,
    display_name = excluded.display_name,
    repo_fingerprint = excluded.repo_fingerprint,
    updated_at = excluded.updated_at
`, w.ID, w.RepoRoot, w.DisplayName, w.RepoFingerprint, formatTime(w.CreatedAt), formatTime(w.UpdatedAt))
	if err != nil {
		return domain.Workspace{}, err
	}
	return w, nil
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (domain.Workspace, error) {
	var w domain.Workspace
	var created, updated string
	err := s.db.QueryRowContext(ctx, `
SELECT id, repo_root, display_name, repo_fingerprint, created_at, updated_at
FROM workspaces WHERE id = ?
`, id).Scan(&w.ID, &w.RepoRoot, &w.DisplayName, &w.RepoFingerprint, &created, &updated)
	if err != nil {
		return domain.Workspace{}, err
	}
	w.CreatedAt = parseTime(created)
	w.UpdatedAt = parseTime(updated)
	return w, nil
}

func (s *Store) SaveDecision(ctx context.Context, d domain.Decision) error {
	defaultDecisionTimes(&d)
	if d.Status == "" {
		d.Status = domain.DecisionActive
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if d.SupersedesID == d.ID {
		return errors.New("decision cannot supersede itself")
	}
	actor, err := marshalJSON(d.Actor, "{}")
	if err != nil {
		return err
	}
	metadata, err := marshalJSON(d.Metadata, "{}")
	if err != nil {
		return err
	}
	alternatives, err := marshalJSON(d.Alternatives, "[]")
	if err != nil {
		return err
	}
	claims, err := marshalJSON(d.Claims, "[]")
	if err != nil {
		return err
	}
	evidence, err := marshalJSON(d.Evidence, "[]")
	if err != nil {
		return err
	}
	relationships, err := marshalJSON(d.Relationships, "[]")
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if d.SupersedesID != "" {
		result, err := tx.ExecContext(ctx, `
UPDATE decisions
SET valid_to = ?, status = ?, updated_at = ?
WHERE id = ? AND workspace_id = ? AND valid_to IS NULL
`, formatTime(d.ValidFrom), domain.DecisionSuperseded, formatTime(d.UpdatedAt), d.SupersedesID, d.WorkspaceID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return fmt.Errorf("supersedes_id %q does not reference an active decision", d.SupersedesID)
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO decisions(
    id, workspace_id, actor_json, type, title, outcome, rationale, confidence, status,
    valid_from, valid_to, transaction_time, precedent_ref, supersedes_id, completeness_score, metadata_json,
    alternatives_json, claims_json, evidence_json, relationships_json, created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, d.ID, d.WorkspaceID, actor, d.Type, d.Title, d.Outcome, d.Rationale, d.Confidence, d.Status, formatTime(d.ValidFrom), nullableTime(d.ValidTo), formatTime(d.TransactionTime), d.PrecedentRef, d.SupersedesID, d.CompletenessScore, metadata, alternatives, claims, evidence, relationships, formatTime(d.CreatedAt), formatTime(d.UpdatedAt))
	if err != nil {
		return err
	}
	if err := replaceDecisionChildren(ctx, tx, d); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetDecision(ctx context.Context, id string) (domain.Decision, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+decisionSelectColumns+` FROM decisions WHERE id = ?`, id)
	d, err := scanDecisionBase(row)
	if err != nil {
		return domain.Decision{}, err
	}
	if err := s.loadDecisionChildren(ctx, &d); err != nil {
		return domain.Decision{}, err
	}
	return d, nil
}

// RecentDecisions closes the base cursor before loading children because this adapter uses one SQLite connection.
func (s *Store) RecentDecisions(ctx context.Context, workspaceID string, limit int) ([]domain.Decision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+decisionSelectColumns+` FROM decisions WHERE workspace_id = ? AND valid_to IS NULL ORDER BY created_at DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	var decisions []domain.Decision
	for rows.Next() {
		d, err := scanDecisionBase(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range decisions {
		if err := s.loadDecisionChildren(ctx, &decisions[i]); err != nil {
			return nil, err
		}
	}
	return decisions, nil
}

func (s *Store) QueryDecisions(ctx context.Context, q ports.DecisionQuery) ([]domain.Decision, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}
	clauses := []string{"workspace_id = ?"}
	args := []any{q.WorkspaceID}
	if q.Type != "" {
		clauses = append(clauses, "lower(type) = lower(?)")
		args = append(args, q.Type)
	}
	if q.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, q.Status)
	}
	if q.MinConfidence > 0 {
		clauses = append(clauses, "confidence >= ?")
		args = append(args, q.MinConfidence)
	}
	for _, token := range searchTokens(q.Text) {
		clauses = append(clauses, "lower(type || ' ' || title || ' ' || outcome || ' ' || rationale || ' ' || precedent_ref || ' ' || supersedes_id) LIKE ?")
		args = append(args, "%"+token+"%")
	}
	if q.RelationshipTo != "" {
		relClause := "EXISTS (SELECT 1 FROM decision_relationships r WHERE r.from_decision_id = decisions.id AND r.workspace_id = decisions.workspace_id AND r.to_decision_id = ?"
		args = append(args, q.RelationshipTo)
		if q.RelationshipType != "" {
			relClause += " AND r.type = ?"
			args = append(args, q.RelationshipType)
		}
		relClause += ")"
		clauses = append(clauses, relClause)
	} else if q.RelationshipType != "" {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM decision_relationships r WHERE r.from_decision_id = decisions.id AND r.workspace_id = decisions.workspace_id AND r.type = ?)")
		args = append(args, q.RelationshipType)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+decisionSelectColumns+` FROM decisions WHERE `+strings.Join(clauses, " AND ")+` ORDER BY created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var decisions []domain.Decision
	for rows.Next() {
		d, err := scanDecisionBase(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range decisions {
		if err := s.loadDecisionChildren(ctx, &decisions[i]); err != nil {
			return nil, err
		}
	}
	return decisions, nil
}

func (s *Store) UpsertDecisionCandidate(ctx context.Context, c domain.DecisionCandidate) (domain.DecisionCandidate, error) {
	if c.Status == "" {
		c.Status = domain.CandidatePending
	}
	if err := c.Validate(); err != nil {
		return domain.DecisionCandidate{}, err
	}
	_, err := s.db.ExecContext(ctx, upsertDecisionCandidateSQL, c.ID, c.WorkspaceID, c.Type, c.Title, c.Outcome, c.Rationale, c.Confidence, c.SourcePath, c.SourceTitle, c.SourceSnippet, c.SourceHash, c.Detector, c.Status, c.AcceptedDecisionID, c.RejectionSummary, formatTime(c.CreatedAt), formatTime(c.UpdatedAt))
	if err != nil {
		return domain.DecisionCandidate{}, err
	}
	return s.getDecisionCandidateBySourceHash(ctx, c.WorkspaceID, c.SourceHash)
}

func (s *Store) GetDecisionCandidate(ctx context.Context, id string) (domain.DecisionCandidate, error) {
	row := s.db.QueryRowContext(ctx, getDecisionCandidateSQL(), id)
	return scanDecisionCandidate(row)
}

func (s *Store) getDecisionCandidateBySourceHash(ctx context.Context, workspaceID, sourceHash string) (domain.DecisionCandidate, error) {
	row := s.db.QueryRowContext(ctx, getDecisionCandidateBySourceHashSQL(), workspaceID, sourceHash)
	return scanDecisionCandidate(row)
}

func (s *Store) ListDecisionCandidates(ctx context.Context, q ports.DecisionCandidateQuery) ([]domain.DecisionCandidate, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	clauses := []string{"workspace_id = ?"}
	args := []any{q.WorkspaceID}
	if !q.AllStatuses {
		status := q.Status
		if status == "" {
			status = domain.CandidatePending
		}
		clauses = append(clauses, "status = ?")
		args = append(args, status)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, listDecisionCandidatesSQL(strings.Join(clauses, " AND ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []domain.DecisionCandidate
	for rows.Next() {
		c, err := scanDecisionCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (s *Store) UpdateDecisionCandidateStatus(ctx context.Context, u ports.DecisionCandidateStatusUpdate) (domain.DecisionCandidate, error) {
	status, err := domain.ParseDecisionCandidateStatus(string(u.Status))
	if err != nil {
		return domain.DecisionCandidate{}, err
	}
	result, err := s.db.ExecContext(ctx, updateDecisionCandidateStatusSQL, status, strings.TrimSpace(u.AcceptedDecisionID), strings.TrimSpace(u.RejectionSummary), formatTime(u.UpdatedAt), strings.TrimSpace(u.ID), strings.TrimSpace(u.WorkspaceID))
	if err != nil {
		return domain.DecisionCandidate{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return domain.DecisionCandidate{}, err
	}
	if rows == 0 {
		return domain.DecisionCandidate{}, sql.ErrNoRows
	}
	return s.GetDecisionCandidate(ctx, u.ID)
}

func replaceDecisionChildren(ctx context.Context, tx *sql.Tx, d domain.Decision) error {
	for _, table := range []string{"decision_alternatives", "decision_claims", "decision_evidence", "decision_relationships"} {
		column := "decision_id"
		if table == "decision_relationships" {
			column = "from_decision_id"
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s = ?", table, column), d.ID); err != nil {
			return err
		}
	}
	for _, alternative := range d.Alternatives {
		_, err := tx.ExecContext(ctx, `
INSERT INTO decision_alternatives(id, decision_id, title, outcome, reason)
VALUES (?, ?, ?, ?, ?)
`, alternative.ID, d.ID, alternative.Title, alternative.Outcome, alternative.Reason)
		if err != nil {
			return err
		}
	}
	for _, claim := range d.Claims {
		if strings.TrimSpace(claim.Text) == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO decision_claims(id, decision_id, text)
VALUES (?, ?, ?)
`, claim.ID, d.ID, claim.Text)
		if err != nil {
			return err
		}
	}
	for _, evidence := range d.Evidence {
		metadata, err := marshalJSON(evidence.Metadata, "{}")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO decision_evidence(id, decision_id, kind, uri, title, snippet, metadata_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
`, evidence.ID, d.ID, evidence.Kind, evidence.URI, evidence.Title, evidence.Snippet, metadata)
		if err != nil {
			return err
		}
	}
	for _, rel := range d.Relationships {
		_, err := tx.ExecContext(ctx, `
INSERT INTO decision_relationships(id, workspace_id, from_decision_id, to_decision_id, type, confidence, reason)
VALUES (?, ?, ?, ?, ?, ?, ?)
`, rel.ID, d.WorkspaceID, d.ID, rel.ToDecisionID, rel.Type, rel.Confidence, rel.Reason)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadDecisionChildren(ctx context.Context, d *domain.Decision) error {
	alternatives, err := s.listAlternatives(ctx, d.ID)
	if err != nil {
		return err
	}
	if len(alternatives) > 0 {
		d.Alternatives = alternatives
	}
	claims, err := s.listClaims(ctx, d.ID)
	if err != nil {
		return err
	}
	if len(claims) > 0 {
		d.Claims = claims
	}
	evidence, err := s.listEvidence(ctx, d.ID)
	if err != nil {
		return err
	}
	if len(evidence) > 0 {
		d.Evidence = evidence
	}
	relationships, err := s.listRelationships(ctx, d.ID)
	if err != nil {
		return err
	}
	if len(relationships) > 0 {
		d.Relationships = relationships
	}
	return nil
}

func (s *Store) listAlternatives(ctx context.Context, decisionID string) ([]domain.Alternative, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, decision_id, title, outcome, reason FROM decision_alternatives WHERE decision_id = ? ORDER BY rowid`, decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Alternative
	for rows.Next() {
		var a domain.Alternative
		if err := rows.Scan(&a.ID, &a.DecisionID, &a.Title, &a.Outcome, &a.Reason); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) listClaims(ctx context.Context, decisionID string) ([]domain.Claim, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, decision_id, text FROM decision_claims WHERE decision_id = ? ORDER BY rowid`, decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Claim
	for rows.Next() {
		var c domain.Claim
		if err := rows.Scan(&c.ID, &c.DecisionID, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) listEvidence(ctx context.Context, decisionID string) ([]domain.Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, decision_id, kind, uri, title, snippet, metadata_json FROM decision_evidence WHERE decision_id = ? ORDER BY rowid`, decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Evidence
	for rows.Next() {
		var e domain.Evidence
		var metadata string
		if err := rows.Scan(&e.ID, &e.DecisionID, &e.Kind, &e.URI, &e.Title, &e.Snippet, &metadata); err != nil {
			return nil, err
		}
		if err := unmarshalJSON(metadata, &e.Metadata); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) listRelationships(ctx context.Context, decisionID string) ([]domain.DecisionRelationship, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id, from_decision_id, to_decision_id, type, confidence, reason FROM decision_relationships WHERE from_decision_id = ? ORDER BY rowid`, decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DecisionRelationship
	for rows.Next() {
		var r domain.DecisionRelationship
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.FromDecisionID, &r.ToDecisionID, &r.Type, &r.Confidence, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceWorkspaceDocuments(ctx context.Context, workspaceID string, docs []domain.RepoDocument) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM repo_documents WHERE workspace_id = ?`, workspaceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_index WHERE workspace_id = ? AND kind = 'document'`, workspaceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vector_index WHERE workspace_id = ? AND kind = 'document'`, workspaceID); err != nil {
		return err
	}
	for _, doc := range docs {
		_, err := tx.ExecContext(ctx, `
INSERT INTO repo_documents(id, workspace_id, path, title, content, chunk_index, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, doc.ID, workspaceID, doc.Path, doc.Title, doc.Content, doc.ChunkIndex, formatTime(doc.CreatedAt), formatTime(doc.UpdatedAt))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListWorkspaceDocuments(ctx context.Context, workspaceID string, limit int) ([]domain.RepoDocument, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, path, title, content, chunk_index, created_at, updated_at
FROM repo_documents
WHERE workspace_id = ?
ORDER BY path, chunk_index
LIMIT ?
`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []domain.RepoDocument
	for rows.Next() {
		var doc domain.RepoDocument
		var created, updated string
		if err := rows.Scan(&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Content, &doc.ChunkIndex, &created, &updated); err != nil {
			return nil, err
		}
		doc.CreatedAt = parseTime(created)
		doc.UpdatedAt = parseTime(updated)
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) SaveLookupReceipt(ctx context.Context, r domain.LookupReceipt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO lookup_receipts(id, workspace_id, query, result_count, created_at)
VALUES (?, ?, ?, ?, ?)
`, r.ID, r.WorkspaceID, r.Query, r.ResultCount, formatTime(r.CreatedAt))
	if err != nil {
		return err
	}
	for i, result := range r.Results {
		_, err := tx.ExecContext(ctx, `
INSERT INTO lookup_receipt_results(receipt_id, position, kind, entity_id, workspace_id, title, path, snippet, score)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`, r.ID, i, result.Kind, result.EntityID, result.WorkspaceID, result.Title, result.Path, result.Snippet, result.Score)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveAssessment(ctx context.Context, a domain.Assessment) error {
	if err := a.Validate(); err != nil {
		return err
	}
	actor, err := marshalJSON(a.Actor, "{}")
	if err != nil {
		return err
	}
	metadata, err := marshalJSON(a.Metadata, "{}")
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO decision_assessments(id, workspace_id, decision_id, actor_json, signal, score, summary, metadata_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`, a.ID, a.WorkspaceID, a.DecisionID, actor, a.Signal, a.Score, a.Summary, metadata, formatTime(a.CreatedAt))
	return err
}

func (s *Store) ListAssessments(ctx context.Context, q ports.AssessmentQuery) ([]domain.Assessment, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	clauses := []string{"workspace_id = ?"}
	args := []any{q.WorkspaceID}
	if q.DecisionID != "" {
		clauses = append(clauses, "decision_id = ?")
		args = append(args, q.DecisionID)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, decision_id, actor_json, signal, score, summary, metadata_json, created_at
FROM decision_assessments
WHERE `+strings.Join(clauses, " AND ")+`
ORDER BY created_at DESC
LIMIT ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var assessments []domain.Assessment
	for rows.Next() {
		var a domain.Assessment
		var actorJSON, metadataJSON, created string
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.DecisionID, &actorJSON, &a.Signal, &a.Score, &a.Summary, &metadataJSON, &created); err != nil {
			return nil, err
		}
		if err := unmarshalJSON(actorJSON, &a.Actor); err != nil {
			return nil, err
		}
		if err := unmarshalJSON(metadataJSON, &a.Metadata); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTime(created)
		assessments = append(assessments, a)
	}
	return assessments, rows.Err()
}

func (s *Store) SaveConflict(ctx context.Context, c domain.Conflict) error {
	status, err := domain.ParseConflictStatus(string(c.Status))
	if err != nil {
		return err
	}
	c.Status = status
	if err := c.Validate(); err != nil {
		return err
	}
	var resolved any
	if c.ResolvedAt != nil {
		resolved = formatTime(*c.ResolvedAt)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO conflicts(id, workspace_id, decision_id, proposal, conflicting_with_id, summary, severity, confidence, status, created_at, resolved_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, c.ID, c.WorkspaceID, c.DecisionID, c.Proposal, c.ConflictingWithID, c.Summary, c.Severity, c.Confidence, c.Status, formatTime(c.CreatedAt), resolved)
	return err
}

func (s *Store) GetConflict(ctx context.Context, id string) (domain.Conflict, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, workspace_id, decision_id, proposal, conflicting_with_id, summary, severity, confidence, status, created_at, resolved_at
FROM conflicts
WHERE id = ?
`, id)
	return scanConflict(row)
}

func (s *Store) ListConflicts(ctx context.Context, q ports.ConflictQuery) ([]domain.Conflict, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	clauses := []string{"workspace_id = ?"}
	args := []any{q.WorkspaceID}
	if q.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, q.Status)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, decision_id, proposal, conflicting_with_id, summary, severity, confidence, status, created_at, resolved_at
FROM conflicts
WHERE `+strings.Join(clauses, " AND ")+`
ORDER BY created_at DESC
LIMIT ?
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var conflicts []domain.Conflict
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			return nil, err
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}

func (s *Store) ResolveConflict(ctx context.Context, conflictID string, status domain.ConflictStatus, resolvedAt time.Time, resolution domain.ConflictResolution) (domain.Conflict, error) {
	if status == "" {
		status = domain.ConflictResolved
	}
	if _, err := domain.ParseConflictStatus(string(status)); err != nil {
		return domain.Conflict{}, err
	}
	if err := resolution.Validate(); err != nil {
		return domain.Conflict{}, err
	}
	actor, err := marshalJSON(resolution.Actor, "{}")
	if err != nil {
		return domain.Conflict{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Conflict{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE conflicts
SET status = ?, resolved_at = ?
WHERE id = ? AND workspace_id = ?
`, status, formatTime(resolvedAt), conflictID, resolution.WorkspaceID)
	if err != nil {
		return domain.Conflict{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return domain.Conflict{}, err
	}
	if rows == 0 {
		return domain.Conflict{}, sql.ErrNoRows
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO conflict_resolutions(id, workspace_id, conflict_id, actor_json, action, summary, decision_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, resolution.ID, resolution.WorkspaceID, resolution.ConflictID, actor, resolution.Action, resolution.Summary, resolution.DecisionID, formatTime(resolution.CreatedAt))
	if err != nil {
		return domain.Conflict{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Conflict{}, err
	}
	return s.GetConflict(ctx, conflictID)
}

func (s *Store) ListOpenConflicts(ctx context.Context, workspaceID string) ([]domain.Conflict, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, c.workspace_id, c.decision_id, c.proposal, c.conflicting_with_id, c.summary, c.severity, c.confidence, c.status, c.created_at, c.resolved_at
FROM conflicts c
WHERE c.workspace_id = ?
  AND c.status = 'open'
  AND EXISTS (SELECT 1 FROM decisions d WHERE d.id = c.conflicting_with_id AND d.workspace_id = c.workspace_id AND d.valid_to IS NULL)
ORDER BY c.created_at DESC
`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var conflicts []domain.Conflict
	for rows.Next() {
		var c domain.Conflict
		var created string
		var resolved sql.NullString
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.DecisionID, &c.Proposal, &c.ConflictingWithID, &c.Summary, &c.Severity, &c.Confidence, &c.Status, &created, &resolved); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(created)
		if resolved.Valid {
			t := parseTime(resolved.String)
			c.ResolvedAt = &t
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, rows.Err()
}

func scanConflict(row rowScanner) (domain.Conflict, error) {
	var c domain.Conflict
	var created string
	var resolved sql.NullString
	if err := row.Scan(&c.ID, &c.WorkspaceID, &c.DecisionID, &c.Proposal, &c.ConflictingWithID, &c.Summary, &c.Severity, &c.Confidence, &c.Status, &created, &resolved); err != nil {
		return domain.Conflict{}, err
	}
	c.CreatedAt = parseTime(created)
	if resolved.Valid {
		t := parseTime(resolved.String)
		c.ResolvedAt = &t
	}
	return c, nil
}

func (s *Store) IndexDecision(ctx context.Context, d domain.Decision) error {
	if !d.IsActive() {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM search_index WHERE workspace_id = ? AND entity_id = ? AND kind IN ('decision', 'claim', 'evidence', 'alternative')`, d.WorkspaceID, d.ID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM vector_index WHERE workspace_id = ? AND entity_id = ? AND kind IN ('decision', 'claim', 'evidence', 'alternative')`, d.WorkspaceID, d.ID); err != nil {
		return err
	}
	text := strings.Join([]string{d.Type, d.Title, d.Outcome, d.Rationale, d.PrecedentRef, d.SupersedesID}, "\n")
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO search_index(workspace_id, kind, entity_id, title, path, body)
VALUES (?, 'decision', ?, ?, '', ?)
`, d.WorkspaceID, d.ID, d.Title, text); err != nil {
		return err
	}
	for _, alternative := range d.Alternatives {
		text := strings.TrimSpace(strings.Join([]string{alternative.Title, alternative.Outcome, alternative.Reason}, "\n"))
		if text == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO search_index(workspace_id, kind, entity_id, title, path, body)
VALUES (?, 'alternative', ?, ?, '', ?)
`, d.WorkspaceID, d.ID, d.Title, text); err != nil {
			return err
		}
	}
	for _, claim := range d.Claims {
		if strings.TrimSpace(claim.Text) == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO search_index(workspace_id, kind, entity_id, title, path, body)
VALUES (?, 'claim', ?, ?, '', ?)
`, d.WorkspaceID, d.ID, d.Title, claim.Text); err != nil {
			return err
		}
	}
	for _, evidence := range d.Evidence {
		text := strings.TrimSpace(strings.Join([]string{evidence.Kind, evidence.URI, evidence.Title, evidence.Snippet}, "\n"))
		if text == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO search_index(workspace_id, kind, entity_id, title, path, body)
VALUES (?, 'evidence', ?, ?, ?, ?)
`, d.WorkspaceID, d.ID, d.Title, evidence.URI, text); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) IndexDocument(ctx context.Context, doc domain.RepoDocument) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO search_index(workspace_id, kind, entity_id, title, path, body)
VALUES (?, 'document', ?, ?, ?, ?)
`, doc.WorkspaceID, doc.ID, doc.Title, doc.Path, doc.Content)
	return err
}

func (s *Store) Search(ctx context.Context, q ports.SearchQuery) ([]ports.SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	match := ftsQuery(q.Query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT kind, entity_id, workspace_id, title, path, snippet(search_index, 5, '', '', '...', 20), bm25(search_index) AS rank
FROM search_index
WHERE search_index MATCH ?
  AND workspace_id = ?
  AND (
      kind = 'document'
      OR EXISTS (
          SELECT 1 FROM decisions d
          WHERE d.id = search_index.entity_id
            AND d.workspace_id = search_index.workspace_id
            AND d.valid_to IS NULL
      )
  )
ORDER BY rank
LIMIT ?
`, match, q.WorkspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []ports.SearchResult
	for rows.Next() {
		var r ports.SearchResult
		var rank float64
		if err := rows.Scan(&r.Kind, &r.EntityID, &r.WorkspaceID, &r.Title, &r.Path, &r.Snippet, &rank); err != nil {
			return nil, err
		}
		r.Score = -rank
		results = append(results, r)
	}
	return results, rows.Err()
}

// UpsertVector keeps SQLite useful for Local Basic; Local Shared should move vector search to pgvector or another VectorStore.
func (s *Store) UpsertVector(ctx context.Context, record ports.VectorRecord) error {
	if strings.TrimSpace(record.WorkspaceID) == "" || len(record.Vector) == 0 {
		return nil
	}
	data, err := json.Marshal(record.Vector)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO vector_index(workspace_id, kind, entity_id, title, path, body, vector_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, kind, entity_id, path) DO UPDATE SET
    title = excluded.title,
    body = excluded.body,
    vector_json = excluded.vector_json
`, record.WorkspaceID, record.Kind, record.EntityID, record.Title, record.Path, snippetText(record.Text), string(data))
	return err
}

// SearchVector is intentionally simple and in-process. It is a correctness path, not the final high-scale vector strategy.
func (s *Store) SearchVector(ctx context.Context, q ports.VectorQuery) ([]ports.SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(q.Vector) == 0 || strings.TrimSpace(q.WorkspaceID) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT kind, entity_id, workspace_id, title, path, body, vector_json
FROM vector_index
WHERE workspace_id = ?
  AND (
      kind = 'document'
      OR EXISTS (
          SELECT 1 FROM decisions d
          WHERE d.id = vector_index.entity_id
            AND d.workspace_id = vector_index.workspace_id
            AND d.valid_to IS NULL
      )
  )
`, q.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []ports.SearchResult
	for rows.Next() {
		var r ports.SearchResult
		var body, vectorJSON string
		if err := rows.Scan(&r.Kind, &r.EntityID, &r.WorkspaceID, &r.Title, &r.Path, &body, &vectorJSON); err != nil {
			return nil, err
		}
		var vector []float32
		if err := json.Unmarshal([]byte(vectorJSON), &vector); err != nil || len(vector) == 0 {
			continue
		}
		r.Score = cosine(q.Vector, vector)
		r.Snippet = body
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av := float64(a[i])
		bv := float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func snippetText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= 240 {
		return text
	}
	return text[:240] + "..."
}

const decisionSelectColumns = `
id, workspace_id, actor_json, type, title, outcome, rationale, confidence, status,
valid_from, valid_to, transaction_time, precedent_ref, supersedes_id, completeness_score, metadata_json,
alternatives_json, claims_json, evidence_json, relationships_json, created_at, updated_at
`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDecisionBase(row rowScanner) (domain.Decision, error) {
	var d domain.Decision
	var actorJSON, metadataJSON, alternativesJSON, claimsJSON, evidenceJSON, relationshipsJSON string
	var validFrom, transactionTime, created, updated string
	var validTo sql.NullString
	if err := row.Scan(
		&d.ID, &d.WorkspaceID, &actorJSON, &d.Type, &d.Title, &d.Outcome, &d.Rationale, &d.Confidence, &d.Status,
		&validFrom, &validTo, &transactionTime, &d.PrecedentRef, &d.SupersedesID, &d.CompletenessScore, &metadataJSON,
		&alternativesJSON, &claimsJSON, &evidenceJSON, &relationshipsJSON, &created, &updated,
	); err != nil {
		return domain.Decision{}, err
	}
	if err := unmarshalJSON(actorJSON, &d.Actor); err != nil {
		return domain.Decision{}, err
	}
	if err := unmarshalJSON(metadataJSON, &d.Metadata); err != nil {
		return domain.Decision{}, err
	}
	if err := unmarshalJSON(alternativesJSON, &d.Alternatives); err != nil {
		return domain.Decision{}, err
	}
	if err := unmarshalJSON(claimsJSON, &d.Claims); err != nil {
		return domain.Decision{}, err
	}
	if err := unmarshalJSON(evidenceJSON, &d.Evidence); err != nil {
		return domain.Decision{}, err
	}
	if err := unmarshalJSON(relationshipsJSON, &d.Relationships); err != nil {
		return domain.Decision{}, err
	}
	d.ValidFrom = parseTime(validFrom)
	if validTo.Valid && strings.TrimSpace(validTo.String) != "" {
		t := parseTime(validTo.String)
		d.ValidTo = &t
	}
	d.TransactionTime = parseTime(transactionTime)
	d.CreatedAt = parseTime(created)
	d.UpdatedAt = parseTime(updated)
	if d.ValidFrom.IsZero() {
		d.ValidFrom = d.CreatedAt
	}
	if d.TransactionTime.IsZero() {
		d.TransactionTime = d.CreatedAt
	}
	return d, nil
}

func scanDecisionCandidate(row rowScanner) (domain.DecisionCandidate, error) {
	var c domain.DecisionCandidate
	var created, updated string
	if err := row.Scan(
		&c.ID, &c.WorkspaceID, &c.Type, &c.Title, &c.Outcome, &c.Rationale, &c.Confidence, &c.SourcePath, &c.SourceTitle,
		&c.SourceSnippet, &c.SourceHash, &c.Detector, &c.Status, &c.AcceptedDecisionID, &c.RejectionSummary, &created, &updated,
	); err != nil {
		return domain.DecisionCandidate{}, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return c, nil
}

func defaultDecisionTimes(d *domain.Decision) {
	now := time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = d.CreatedAt
	}
	if d.ValidFrom.IsZero() {
		d.ValidFrom = d.CreatedAt
	}
	if d.TransactionTime.IsZero() {
		d.TransactionTime = d.CreatedAt
	}
}

func marshalJSON(value any, fallback string) (string, error) {
	if value == nil {
		return fallback, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(data)) == "null" {
		return fallback, nil
	}
	return string(data), nil
}

func unmarshalJSON(value string, out any) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return json.Unmarshal([]byte(value), out)
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, value)
	return t
}

var ftsTokenRE = regexp.MustCompile(`[A-Za-z0-9_]+`)

func searchTokens(query string) []string {
	raw := ftsTokenRE.FindAllString(strings.ToLower(query), -1)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, token := range raw {
		if len(token) < 2 {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
		if len(out) == 12 {
			break
		}
	}
	return out
}

func ftsQuery(query string) string {
	tokens := ftsTokenRE.FindAllString(strings.ToLower(query), -1)
	seen := map[string]struct{}{}
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if len(token) < 2 {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		parts = append(parts, fmt.Sprintf("%q", token))
		if len(parts) == 12 {
			break
		}
	}
	return strings.Join(parts, " OR ")
}
