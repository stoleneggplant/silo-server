package apiv2

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/settingskeys"
)

// groupTMDBCollection is the listCatalogItems group value that shows a TMDB
// collection's movies as one box-set card.
const groupTMDBCollection = "tmdb_collection"

// movieCollectionGroupingFromLibrary is MovieCollectionGrouping.Source when
// no profile value applies.
const movieCollectionGroupingFromLibrary = "library_default"

// CatalogMovieCollectionCard is the box set a type=movie_collection card
// stands for.
type CatalogMovieCollectionCard struct {
	ID          ID     `json:"id" doc:"The TMDB collection id; pass it to getMovieCollection" example:"2344"`
	Title       string `json:"title" example:"The Matrix Collection"`
	MemberCount int    `json:"member_count" doc:"Movies of the collection the viewer can see in the browsed libraries; at least 2" example:"3"`
	PlayedCount int    `json:"played_count" doc:"How many of those movies the viewer has watched" example:"1"`
}

// MovieCollectionInput names one box set.
type MovieCollectionInput struct {
	ID        ID     `path:"id" doc:"TMDB collection id, as movie_collection.id on a card" example:"2344"`
	LibraryID ID     `query:"library_id" doc:"The library being browsed; absent includes every library the viewer can see" example:"1"`
	ImageSize string `query:"image_size" enum:"small,medium,large,original" doc:"Artwork variant to presign"`
}

// MovieCollection is one box set with its movies.
type MovieCollection struct {
	ID          ID            `json:"id" doc:"The TMDB collection id" example:"2344"`
	Title       string        `json:"title" example:"The Matrix Collection"`
	Overview    string        `json:"overview,omitempty"`
	MemberCount int           `json:"member_count" doc:"len(items)" example:"3"`
	PlayedCount int           `json:"played_count" doc:"How many of the movies the viewer has watched" example:"1"`
	Items       []CatalogItem `json:"items" doc:"The collection's movies the viewer can see, in TMDB's collection order; movies TMDB no longer lists follow by release date"`
}

// MovieCollectionOutput is the getMovieCollection response.
type MovieCollectionOutput struct {
	Body MovieCollection
}

// MovieCollectionGroupingInput names the library whose grouping to resolve.
type MovieCollectionGroupingInput struct {
	LibraryID ID `query:"library_id" required:"true" doc:"The movie library being browsed" example:"1"`
}

// MovieCollectionGrouping is whether a library's Browse shows box sets for
// the acting profile, and why.
type MovieCollectionGrouping struct {
	LibraryDefault string `json:"library_default" enum:"grouped,flat" doc:"The library's movie_collection_grouping_default, set by an administrator" example:"flat"`
	Effective      string `json:"effective" enum:"grouped,flat" doc:"What to browse the library with: grouped means listCatalogItems with group=tmdb_collection" example:"grouped"`
	Source         string `json:"source" enum:"profile_library,profile,library_default" doc:"Where effective came from: the profile's ui.movie_collection_grouping value for this library, its value for every library, or the library default" example:"profile"`
}

// MovieCollectionGroupingOutput is the getMovieCollectionGrouping response.
type MovieCollectionGroupingOutput struct {
	Body MovieCollectionGrouping
}

func registerMovieCollections(reg *Registry) {
	Register(reg, viewerOperation(humaOp(http.MethodGet, Prefix+"/catalog/movie-collections/{id}", "getMovieCollection", "catalog",
		"One box set: the movies of a TMDB collection the viewer can see, in collection order.")), reg.getMovieCollection)
	Register(reg, viewerOperation(humaOp(http.MethodGet, Prefix+"/catalog/movie-collection-grouping", "getMovieCollectionGrouping", "catalog",
		"Whether a movie library's Browse shows box sets for the acting profile.")), reg.getMovieCollectionGrouping)
}

func (reg *Registry) getMovieCollection(ctx context.Context, in *MovieCollectionInput) (*MovieCollectionOutput, error) {
	svc, p := reg.catalogBrowse()
	if p != nil {
		return nil, p
	}
	collectionID, p := in.ID.positive("path.id")
	if p != nil {
		return nil, p
	}
	viewer, p := reg.itemViewer(ctx, in.ImageSize, in.LibraryID, "")
	if p != nil {
		return nil, p
	}
	var libraryIDs []int
	if in.LibraryID != "" {
		libraryID, p := in.LibraryID.positive(locationQueryLibraryID)
		if p != nil {
			return nil, p
		}
		libraryIDs = []int{libraryID}
	}
	view, err := svc.MovieCollection(ctx, viewer, collectionID, libraryIDs)
	if err != nil {
		return nil, serviceProblem(err)
	}
	sel := reg.ratingSelection(ctx)
	items := make([]CatalogItem, 0, len(view.Items))
	for _, item := range view.Items {
		items = append(items, catalogItemOfListing(item, sel))
	}
	return &MovieCollectionOutput{Body: MovieCollection{
		ID: IDFromInt(int64(view.TMDBCollectionID)), Title: view.Title, Overview: view.Overview,
		MemberCount: len(items), PlayedCount: view.PlayedCount, Items: items,
	}}, nil
}

func (reg *Registry) getMovieCollectionGrouping(ctx context.Context, in *MovieCollectionGroupingInput) (*MovieCollectionGroupingOutput, error) {
	svc, p := reg.catalogBrowse()
	if p != nil {
		return nil, p
	}
	userID, p := reg.settingValuesReady(ctx)
	if p != nil {
		return nil, p
	}
	libraryID, p := in.LibraryID.positive(locationQueryLibraryID)
	if p != nil {
		return nil, p
	}
	viewer, p := reg.itemViewer(ctx, "", "", "")
	if p != nil {
		return nil, p
	}
	libraryDefault, err := svc.LibraryMovieCollectionGroupingDefault(ctx, viewer, libraryID)
	if err != nil {
		return nil, serviceProblem(err)
	}
	views, err := reg.deps.SettingValues.ResolveEffectiveSettings(ctx, userID, handlers.EffectiveSettingsQuery{
		Keys:            []string{settingskeys.UiMovieCollectionGrouping},
		ActiveProfileID: profileFrom(ctx),
		LibraryIDs:      []int{libraryID},
		VerifyProfile:   verifyHouseholdProfile(ctx),
	})
	if err != nil {
		return nil, settingValueProblem(err, map[string]string{fieldLibraryID: locationQueryLibraryID})
	}
	return &MovieCollectionGroupingOutput{Body: resolveMovieCollectionGrouping(libraryDefault, views)}, nil
}

// resolveMovieCollectionGrouping applies the library default under the
// profile's resolved ui.movie_collection_grouping: a stored grouped or flat
// wins, and an unset value (the contract default, null) defers to the library.
func resolveMovieCollectionGrouping(libraryDefault string, views []handlers.EffectiveSettingValueView) MovieCollectionGrouping {
	if libraryDefault != models.MovieCollectionGroupingGrouped {
		libraryDefault = models.MovieCollectionGroupingFlat
	}
	out := MovieCollectionGrouping{LibraryDefault: libraryDefault, Effective: libraryDefault, Source: movieCollectionGroupingFromLibrary}
	for _, v := range views {
		if v.Key != settingskeys.UiMovieCollectionGrouping {
			continue
		}
		var value string
		if err := json.Unmarshal(v.Value, &value); err != nil {
			continue
		}
		if value == models.MovieCollectionGroupingGrouped || value == models.MovieCollectionGroupingFlat {
			out.Effective, out.Source = value, v.Source
		}
	}
	return out
}
