package ytx

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

const (
	// DefaultBatchSize is the number of tracks processed in each batch
	// 10 is safe for premium authenticated accounts
	DefaultBatchSize = 10

	// IntraBatchDelay is the delay between starting each request within a batch
	// 50ms = 20 req/sec max, mimics browsing behavior
	IntraBatchDelay = 50 * time.Millisecond

	// InterBatchDelay is the delay between batches
	// 200ms breathing room between batches
	InterBatchDelay = 200 * time.Millisecond
)

// BulkExtract processes multiple video IDs with bucket-based batching
// Results are streamed to stdout as NDJSON
func (e *Extractor) BulkExtract(videoIDs []string) {
	encoder := json.NewEncoder(os.Stdout)

	// Process in batches
	for batchStart := 0; batchStart < len(videoIDs); batchStart += DefaultBatchSize {
		batchEnd := min(batchStart+DefaultBatchSize, len(videoIDs))
		batch := videoIDs[batchStart:batchEnd]

		// Process this batch with pipelining
		results := e.processBatch(batch)

		// Stream results immediately
		for _, result := range results {
			encoder.Encode(result)
		}

		// Inter-batch delay (except for last batch)
		if batchEnd < len(videoIDs) {
			time.Sleep(InterBatchDelay)
		}
	}
}

// processBatch handles a single batch of video IDs with staggered pipelining
func (e *Extractor) processBatch(videoIDs []string) []BulkResult {
	results := make([]BulkResult, len(videoIDs))
	var wg sync.WaitGroup

	for i, id := range videoIDs {
		wg.Add(1)
		go func(idx int, videoID string) {
			defer wg.Done()

			result, err := e.Extract(videoID)
			if err != nil {
				results[idx] = BulkResult{
					ID:    videoID,
					Error: err.Error(),
				}
				return
			}

			results[idx] = BulkResult{
				ID:      videoID,
				URL:     result.URL,
				Itag:    result.Itag,
				Bitrate: result.Bitrate,
				Title:   result.Title,
			}
		}(i, id)

		// Stagger next request start within batch
		if i < len(videoIDs)-1 {
			time.Sleep(IntraBatchDelay)
		}
	}

	wg.Wait()
	return results
}

// BulkExtractOrdered processes video IDs and returns results in order
// Uses bucket-based batching for rate-limit safety
func (e *Extractor) BulkExtractOrdered(videoIDs []string) []BulkResult {
	allResults := make([]BulkResult, 0, len(videoIDs))

	// Process in batches
	for batchStart := 0; batchStart < len(videoIDs); batchStart += DefaultBatchSize {
		batchEnd := min(batchStart+DefaultBatchSize, len(videoIDs))
		batch := videoIDs[batchStart:batchEnd]

		// Process this batch
		batchResults := e.processBatch(batch)
		allResults = append(allResults, batchResults...)

		// Inter-batch delay (except for last batch)
		if batchEnd < len(videoIDs) {
			time.Sleep(InterBatchDelay)
		}
	}

	return allResults
}
