package moviecollections

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PendingMovie is a movie whose collection membership is unknown or stale.
type PendingMovie struct {
	ContentID string
	TMDBID    string
}

// Store writes movie collection memberships and collection details.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a store over pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// PendingMovies returns up to limit movies with a TMDB id whose membership
// was never recorded, was recorded for another TMDB id, or is older than
// recheckAfter, never-checked movies first. Content ids in skip are left out.
func (s *Store) PendingMovies(ctx context.Context, recheckAfter time.Duration, skip []string, limit int) ([]PendingMovie, error) {
	if skip == nil {
		skip = []string{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT mi.content_id, mi.tmdb_id
		FROM media_items mi
		LEFT JOIN media_item_tmdb_collections mitc ON mitc.content_id = mi.content_id
		WHERE mi.type = 'movie'
		  AND COALESCE(mi.tmdb_id, '') <> ''
		  AND (mitc.content_id IS NULL
		       OR mitc.tmdb_id <> mi.tmdb_id
		       OR mitc.checked_at < NOW() - make_interval(secs => $1))
		  AND NOT (mi.content_id = ANY($2::text[]))
		ORDER BY mitc.checked_at ASC NULLS FIRST, mi.content_id
		LIMIT $3`, recheckAfter.Seconds(), skip, limit)
	if err != nil {
		return nil, fmt.Errorf("listing movies pending collection check: %w", err)
	}
	defer rows.Close()
	var movies []PendingMovie
	for rows.Next() {
		var m PendingMovie
		if err := rows.Scan(&m.ContentID, &m.TMDBID); err != nil {
			return nil, fmt.Errorf("scanning movie pending collection check: %w", err)
		}
		movies = append(movies, m)
	}
	return movies, rows.Err()
}

// RecordMembership stores that the movie, under tmdbID, belongs to
// collectionID, or to no collection when collectionID is zero. A movie
// deleted since it was listed is ignored.
func (s *Store) RecordMembership(ctx context.Context, contentID, tmdbID string, collectionID int) error {
	var collection *int
	if collectionID > 0 {
		collection = &collectionID
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO media_item_tmdb_collections (content_id, tmdb_id, tmdb_collection_id, checked_at)
		SELECT content_id, $2, $3, NOW() FROM media_items WHERE content_id = $1
		ON CONFLICT (content_id) DO UPDATE
		SET tmdb_id = EXCLUDED.tmdb_id,
		    tmdb_collection_id = EXCLUDED.tmdb_collection_id,
		    checked_at = EXCLUDED.checked_at`, contentID, tmdbID, collection); err != nil {
		return fmt.Errorf("recording movie collection membership: %w", err)
	}
	return nil
}

// CollectionFresh reports whether the collection's details were fetched
// within maxAge.
func (s *Store) CollectionFresh(ctx context.Context, collectionID int, maxAge time.Duration) (bool, error) {
	var fresh bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM tmdb_movie_collections
			WHERE tmdb_collection_id = $1 AND fetched_at >= NOW() - make_interval(secs => $2)
		)`, collectionID, maxAge.Seconds()).Scan(&fresh); err != nil {
		return false, fmt.Errorf("checking tmdb collection freshness: %w", err)
	}
	return fresh, nil
}

// UpsertCollection stores a collection's details and its members in TMDB
// order.
func (s *Store) UpsertCollection(ctx context.Context, collectionID int, name, overview string, partTMDBIDs []int) error {
	if partTMDBIDs == nil {
		partTMDBIDs = []int{}
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tmdb_movie_collections (tmdb_collection_id, name, overview, part_tmdb_ids, fetched_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (tmdb_collection_id) DO UPDATE
		SET name = EXCLUDED.name,
		    overview = EXCLUDED.overview,
		    part_tmdb_ids = EXCLUDED.part_tmdb_ids,
		    fetched_at = EXCLUDED.fetched_at`, collectionID, name, overview, partTMDBIDs); err != nil {
		return fmt.Errorf("storing tmdb collection: %w", err)
	}
	return nil
}
