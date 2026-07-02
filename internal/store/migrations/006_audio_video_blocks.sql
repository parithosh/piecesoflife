-- Local-only schema rewrite: allow recorded audio and video response blocks.
CREATE TABLE response_blocks_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    response_id INTEGER NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
    type TEXT NOT NULL CHECK(type IN ('text', 'photo', 'audio', 'video', 'link')),
    content TEXT,
    file_path TEXT,
    caption TEXT,
    link_url TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO response_blocks_new
    (id, response_id, type, content, file_path, caption, link_url, sort_order, created_at, updated_at)
SELECT id, response_id, type, content, file_path, caption, link_url, sort_order, created_at, updated_at
FROM response_blocks;

DROP TABLE response_blocks;
ALTER TABLE response_blocks_new RENAME TO response_blocks;

CREATE INDEX IF NOT EXISTS idx_response_blocks_response_sort
    ON response_blocks(response_id, sort_order);
