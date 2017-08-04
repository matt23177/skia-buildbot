package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.skia.org/infra/ct_pixel_diff/go/resultstore"
	"go.skia.org/infra/go/httputils"
	"go.skia.org/infra/golden/go/diffstore"
)

// jsonRunsHandler returns the current list of CT Pixel Diff jobs as a
// serialized list of strings.
func jsonRunsHandler(w http.ResponseWriter, r *http.Request) {
	runIDs, err := resultStore.GetRunIDs(resultstore.BeginningOfTime, time.Now())
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to retrieve runIDs")
		return
	}
	sendJsonResponse(w, map[string][]string{"runs": runIDs})
}

// jsonRenderHandler parses a start index, end index, and runID from the query
// and uses them to return results in the specified range for the specified run
// from the ResultStore cache.
func jsonRenderHandler(w http.ResponseWriter, r *http.Request) {
	runID := r.FormValue("runID")

	startIdx, err := strconv.Atoi(r.FormValue("startIdx"))
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to parse start index")
		return
	}

	endIdx, err := strconv.Atoi(r.FormValue("endIdx"))
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to parse end index")
		return
	}

	// If the runID does not exist in the cache, this will return an error.
	results, err := resultStore.GetRange(runID, startIdx, endIdx)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to get cached results")
		return
	}
	sendJsonResponse(w, map[string][]*resultstore.ResultRec{"results": results})
}

// jsonSortHandler sorts the ResultStore's cached list of diff results using the
// specified sort field, sort order, and runID.
func jsonSortHandler(w http.ResponseWriter, r *http.Request) {
	runID := r.FormValue("runID")
	sortField := r.FormValue("sortField")
	sortOrder := r.FormValue("sortOrder")

	// If the runID does not exist in the cache, this will return an error.
	err := resultStore.SortRun(runID, sortField, sortOrder)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to sort cached results")
		return
	}
}

// jsonDeleteHandler deletes the data for the specified runID from the server.
func jsonDeleteHandler(w http.ResponseWriter, r *http.Request) {
	runID := r.FormValue("runID")

	// Remove ResultStore data.
	err := resultStore.RemoveRun(runID)
	if err != nil {
		httputils.ReportError(w, r, err, fmt.Sprintf("Failed to remove run %s from server", runID))
		return
	}

	// Remove screenshots and diff images from the DiffStore.
	imagePath := filepath.Join(*imageDir, diffstore.DEFAULT_IMG_DIR_NAME, runID)
	diffPath := filepath.Join(*imageDir, diffstore.DEFAULT_DIFFIMG_DIR_NAME, runID)
	err = os.RemoveAll(imagePath)
	if err != nil {
		httputils.ReportError(w, r, err, fmt.Sprintf("Failed to remove screenshots for run %s from DiffStore", runID))
		return
	}
	err = os.RemoveAll(diffPath)
	if err != nil {
		httputils.ReportError(w, r, err, fmt.Sprintf("Failed to remove diff images for run %s from DiffStore", runID))
		return
	}
}

// makeResourceHandler creates a static file handler that sets a caching policy.
func makeResourceHandler(resourceDir string) func(http.ResponseWriter, *http.Request) {
	fileServer := http.FileServer(http.Dir(resourceDir))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", "max-age=300")
		fileServer.ServeHTTP(w, r)
	}
}
