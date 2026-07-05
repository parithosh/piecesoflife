package store

import (
	"context"
	"fmt"
	"time"
)

// InstanceSettings holds operator-level configuration shared by every Loop
// on this instance. Policy fields act as ceilings: a Loop can only enable
// the matching feature if the instance policy allows it too.
type InstanceSettings struct {
	ID                  int64     `json:"id"`
	InstanceName        string    `json:"instance_name"`
	AllowPublicMementos bool      `json:"allow_public_mementos"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// GetInstanceSettings returns the single instance settings row.
func (s *Store) GetInstanceSettings(ctx context.Context) (*InstanceSettings, error) {
	var st InstanceSettings

	err := s.read.QueryRowContext(ctx,
		`SELECT id, instance_name, allow_public_mementos, created_at, updated_at
		 FROM instance_settings WHERE id = 1`,
	).Scan(&st.ID, &st.InstanceName, &st.AllowPublicMementos,
		&st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting instance settings: %w", err)
	}

	return &st, nil
}

// UpdateInstanceSettings writes the editable instance-level fields.
func (s *Store) UpdateInstanceSettings(
	ctx context.Context, st *InstanceSettings,
) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE instance_settings SET
			instance_name = ?, allow_public_mementos = ?,
			updated_at = CURRENT_TIMESTAMP
		 WHERE id = 1`,
		st.InstanceName, st.AllowPublicMementos,
	)
	if err != nil {
		return fmt.Errorf("updating instance settings: %w", err)
	}

	return nil
}

// SeedInstanceSettings ensures the single instance settings row exists.
func (s *Store) SeedInstanceSettings(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx,
		"INSERT OR IGNORE INTO instance_settings (id) VALUES (1)",
	)
	if err != nil {
		return fmt.Errorf("seeding instance settings: %w", err)
	}

	return nil
}
