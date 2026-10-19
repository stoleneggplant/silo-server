// Package moviecollections records which TMDB collection (franchise or saga)
// each movie belongs to, so movie libraries can show a collection as one
// box-set card. The catalog reads what this package writes; browsing never
// calls TMDB.
package moviecollections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/database/pglock"
	"github.com/Silo-Server/silo-server/internal/metadata/tmdb"
)

// refreshLockKey lets one server at a time check memberships, so a cluster
// spends one TMDB budget rather than one per node.
const refreshLockKey int64 = 0x53494c4f4d4343

const (
	// membershipRecheckAfter is how long a movie's answer stands. TMDB adds a
	// movie to a collection when a sequel is announced, so a movie in none is
	// asked again eventually.
	membershipRecheckAfter = 30 * 24 * time.Hour
	// collectionRefetchAfter is how long a collection's name and member order
	// stand before the next membership that names it fetches them again.
	collectionRefetchAfter = 7 * 24 * time.Hour
	// maxMoviesPerRun bounds one run's TMDB calls; the next run continues.
	maxMoviesPerRun = 5000
	batchSize       = 200
	// maxConsecutiveFailures stops a run once TMDB looks unreachable instead
	// of spending the rest of the batch on the same error.
	maxConsecutiveFailures = 20
)

// TMDB is the slice of the TMDB client the refresher calls.
type TMDB interface {
	GetMovieCollectionID(ctx context.Context, movieID int) (int, error)
	GetCollection(ctx context.Context, id int) (*tmdb.Collection, error)
}

// Result is what one refresh did.
type Result struct {
	// Skipped: another server holds the refresh.
	Skipped bool `json:"skipped,omitempty"`
	// Checked counts movies whose membership was recorded.
	Checked int `json:"checked"`
	// InCollection counts the checked movies that belong to a collection.
	InCollection int `json:"in_collection"`
	// Collections counts collections whose details were fetched.
	Collections int `json:"collections"`
	// Failed counts movies TMDB could not answer for; they are tried again
	// on the next run.
	Failed int `json:"failed"`
}

// Refresher checks movies whose membership is unknown or stale.
type Refresher struct {
	pool  *pgxpool.Pool
	store *Store
	tmdb  TMDB
}

// NewRefresher returns a refresher that reads TMDB through client.
func NewRefresher(pool *pgxpool.Pool, client TMDB) *Refresher {
	return &Refresher{pool: pool, store: NewStore(pool), tmdb: client}
}

// Refresh checks up to maxMoviesPerRun movies, unless another server is
// already doing so.
func (r *Refresher) Refresh(ctx context.Context) (Result, error) {
	if r == nil || r.pool == nil || r.tmdb == nil {
		return Result{}, errors.New("movie collections: refresher not configured")
	}
	lock, acquired, err := pglock.TryAcquire(ctx, r.pool, refreshLockKey)
	if err != nil {
		return Result{}, err
	}
	if !acquired {
		return Result{Skipped: true}, nil
	}
	defer func() {
		if err := lock.Release(context.WithoutCancel(ctx)); err != nil {
			slog.WarnContext(ctx, "releasing movie collection refresh lock", "component", "moviecollections", "error", err)
		}
	}()

	var result Result
	fetched := map[int]bool{}
	failures := 0
	// Movies TMDB failed for stay pending; exclude them so the run moves on.
	var skip []string
	for result.Checked+result.Failed < maxMoviesPerRun {
		movies, err := r.store.PendingMovies(ctx, membershipRecheckAfter, skip, batchSize)
		if err != nil {
			return result, err
		}
		if len(movies) == 0 {
			return result, nil
		}
		for _, movie := range movies {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			collectionID, err := r.checkMovie(ctx, movie)
			if err != nil {
				result.Failed++
				failures++
				skip = append(skip, movie.ContentID)
				slog.WarnContext(ctx, "movie collections: membership lookup failed", "content_id", movie.ContentID, "tmdb_id", movie.TMDBID, "error", err)
				if failures >= maxConsecutiveFailures {
					return result, fmt.Errorf("movie collections: stopping after %d consecutive TMDB failures: %w", failures, err)
				}
				continue
			}
			failures = 0
			if collectionID > 0 && !fetched[collectionID] {
				fetched[collectionID] = true
				refreshed, err := r.refreshCollection(ctx, collectionID)
				if err != nil {
					// The membership is still worth keeping: without a collection
					// row the catalog treats the movie as ungrouped until the
					// next run fetches it.
					slog.WarnContext(ctx, "movie collections: collection fetch failed", "tmdb_collection_id", collectionID, "error", err)
				} else if refreshed {
					result.Collections++
				}
			}
			if err := r.store.RecordMembership(ctx, movie.ContentID, movie.TMDBID, collectionID); err != nil {
				return result, err
			}
			result.Checked++
			if collectionID > 0 {
				result.InCollection++
			}
		}
	}
	return result, nil
}

// checkMovie asks TMDB which collection the movie belongs to. A TMDB id that
// TMDB no longer has belongs to no collection.
func (r *Refresher) checkMovie(ctx context.Context, movie PendingMovie) (int, error) {
	id, ok := ParseMovieTMDBID(movie.TMDBID)
	if !ok {
		return 0, nil
	}
	collectionID, err := r.tmdb.GetMovieCollectionID(ctx, id)
	if errors.Is(err, tmdb.ErrNotFound) {
		return 0, nil
	}
	return collectionID, err
}

// refreshCollection fetches the collection's details when they are missing or
// stale, and reports whether it fetched them.
func (r *Refresher) refreshCollection(ctx context.Context, collectionID int) (bool, error) {
	fresh, err := r.store.CollectionFresh(ctx, collectionID, collectionRefetchAfter)
	if err != nil || fresh {
		return false, err
	}
	collection, err := r.tmdb.GetCollection(ctx, collectionID)
	if err != nil {
		return false, err
	}
	partIDs := make([]int, 0, len(collection.Parts))
	for _, part := range collection.Parts {
		if part.ID > 0 {
			partIDs = append(partIDs, part.ID)
		}
	}
	name := strings.TrimSpace(collection.Name)
	if name == "" {
		return false, fmt.Errorf("tmdb collection %d has no name", collectionID)
	}
	return true, r.store.UpsertCollection(ctx, collectionID, name, strings.TrimSpace(collection.Overview), partIDs)
}

// ParseMovieTMDBID reads the TMDB id a movie's tmdb_id column holds. The
// column is the bare number, sometimes followed by a "-slug".
func ParseMovieTMDBID(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '-'); i >= 0 {
		raw = raw[:i]
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
