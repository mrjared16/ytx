package ytx

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"time"
)

const (
	bulkInterTrackDelayBase   = 150 * time.Millisecond
	bulkInterTrackDelayJitter = 100 * time.Millisecond
)

// BulkExtract processes multiple video IDs and streams NDJSON results.
// Results are streamed to stdout as NDJSON
func (e *Extractor) BulkExtract(videoIDs []string) {
	e.BulkExtractContext(context.Background(), videoIDs)
}

// BulkExtractContext processes multiple video IDs with shared context.
func (e *Extractor) BulkExtractContext(ctx context.Context, videoIDs []string) {
	encoder := json.NewEncoder(os.Stdout)
	e.bulkExtractWithEmitterContext(ctx, videoIDs, func(result BulkResult) {
		_ = encoder.Encode(result)
	})
}

func (e *Extractor) bulkExtractWithEmitterContext(ctx context.Context, videoIDs []string, emit func(BulkResult)) {
	if len(videoIDs) == 0 {
		return
	}

	// Single-ID bulk: skip prefetch warmup and jitter overhead
	if len(videoIDs) == 1 {
		emit(e.extractBulkResultContext(ctx, videoIDs[0]))
		return
	}

	e.prefetchBatchSharedState(ctx, videoIDs[0])
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i, videoID := range videoIDs {
		emit(e.extractBulkResultContext(ctx, videoID))

		if i == len(videoIDs)-1 {
			continue
		}
		if err := sleepContext(ctx, randomizedBulkDelay(rng)); err != nil {
			return
		}
	}
}

func (e *Extractor) extractBulkResultContext(ctx context.Context, videoID string) BulkResult {
	result, err := e.ExtractContext(ctx, videoID)
	if err != nil {
		errMsg := err.Error()
		if !e.poMode {
			if IsLikelyPORequiredError(err) {
				errMsg += " (hint: try rerunning with --po)"
			}
		}
		return BulkResult{ID: videoID, Error: errMsg}
	}

	return BulkResult{
		ID:      videoID,
		URL:     result.URL,
		Itag:    result.Itag,
		Bitrate: result.Bitrate,
		Title:   result.Title,
	}
}

func (e *Extractor) prefetchBatchSharedState(ctx context.Context, videoID string) {
	go func() {
		prefetchCtx, cancel := backgroundRefreshContext(ctx, backgroundWarmupTTL)
		defer cancel()
		if err := e.fetchVisitorDataContext(prefetchCtx, videoID); err != nil {
			return
		}
		cipher, err := e.ensureCipherContext(prefetchCtx, videoID)
		if err != nil || cipher == nil {
			return
		}
		if err := cipher.WarmNTransformEngineContext(prefetchCtx); err != nil {
			return
		}
	}()
}

func randomizedBulkDelay(rng *rand.Rand) time.Duration {
	if rng == nil {
		return bulkInterTrackDelayBase
	}

	window := int((2 * bulkInterTrackDelayJitter) / time.Millisecond)
	delta := time.Duration(rng.Intn(window+1))*time.Millisecond - bulkInterTrackDelayJitter
	return bulkInterTrackDelayBase + delta
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// BulkExtractOrdered processes video IDs and returns results in order.
// This is the programmatic API - use BulkExtract() for CLI streaming to stdout.
func (e *Extractor) BulkExtractOrdered(videoIDs []string) []BulkResult {
	return e.BulkExtractOrderedContext(context.Background(), videoIDs)
}

func (e *Extractor) BulkExtractOrderedContext(ctx context.Context, videoIDs []string) []BulkResult {
	allResults := make([]BulkResult, 0, len(videoIDs))
	e.bulkExtractWithEmitterContext(ctx, videoIDs, func(result BulkResult) {
		allResults = append(allResults, result)
	})
	return allResults
}
