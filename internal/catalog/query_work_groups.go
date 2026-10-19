package catalog

import (
	"fmt"
	"strings"
)

// groupedByWork chooses the first matching edition in the complete source
// ordering before applying a page boundary. Seeking raw editions first would
// repeat a work through another edition on a later page.
func (p previewPagePlan) groupedByWork() previewPagePlan {
	return p.groupedBy(
		`CASE WHEN mi.type IN ('ebook','audiobook') AND work_link.work_id IS NOT NULL THEN 'work:' || work_link.work_id ELSE 'item:' || mi.content_id END`,
		"LEFT JOIN literary_work_items work_link ON work_link.content_id=mi.content_id",
	)
}

// groupedByMovieCollection collapses the movies of one TMDB collection into
// their first match in the source ordering, the way groupedByWork collapses
// editions. A membership recorded under another TMDB id than the movie's
// current one is stale and leaves the movie ungrouped.
func (p previewPagePlan) groupedByMovieCollection() previewPagePlan {
	return p.groupedBy(
		`CASE WHEN mi.type = 'movie' AND movie_collection_link.tmdb_collection_id IS NOT NULL AND movie_collection_link.tmdb_id = mi.tmdb_id THEN 'movie_collection:' || movie_collection_link.tmdb_collection_id ELSE 'item:' || mi.content_id END`,
		"LEFT JOIN media_item_tmdb_collections movie_collection_link ON movie_collection_link.content_id=mi.content_id",
	)
}

// groupedBy keeps, for each value of groupKey, the row that sorts first in
// the complete source ordering, and pages over those representatives.
// groupKey reads columns of mi and of the relation join adds.
func (p previewPagePlan) groupedBy(groupKey, join string) previewPagePlan {
	columns := []string{"mi.*", groupKey + " AS catalog_work_key"}
	terms := append([]queryCursorTerm(nil), p.cursorTerms...)
	innerOrder := make([]string, len(terms))
	outerOrder := make([]string, len(terms))
	for i := range terms {
		alias := fmt.Sprintf("catalog_work_sort_%d", i)
		columns = append(columns, terms[i].expression+" AS "+alias)
		suffix := " ASC NULLS LAST"
		if terms[i].descending {
			suffix = " DESC NULLS LAST"
		}
		if !terms[i].nullsLast {
			suffix = strings.ReplaceAll(suffix, "LAST", "FIRST")
		}
		innerOrder[i] = alias + suffix
		terms[i].expression = "mi." + alias
		outerOrder[i] = terms[i].expression + suffix
	}
	// Sort parameters become candidate parameters, because the representative
	// and exact grouped count both depend on the requested source ordering.
	p.args = append(p.args, p.sortArgs...)
	p.sortArgs = nil
	sourceLimit := ""
	sourceOrder := ""
	if p.maxResults > 0 {
		sourceOrder = p.orderBy
		p.args = append(p.args, p.maxResults)
		sourceLimit = fmt.Sprintf(" LIMIT $%d", len(p.cteArgs)+len(p.args))
	}
	candidates := fmt.Sprintf("catalog_work_candidates AS (SELECT %s %s %s %s %s%s)", strings.Join(columns, ", "), p.fromClausePaged, join, p.whereClause, sourceOrder, sourceLimit)
	ranked := "catalog_work_ranked AS (SELECT catalog_work_candidates.*, ROW_NUMBER() OVER (PARTITION BY catalog_work_key ORDER BY " + strings.Join(innerOrder, ", ") + ") AS catalog_work_rank FROM catalog_work_candidates)"
	p.ctes = append(p.ctes, candidates, ranked)
	p.fromClausePaged = "FROM catalog_work_ranked mi"
	p.fromClauseCount = p.fromClausePaged
	p.whereClause = "WHERE mi.catalog_work_rank = 1"
	p.orderBy = "ORDER BY " + strings.Join(outerOrder, ", ")
	p.cursorTerms = terms
	p.maxResults = 0
	p.limitArgIdx = len(p.cteArgs) + len(p.args) + 1
	return p
}
