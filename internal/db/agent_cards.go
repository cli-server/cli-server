package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

// AgentCard stores an agent's capability declaration.
type AgentCard struct {
	SandboxID   string
	WorkspaceID string
	AgentType   string
	DisplayName string
	Description string
	CardJSON    json.RawMessage
	AgentStatus string
	Version     int
	UpdatedAt   time.Time
}

func (db *DB) UpsertAgentCard(card *AgentCard) error {
	_, err := db.Exec(
		`INSERT INTO agent_cards (sandbox_id, workspace_id, agent_type, display_name, description, card_json, agent_status, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 1)
		 ON CONFLICT (sandbox_id) DO UPDATE SET
		   display_name = EXCLUDED.display_name,
		   description = EXCLUDED.description,
		   card_json = EXCLUDED.card_json,
		   agent_status = EXCLUDED.agent_status,
		   version = agent_cards.version + 1,
		   updated_at = NOW()`,
		card.SandboxID, card.WorkspaceID, card.AgentType, card.DisplayName, card.Description, card.CardJSON, card.AgentStatus,
	)
	return err
}

func (db *DB) GetAgentCard(sandboxID string) (*AgentCard, error) {
	c := &AgentCard{}
	err := db.QueryRow(
		`SELECT sandbox_id, workspace_id, agent_type, display_name, description, card_json, agent_status, version, updated_at
		 FROM agent_cards WHERE sandbox_id = $1`, sandboxID,
	).Scan(&c.SandboxID, &c.WorkspaceID, &c.AgentType, &c.DisplayName, &c.Description, &c.CardJSON, &c.AgentStatus, &c.Version, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (db *DB) ListAgentCardsByWorkspace(workspaceID string) ([]AgentCard, error) {
	rows, err := db.Query(
		`SELECT sandbox_id, workspace_id, agent_type, display_name, description, card_json, agent_status, version, updated_at
		 FROM agent_cards WHERE workspace_id = $1 ORDER BY display_name`, workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cards []AgentCard
	for rows.Next() {
		var c AgentCard
		if err := rows.Scan(&c.SandboxID, &c.WorkspaceID, &c.AgentType, &c.DisplayName, &c.Description, &c.CardJSON, &c.AgentStatus, &c.Version, &c.UpdatedAt); err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	return cards, rows.Err()
}

func (db *DB) UpdateAgentCardStatus(sandboxID, status string) error {
	_, err := db.Exec(
		`UPDATE agent_cards SET agent_status = $2, updated_at = NOW() WHERE sandbox_id = $1`,
		sandboxID, status,
	)
	return err
}

func (db *DB) DeleteAgentCard(sandboxID string) error {
	_, err := db.Exec(`DELETE FROM agent_cards WHERE sandbox_id = $1`, sandboxID)
	return err
}

// UpsertAgentCardFromCapabilities creates or updates an agent card from capability data.
func (db *DB) UpsertAgentCardFromCapabilities(sandboxID, workspaceID, displayName string, cardJSON json.RawMessage) error {
	_, err := db.Exec(
		`INSERT INTO agent_cards (sandbox_id, workspace_id, agent_type, display_name, card_json, agent_status, version)
		 VALUES ($1, $2, 'claudecode', $3, $4, 'available', 1)
		 ON CONFLICT (sandbox_id) DO UPDATE SET
		   card_json = EXCLUDED.card_json,
		   agent_status = 'available',
		   version = agent_cards.version + 1,
		   updated_at = NOW()`,
		sandboxID, workspaceID, displayName, cardJSON,
	)
	return err
}

// MarkStaleAgentCardsOffline marks agents as offline if their sandbox heartbeat is stale.
func (db *DB) MarkStaleAgentCardsOffline(threshold time.Duration) (int64, error) {
	result, err := db.Exec(
		`UPDATE agent_cards SET agent_status = 'offline', updated_at = NOW()
		 WHERE agent_status != 'offline'
		   AND sandbox_id NOT IN (
		     SELECT id FROM sandboxes WHERE last_heartbeat_at > NOW() - $1::interval
		   )`,
		threshold.String(),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
