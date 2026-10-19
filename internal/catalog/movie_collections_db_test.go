package catalog

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/access"
)

func TestMovieCollectionGroupingDB(t *testing.T) {
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SILO_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("boxset-%d", time.Now().UnixNano())
	// TMDB collection ids are global, so derive ones no other run uses.
	collectionID := int(time.Now().UnixNano()%1_000_000_000) + 1
	var lib, otherLib int
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type,name,enabled,movie_collection_grouping_default) VALUES('movies',$1,true,'grouped') RETURNING id`, prefix).Scan(&lib); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO media_folders(type,name,enabled) VALUES('movies',$1,true) RETURNING id`, prefix+"-other").Scan(&otherLib); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_items WHERE content_id LIKE $1`, prefix+"%")
		_, _ = pool.Exec(context.Background(), `DELETE FROM tmdb_movie_collections WHERE tmdb_collection_id = $1`, collectionID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM media_folders WHERE id = ANY($1)`, []int{lib, otherLib})
	}()
	type movie struct {
		suffix, title, tmdbID, rating, release string
		library                                int
		// member is the tmdb_id recorded for the collection; empty records none.
		member string
	}
	movies := []movie{
		{"matrix", "The Matrix", "603", "PG", "1999-03-31", lib, "603"},
		{"reloaded", "The Matrix Reloaded", "604", "PG", "2003-05-15", lib, "604"},
		// Above the viewer's rating ceiling: never counted or shown.
		{"revolutions", "The Matrix Revolutions", "605", "R", "2003-11-05", lib, "605"},
		// In another library than the one browsed.
		{"resurrections", "The Matrix Resurrections", "624860", "PG", "2021-12-22", otherLib, "624860"},
		// Matched to another title since its membership was recorded.
		{"animatrix", "Animatrix", "55931", "PG", "2003-06-03", lib, "1"},
		{"heat", "Heat", "949", "PG", "1995-12-15", lib, ""},
	}
	id := func(suffix string) string { return prefix + "-" + suffix }
	for _, m := range movies {
		age := 8
		if m.rating == "R" {
			age = 17
		}
		exec(`INSERT INTO media_items(content_id,type,title,status,genres,content_rating,content_rating_age,tmdb_id,release_date,year,created_at) VALUES($1,'movie',$2,'matched','{}',$3,$4,$5,$6::date,EXTRACT(YEAR FROM $6::date)::int,'2025-01-01'::timestamptz)`,
			id(m.suffix), m.title, m.rating, age, m.tmdbID, m.release)
		exec(`INSERT INTO media_item_libraries(content_id,media_folder_id) VALUES($1,$2)`, id(m.suffix), m.library)
		if m.member != "" {
			exec(`INSERT INTO media_item_tmdb_collections(content_id,tmdb_id,tmdb_collection_id,checked_at) VALUES($1,$2,$3,NOW())`, id(m.suffix), m.member, collectionID)
		} else {
			exec(`INSERT INTO media_item_tmdb_collections(content_id,tmdb_id,tmdb_collection_id,checked_at) VALUES($1,$2,NULL,NOW())`, id(m.suffix), m.tmdbID)
		}
	}
	// TMDB lists Reloaded before The Matrix here so the test can tell
	// collection order from title or release order.
	exec(`INSERT INTO tmdb_movie_collections(tmdb_collection_id,name,overview,part_tmdb_ids,fetched_at) VALUES($1,'The Matrix Collection','Neo.','{604,603,605,624860}',NOW())`, collectionID)

	viewer := AccessFilter{UserID: 1, ProfileID: prefix, AllowedLibraryIDs: []int{lib, otherLib}, MaturityLimits: access.MaturityLimits{MaxContentRating: "PG"}}
	def := QueryDefinition{LibraryIDs: []int{lib}, MediaScope: "movie", Sort: QuerySort{Field: "title", Order: "asc"}}

	t.Run("browse collapses a collection into its first title", func(t *testing.T) {
		grouped := &QueryExecutor{Pool: pool, Scope: "movie", GroupByMovieCollection: true}
		var got []string
		var after *QueryCursor
		for range 5 {
			page, err := grouped.PreviewCursorPage(ctx, def, viewer, 1, after, true)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 3 {
				t.Fatalf("total = %d, want 3", page.Total)
			}
			for _, item := range page.Items {
				got = append(got, item.ContentID)
			}
			if !page.HasMore {
				break
			}
			after = page.Next
		}
		want := []string{id("animatrix"), id("heat"), id("matrix")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("grouped page = %v, want %v", got, want)
		}
		desc := def
		desc.Sort.Order = "desc"
		page, err := grouped.PreviewCursorPage(ctx, desc, viewer, 10, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		// In descending title order the collection surfaces through Reloaded.
		if len(page.Items) != 3 || page.Items[0].ContentID != id("reloaded") {
			t.Fatalf("descending grouped page = %v", page.Items)
		}
	})

	resolver := NewCatalogResolver(NewBrowseRepository(pool), NewItemRepository(pool))

	t.Run("box set lists visible members in collection order", func(t *testing.T) {
		collections, err := resolver.MovieCollections(ctx, []int{collectionID, collectionID + 1}, []int{lib}, viewer)
		if err != nil {
			t.Fatal(err)
		}
		c := collections[collectionID]
		if len(collections) != 1 || c == nil || c.Name != "The Matrix Collection" || c.Overview != "Neo." || !c.IsBoxSet() {
			t.Fatalf("collections = %+v", collections)
		}
		var got []string
		for _, m := range c.Members {
			got = append(got, m.ContentID)
		}
		if want := []string{id("reloaded"), id("matrix")}; !reflect.DeepEqual(got, want) {
			t.Fatalf("members = %v, want %v", got, want)
		}
		// Across both libraries Resurrections joins the set.
		collections, err = resolver.MovieCollections(ctx, []int{collectionID}, nil, viewer)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(collections[collectionID].Members); n != 3 {
			t.Fatalf("members across libraries = %d, want 3", n)
		}
		// One visible movie is not a box set.
		collections, err = resolver.MovieCollections(ctx, []int{collectionID}, []int{otherLib}, viewer)
		if err != nil {
			t.Fatal(err)
		}
		if collections[collectionID].IsBoxSet() {
			t.Fatal("a single visible movie forms a box set")
		}
	})

	t.Run("membership ignores stale and missing rows", func(t *testing.T) {
		ids, err := resolver.MovieCollectionIDs(ctx, []string{id("matrix"), id("animatrix"), id("heat")})
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]int{id("matrix"): collectionID}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("membership = %v, want %v", ids, want)
		}
	})

	t.Run("library default", func(t *testing.T) {
		value, err := resolver.LibraryMovieCollectionGroupingDefault(ctx, lib, viewer)
		if err != nil || value != "grouped" {
			t.Fatalf("default = %q, %v", value, err)
		}
		value, err = resolver.LibraryMovieCollectionGroupingDefault(ctx, otherLib, viewer)
		if err != nil || value != "flat" {
			t.Fatalf("other default = %q, %v", value, err)
		}
		restricted := viewer
		restricted.AllowedLibraryIDs = []int{otherLib}
		if _, err := resolver.LibraryMovieCollectionGroupingDefault(ctx, lib, restricted); err == nil {
			t.Fatal("a library outside the viewer's scope answered")
		}
	})
}
