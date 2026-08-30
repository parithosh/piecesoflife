package store

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckpointTruncatesWAL is the guard against the 99MB -wal beside a
// 7.9MB database seen in production. A TRUNCATE checkpoint must fold WAL
// frames back into the main file and reset the -wal to zero length, so an
// operator copying the bare .db afterwards gets a complete database.
func TestCheckpointTruncatesWAL(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	walPath := st.dbPath + "-wal"

	for i := range 200 {
		_, err := st.write.ExecContext(ctx,
			"INSERT INTO question_bank (group_id, text, category) VALUES (1, ?, 'test')",
			"wal filler question number "+string(rune('a'+i%26))+string(rune('a'+i/26)),
		)
		require.NoError(t, err)
	}

	before, err := os.Stat(walPath)
	require.NoError(t, err, "WAL must exist after writes")
	require.Positive(t, before.Size(), "WAL must have frames to reclaim")

	require.NoError(t, st.Checkpoint(ctx))

	after, err := os.Stat(walPath)
	require.NoError(t, err)

	assert.Zero(t, after.Size(),
		"TRUNCATE checkpoint must reset the WAL to zero length")

	// The data must survive the checkpoint — it moved into the main file,
	// it did not disappear.
	var count int
	require.NoError(t, st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM question_bank WHERE category = 'test'",
	).Scan(&count))
	assert.Equal(t, 200, count)
}
