-- +goose Up
-- TMDB movie collections (franchises and sagas such as "The Matrix
-- Collection") that movie libraries can show as one box-set card. A background
-- task fills both tables from TMDB; browsing only reads them, so it never
-- waits on TMDB.
CREATE TABLE tmdb_movie_collections (
    tmdb_collection_id integer PRIMARY KEY,
    name text NOT NULL,
    overview text NOT NULL DEFAULT '',
    -- The collection's movies as TMDB orders them, which is the order a box
    -- set lists its members in.
    part_tmdb_ids integer[] NOT NULL DEFAULT '{}',
    fetched_at timestamptz NOT NULL
);

-- Which TMDB collection a movie belongs to. tmdb_id is the movie's TMDB id at
-- the time it was checked: a movie matched again to another title no longer
-- equals its row, so readers ignore the row and the task checks it again.
-- tmdb_collection_id NULL records that the movie belongs to no collection.
CREATE TABLE media_item_tmdb_collections (
    content_id text PRIMARY KEY REFERENCES media_items(content_id) ON DELETE CASCADE,
    tmdb_id text NOT NULL,
    tmdb_collection_id integer,
    checked_at timestamptz NOT NULL
);

CREATE INDEX media_item_tmdb_collections_collection_idx
    ON media_item_tmdb_collections (tmdb_collection_id)
    WHERE tmdb_collection_id IS NOT NULL;

-- Whether a movie library's Browse shows box sets for viewers who have not
-- chosen. Existing and new libraries stay flat until an admin turns it on.
ALTER TABLE media_folders
    ADD COLUMN movie_collection_grouping_default text NOT NULL DEFAULT 'flat'
        CONSTRAINT media_folders_movie_collection_grouping_default_check
        CHECK (movie_collection_grouping_default IN ('grouped', 'flat'));

-- +goose Down
ALTER TABLE media_folders DROP COLUMN IF EXISTS movie_collection_grouping_default;
DROP TABLE IF EXISTS media_item_tmdb_collections;
DROP TABLE IF EXISTS tmdb_movie_collections;
