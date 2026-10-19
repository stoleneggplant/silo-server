package catalog

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Silo-Server/silo-server/internal/models"
)

// A movie library can show the movies of one TMDB collection (a franchise or
// saga) as one box-set card. internal/moviecollections records which
// collection each movie belongs to; browsing reads only those rows, so it
// never waits on TMDB. A box set is the collection's movies the viewer can
// see in the browsed libraries, and it exists only when there are at least
// MovieCollectionMinMembers of them.

// MovieCollectionMinMembers is the fewest visible movies that make a box set.
// TMDB's own collections need two titles, so one local sequel and its
// original already form a set.
const MovieCollectionMinMembers = 2

// movieCollectionMemberLimit bounds one read of collection members. The
// largest TMDB collections hold a few dozen movies, so the bound only guards
// against a page naming very many collections.
const movieCollectionMemberLimit = 1000

// MovieCollection is one TMDB collection as one viewer sees it.
type MovieCollection struct {
	TMDBCollectionID int
	Name             string
	Overview         string
	// Members are the collection's movies the viewer can see, in TMDB's
	// collection order; movies TMDB no longer lists follow by release date.
	Members []*models.MediaItem
}

// IsBoxSet reports whether the viewer sees enough of the collection for it
// to show as a box set.
func (c *MovieCollection) IsBoxSet() bool {
	return c != nil && len(c.Members) >= MovieCollectionMinMembers
}

// MovieCollections returns the given TMDB collections with the members the
// viewer can see in libraryIDs (all of the viewer's libraries when empty).
// Collections that are unknown or have no visible member are absent.
func (r *CatalogResolver) MovieCollections(ctx context.Context, collectionIDs []int, libraryIDs []int, access AccessFilter) (map[int]*MovieCollection, error) {
	collections := map[int]*MovieCollection{}
	ids := slices.Clone(collectionIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	ids = slices.DeleteFunc(ids, func(id int) bool { return id <= 0 })
	if len(ids) == 0 {
		return collections, nil
	}
	if r == nil || r.itemRepo == nil || r.itemRepo.pool == nil {
		return nil, fmt.Errorf("movie collections require a database pool")
	}
	pool := r.itemRepo.pool

	partOrder := map[int]map[int]int{}
	rows, err := pool.Query(ctx, `
		SELECT tmdb_collection_id, name, overview, part_tmdb_ids
		FROM tmdb_movie_collections
		WHERE tmdb_collection_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("loading movie collections: %w", err)
	}
	for rows.Next() {
		var c MovieCollection
		var parts []int32
		if err := rows.Scan(&c.TMDBCollectionID, &c.Name, &c.Overview, &parts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning movie collection: %w", err)
		}
		order := make(map[int]int, len(parts))
		for i, part := range parts {
			if _, seen := order[int(part)]; !seen {
				order[int(part)] = i
			}
		}
		partOrder[c.TMDBCollectionID] = order
		collections[c.TMDBCollectionID] = &c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading movie collections: %w", err)
	}
	if len(collections) == 0 {
		return collections, nil
	}
	known := make([]int, 0, len(collections))
	for id := range collections {
		known = append(known, id)
	}

	// The executor applies the viewer's whole access policy, so a box set
	// never counts or shows a movie the viewer's ordinary browse would hide.
	executor := r.queryExecutorForScope(playableTypeMovie, nil)
	executor.SourceJoin = "JOIN media_item_tmdb_collections movie_collection_source ON movie_collection_source.content_id = mi.content_id AND movie_collection_source.tmdb_id = mi.tmdb_id AND movie_collection_source.tmdb_collection_id = ANY($1)"
	executor.SourceArgs = []any{known}
	def := QueryDefinition{LibraryIDs: libraryIDs, MediaScope: playableTypeMovie, Sort: QuerySort{Field: querySortTitle, Order: querySortAsc}}
	items, _, _, err := executor.PreviewPage(ctx, def, access, movieCollectionMemberLimit, 0, false)
	if err != nil {
		return nil, fmt.Errorf("listing movie collection members: %w", err)
	}
	if len(items) == 0 {
		return map[int]*MovieCollection{}, nil
	}

	contentIDs := make([]string, 0, len(items))
	for _, item := range items {
		contentIDs = append(contentIDs, item.ContentID)
	}
	membership := make(map[string]int, len(items))
	memberRows, err := pool.Query(ctx, `
		SELECT content_id, tmdb_collection_id
		FROM media_item_tmdb_collections
		WHERE content_id = ANY($1) AND tmdb_collection_id IS NOT NULL`, contentIDs)
	if err != nil {
		return nil, fmt.Errorf("loading movie collection memberships: %w", err)
	}
	for memberRows.Next() {
		var contentID string
		var collectionID int
		if err := memberRows.Scan(&contentID, &collectionID); err != nil {
			memberRows.Close()
			return nil, fmt.Errorf("scanning movie collection membership: %w", err)
		}
		membership[contentID] = collectionID
	}
	memberRows.Close()
	if err := memberRows.Err(); err != nil {
		return nil, fmt.Errorf("loading movie collection memberships: %w", err)
	}
	for _, item := range items {
		if c := collections[membership[item.ContentID]]; c != nil {
			c.Members = append(c.Members, item)
		}
	}
	for id, c := range collections {
		if len(c.Members) == 0 {
			delete(collections, id)
			continue
		}
		sortMovieCollectionMembers(c.Members, partOrder[id])
	}
	return collections, nil
}

// MovieCollectionIDs returns the TMDB collection each of the given movies
// currently belongs to; movies in none, and stale memberships, are absent.
func (r *CatalogResolver) MovieCollectionIDs(ctx context.Context, contentIDs []string) (map[string]int, error) {
	ids := map[string]int{}
	if len(contentIDs) == 0 {
		return ids, nil
	}
	if r == nil || r.itemRepo == nil || r.itemRepo.pool == nil {
		return nil, fmt.Errorf("movie collections require a database pool")
	}
	rows, err := r.itemRepo.pool.Query(ctx, `
		SELECT mitc.content_id, mitc.tmdb_collection_id
		FROM media_item_tmdb_collections mitc
		JOIN media_items mi ON mi.content_id = mitc.content_id AND mi.tmdb_id = mitc.tmdb_id
		WHERE mitc.content_id = ANY($1) AND mitc.tmdb_collection_id IS NOT NULL AND mi.type = 'movie'`, contentIDs)
	if err != nil {
		return nil, fmt.Errorf("loading movie collection ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var contentID string
		var collectionID int
		if err := rows.Scan(&contentID, &collectionID); err != nil {
			return nil, fmt.Errorf("scanning movie collection id: %w", err)
		}
		ids[contentID] = collectionID
	}
	return ids, rows.Err()
}

// sortMovieCollectionMembers puts members in TMDB's collection order. A
// member TMDB does not list (its collection changed since the last fetch)
// follows the listed ones, by release date and then title.
func sortMovieCollectionMembers(members []*models.MediaItem, partOrder map[int]int) {
	position := func(item *models.MediaItem) (int, bool) {
		raw := strings.TrimSpace(item.TmdbID)
		if i := strings.IndexByte(raw, '-'); i >= 0 {
			raw = raw[:i]
		}
		id, err := strconv.Atoi(raw)
		if err != nil {
			return 0, false
		}
		pos, ok := partOrder[id]
		return pos, ok
	}
	slices.SortStableFunc(members, func(a, b *models.MediaItem) int {
		pa, okA := position(a)
		pb, okB := position(b)
		switch {
		case okA && okB:
			if c := cmp.Compare(pa, pb); c != 0 {
				return c
			}
		case okA:
			return -1
		case okB:
			return 1
		}
		if c := cmp.Compare(movieCollectionReleaseKey(a), movieCollectionReleaseKey(b)); c != 0 {
			return c
		}
		if c := cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
			return c
		}
		return cmp.Compare(a.ContentID, b.ContentID)
	})
}

// movieCollectionReleaseKey orders by release date when known, else by year;
// an undated movie sorts last.
func movieCollectionReleaseKey(item *models.MediaItem) string {
	if item.ReleaseDate != nil && *item.ReleaseDate != "" {
		return *item.ReleaseDate
	}
	if item.Year > 0 {
		return fmt.Sprintf("%04d", item.Year)
	}
	return "9999"
}

// LibraryMovieCollectionGroupingDefault returns a library's
// movie_collection_grouping_default when the viewer can browse the library,
// and ErrCatalogSourceNotFound otherwise.
func (r *CatalogResolver) LibraryMovieCollectionGroupingDefault(ctx context.Context, libraryID int, access AccessFilter) (string, error) {
	if r == nil || r.itemRepo == nil || r.itemRepo.pool == nil {
		return "", fmt.Errorf("movie collections require a database pool")
	}
	if _, none := access.LibraryScope([]int{libraryID}); none {
		return "", ErrCatalogSourceNotFound
	}
	var value string
	err := r.itemRepo.pool.QueryRow(ctx, `SELECT movie_collection_grouping_default FROM media_folders WHERE id = $1 AND enabled IS NOT FALSE`, libraryID).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCatalogSourceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("loading library movie collection grouping: %w", err)
	}
	return value, nil
}
