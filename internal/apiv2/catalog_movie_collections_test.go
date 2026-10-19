package apiv2

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/ratingsources"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
)

func (f *fakeCatalog) MovieCollection(_ context.Context, v handlers.ItemViewer, id int, libraryIDs []int) (handlers.MovieCollectionView, error) {
	if f.err != nil {
		return handlers.MovieCollectionView{}, f.err
	}
	f.lastViewer = v
	f.lastMovieCollectionLibraries = libraryIDs
	if id != 2344 {
		return handlers.MovieCollectionView{}, &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Movie collection not found"}
	}
	return handlers.MovieCollectionView{TMDBCollectionID: 2344, Title: "The Matrix Collection", Overview: "Neo.", PlayedCount: 1,
		Items: []handlers.CollectionItemView{fakeListingCard("movie:the-matrix-1999"), fakeListingCard("movie:the-matrix-reloaded-2003")}}, nil
}

func (f *fakeCatalog) LibraryMovieCollectionGroupingDefault(_ context.Context, _ handlers.ItemViewer, libraryID int) (string, error) {
	if libraryID != 1 {
		return "", &handlers.APIError{Status: http.StatusNotFound, Code: "not_found", Message: "Catalog source not found"}
	}
	return "grouped", nil
}

func TestListCatalogItemsGroupsMovieCollections(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog?group=tmdb_collection&library_id=1", "", viewerHeaders())
	if rec.Code != 200 || !fake.lastReq.GroupByMovieCollection || fake.lastGroup {
		t.Fatalf("group=tmdb_collection: %d movie=%v work=%v %s", rec.Code, fake.lastReq.GroupByMovieCollection, fake.lastGroup, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/api/v2/catalog?library_id=1", "", viewerHeaders())
	if rec.Code != 200 || fake.lastReq.GroupByMovieCollection {
		t.Fatalf("ungrouped browse grouped: %d", rec.Code)
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog?group=box_set", "", viewerHeaders()), TypeValidationFailed)
}

func TestCatalogItemOfListingMovieCollection(t *testing.T) {
	card := fakeListingCard("movie:the-matrix-1999")
	card.Type = "movie_collection"
	card.MovieCollection = &handlers.MovieCollectionCardView{TMDBCollectionID: 2344, Title: "The Matrix Collection", MemberCount: 3, PlayedCount: 1}
	item := catalogItemOfListing(card, ratingsources.Selection{})
	if item.MovieCollection == nil || item.MovieCollection.ID != "2344" || item.MovieCollection.MemberCount != 3 || item.MovieCollection.PlayedCount != 1 || item.Type != "movie_collection" {
		t.Fatalf("card = %+v", item.MovieCollection)
	}
	if item := catalogItemOfListing(fakeListingCard("movie:heat-1995"), ratingsources.Selection{}); item.MovieCollection != nil {
		t.Fatalf("plain card carries a box set: %+v", item.MovieCollection)
	}
}

func TestGetMovieCollection(t *testing.T) {
	deps, fake := catalogDeps(t)
	h := newTestHandler(t, deps)
	rec := do(t, h, http.MethodGet, "/api/v2/catalog/movie-collections/2344?library_id=1", "", viewerHeaders())
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var body MovieCollection
	decodeJSON(t, rec.Body, &body)
	if body.ID != "2344" || body.Title != "The Matrix Collection" || body.MemberCount != 2 || body.PlayedCount != 1 || len(body.Items) != 2 || body.Items[0].ContentID != "movie:the-matrix-1999" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if len(fake.lastMovieCollectionLibraries) != 1 || fake.lastMovieCollectionLibraries[0] != 1 {
		t.Fatalf("libraries = %v", fake.lastMovieCollectionLibraries)
	}
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/movie-collections/99", "", viewerHeaders()), TypeNotFound)
	requireProblem(t, do(t, h, http.MethodGet, "/api/v2/catalog/movie-collections/abc", "", viewerHeaders()), TypeValidationFailed)
}

func TestResolveMovieCollectionGrouping(t *testing.T) {
	setting := func(value, source string) []handlers.EffectiveSettingValueView {
		return []handlers.EffectiveSettingValueView{{Key: settingskeys.UiMovieCollectionGrouping, Value: json.RawMessage(value), Source: source}}
	}
	cases := []struct {
		name           string
		libraryDefault string
		views          []handlers.EffectiveSettingValueView
		want           MovieCollectionGrouping
	}{
		{"unset follows the library", "grouped", setting(`null`, "default"), MovieCollectionGrouping{"grouped", "grouped", "library_default"}},
		{"unknown library value is flat", "", nil, MovieCollectionGrouping{"flat", "flat", "library_default"}},
		{"profile overrides the library", "grouped", setting(`"flat"`, "profile"), MovieCollectionGrouping{"grouped", "flat", "profile"}},
		{"library override wins", "flat", setting(`"grouped"`, "profile_library"), MovieCollectionGrouping{"flat", "grouped", "profile_library"}},
	}
	for _, tc := range cases {
		if got := resolveMovieCollectionGrouping(tc.libraryDefault, tc.views); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
