package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// MovieCollectionCardView marks a catalog card that stands for a box set: the
// movies of one TMDB collection the viewer can see in the browsed libraries.
type MovieCollectionCardView struct {
	TMDBCollectionID int
	Title            string
	MemberCount      int
	PlayedCount      int
}

// MovieCollectionView is one box set with its members as cards.
type MovieCollectionView struct {
	TMDBCollectionID int
	Title            string
	Overview         string
	PlayedCount      int
	Items            []itemListResponse
}

// applyMovieCollectionCards turns each card whose movie belongs to a box set
// into that box set's card. A grouped page has one row per collection, so a
// box set appears once; the row's own movie, the first in the requested order,
// stays the card's content_id.
//
// A failed lookup leaves the cards as movies rather than failing the browse:
// grouping only presents a page, it never decides what is on it.
func (h *CatalogHandler) applyMovieCollectionCards(ctx context.Context, v ItemViewer, req catalog.CatalogRequest, items []*models.MediaItem, cards []itemListResponse) {
	if h == nil || h.resolver == nil || len(items) == 0 {
		return
	}
	contentIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil && item.Type == trailerItemTypeMovie {
			contentIDs = append(contentIDs, item.ContentID)
		}
	}
	membership, err := h.resolver.MovieCollectionIDs(ctx, contentIDs)
	if err != nil {
		slog.WarnContext(ctx, "catalog: movie collection lookup failed", "component", "api", "error", err)
		return
	}
	if len(membership) == 0 {
		return
	}
	collectionIDs := make([]int, 0, len(membership))
	for _, id := range membership {
		collectionIDs = append(collectionIDs, id)
	}
	collections, err := h.resolver.MovieCollections(ctx, collectionIDs, playableTargetLibraryIDs(req), v.Access)
	if err != nil {
		slog.WarnContext(ctx, "catalog: movie collection members lookup failed", "component", "api", "error", err)
		return
	}
	var (
		members []*models.MediaItem
		covers  []*models.MediaItem
	)
	for _, c := range collections {
		if c.IsBoxSet() {
			members = append(members, c.Members...)
			covers = append(covers, c.Members[0])
		}
	}
	if len(covers) == 0 {
		return
	}
	states := h.itemsH.listItemUserStates(ctx, v, members)
	images := h.itemsH.itemListCardImageURLs(ctx, covers, v.Access.ImageSize)
	byID := make(map[string]int, len(cards))
	for i := range cards {
		byID[cards[i].ContentID] = i
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		c := collections[membership[item.ContentID]]
		i, ok := byID[item.ContentID]
		if !c.IsBoxSet() || !ok {
			continue
		}
		applyMovieCollectionToCard(&cards[i], c, playedCount(c.Members, states), images[c.Members[0].ContentID])
	}
}

// applyMovieCollectionToCard dresses a movie card as its box set. The cover
// is the collection's first movie, so a box set shows the same artwork
// whichever member the page's order surfaced.
func applyMovieCollectionToCard(card *itemListResponse, c *catalog.MovieCollection, played int, cover itemListImageURLs) {
	first := c.Members[0]
	card.Type = "movie_collection"
	card.Title = c.Name
	card.Overview = c.Overview
	card.MovieCollection = &MovieCollectionCardView{
		TMDBCollectionID: c.TMDBCollectionID,
		Title:            c.Name,
		MemberCount:      len(c.Members),
		PlayedCount:      played,
	}
	card.PosterURL = cover.posterURL
	card.PosterThumbhash = first.PosterThumbhash
	card.BackdropURL = cover.backdropURL
	card.BackdropThumbhash = first.BackdropThumbhash
	card.LogoURL = ""
	card.PosterIsEpisodeStill = nil
	card.BackdropIsEpisodeStill = nil
	// A box set has no single play state; played_count carries its progress.
	card.UserState = nil
	card.PlayContentID = ""
}

func playedCount(members []*models.MediaItem, states map[string]*itemUserStateResponse) int {
	played := 0
	for _, m := range members {
		if s := states[m.ContentID]; s != nil && s.Played {
			played++
		}
	}
	return played
}

// MovieCollection answers one box set: the collection's movies the viewer can
// see in libraryIDs, in collection order. A collection with fewer than two
// such movies is not a box set and is not found.
func (h *CatalogHandler) MovieCollection(ctx context.Context, v ItemViewer, tmdbCollectionID int, libraryIDs []int) (MovieCollectionView, error) {
	if h == nil || h.resolver == nil || h.itemsH == nil {
		return MovieCollectionView{}, apiError(http.StatusInternalServerError, "internal_error", "Catalog is not configured")
	}
	collections, err := h.resolver.MovieCollections(ctx, []int{tmdbCollectionID}, libraryIDs, v.Access)
	if err != nil {
		return MovieCollectionView{}, catalogResolveError(ctx, err, false)
	}
	c := collections[tmdbCollectionID]
	if !c.IsBoxSet() {
		return MovieCollectionView{}, apiError(http.StatusNotFound, "not_found", "Movie collection not found")
	}
	items := h.catalogItemResponses(ctx, v, c.Members, "", libraryIDs, v.Access)
	view := MovieCollectionView{TMDBCollectionID: c.TMDBCollectionID, Title: c.Name, Overview: c.Overview, Items: items}
	for _, item := range items {
		if item.UserState != nil && item.UserState.Played {
			view.PlayedCount++
		}
	}
	return view, nil
}

// LibraryMovieCollectionGroupingDefault answers whether a library shows box
// sets to viewers who have not chosen: "grouped" or "flat".
func (h *CatalogHandler) LibraryMovieCollectionGroupingDefault(ctx context.Context, v ItemViewer, libraryID int) (string, error) {
	if h == nil || h.resolver == nil {
		return "", apiError(http.StatusInternalServerError, "internal_error", "Catalog is not configured")
	}
	value, err := h.resolver.LibraryMovieCollectionGroupingDefault(ctx, libraryID, v.Access)
	if err != nil {
		return "", catalogResolveError(ctx, err, false)
	}
	return value, nil
}
