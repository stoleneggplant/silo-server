package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Silo-Server/silo-server/internal/librarykind"
	"github.com/Silo-Server/silo-server/internal/moviecollections"
	"github.com/Silo-Server/silo-server/internal/taskmanager"
)

// MovieCollectionsRefresher checks which TMDB collection movies belong to.
// Satisfied by *moviecollections.Refresher.
type MovieCollectionsRefresher interface {
	Refresh(ctx context.Context) (moviecollections.Result, error)
}

// RefreshMovieCollectionsTask records the TMDB collection each movie belongs
// to, which movie libraries show as box sets. One server checks at a time.
type RefreshMovieCollectionsTask struct {
	refresher MovieCollectionsRefresher
}

// NewRefreshMovieCollectionsTask creates a new RefreshMovieCollectionsTask.
func NewRefreshMovieCollectionsTask(refresher MovieCollectionsRefresher) *RefreshMovieCollectionsTask {
	return &RefreshMovieCollectionsTask{refresher: refresher}
}

func (t *RefreshMovieCollectionsTask) Key() string  { return "refresh_movie_collections" }
func (t *RefreshMovieCollectionsTask) Name() string { return "Refresh Movie Collections" }
func (t *RefreshMovieCollectionsTask) Description() string {
	return "Looks up which TMDB collection each movie belongs to, so movie libraries can group a franchise into one box set"
}

func (t *RefreshMovieCollectionsTask) Category() taskmanager.TaskCategory {
	return taskmanager.TaskCategoryMetadata
}

func (t *RefreshMovieCollectionsTask) IsHidden() bool { return false }

// ServesLibrary keeps the task out of the task list until a library that
// holds movies exists.
func (t *RefreshMovieCollectionsTask) ServesLibrary(libraryType string) bool {
	return librarykind.IsMovie(libraryType) || librarykind.IsMixed(libraryType)
}

func (t *RefreshMovieCollectionsTask) DefaultTriggers() []taskmanager.TriggerConfig {
	return []taskmanager.TriggerConfig{
		{Type: taskmanager.TriggerTypeStartup},
		{Type: taskmanager.TriggerTypeInterval, IntervalMs: 60 * 60 * 1000}, // hourly
	}
}

func (t *RefreshMovieCollectionsTask) Execute(ctx context.Context, progress taskmanager.ProgressReporter) error {
	if t.refresher == nil {
		return errors.New("movie collections refresh: refresher not configured")
	}
	progress.Report(0, "Checking movie collections")
	result, err := t.refresher.Refresh(ctx)
	if data, marshalErr := json.Marshal(result); marshalErr == nil {
		progress.SetResultData(data)
	}
	if err != nil {
		return fmt.Errorf("movie collections refresh: %w", err)
	}
	if result.Skipped {
		progress.Report(100, "Another server is checking movie collections")
		return nil
	}
	progress.Report(100, fmt.Sprintf("Checked %d movies, %d in a collection", result.Checked, result.InCollection))
	return nil
}
