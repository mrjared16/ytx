package ytx

import (
	"encoding/json"
	"fmt"
	"net/url"
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

		// Process this batch with pipelining and batch n-transform
		results := e.processBatchOptimized(batch)

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

// intermediateResult holds partial extraction results before n-transform
type intermediateResult struct {
	videoID   string
	streamURL string // URL before n-transform
	nParam    string // n-parameter to transform
	itag      int
	bitrate   int
	title     string
	err       error
}

// processBatchOptimized handles a batch with optimized batch n-transform
// 1. Parallel API calls to get stream URLs with n-parameters
// 2. Batch n-transform all n-parameters in single IPC call
// 3. Apply transformed n-parameters to URLs
func (e *Extractor) processBatchOptimized(videoIDs []string) []BulkResult {
	results := make([]BulkResult, len(videoIDs))
	intermediates := make([]intermediateResult, len(videoIDs))
	var wg sync.WaitGroup

	// Step 1: Parallel API calls (get URLs without n-transform)
	for i, id := range videoIDs {
		wg.Add(1)
		go func(idx int, videoID string) {
			defer wg.Done()
			ir := e.extractWithoutNTransform(videoID)
			intermediates[idx] = ir
		}(i, id)

		// Stagger requests
		if i < len(videoIDs)-1 {
			time.Sleep(IntraBatchDelay)
		}
	}
	wg.Wait()

	// Step 2: Collect all n-parameters that need transformation
	var nValues []string
	nIndexMap := make(map[int]int) // intermediates index -> nValues index
	for i, ir := range intermediates {
		if ir.err == nil && ir.nParam != "" {
			nIndexMap[i] = len(nValues)
			nValues = append(nValues, ir.nParam)
		}
	}

	// Step 3: Batch transform all n-parameters (single IPC call)
	var transformedN []string
	if len(nValues) > 0 && e.cipher != nil {
		var err error
		transformedN, err = e.cipher.TransformNBatch(nValues)
		if err != nil {
			// Fallback: use original n-values
			transformedN = nValues
		}
	}

	// Step 4: Apply transformed n-parameters and build final results
	for i, ir := range intermediates {
		if ir.err != nil {
			results[i] = BulkResult{
				ID:    ir.videoID,
				Error: ir.err.Error(),
			}
			continue
		}

		// Apply transformed n-parameter if available
		finalURL := ir.streamURL
		if nIdx, ok := nIndexMap[i]; ok && len(transformedN) > nIdx {
			finalURL = applyNParam(ir.streamURL, transformedN[nIdx])
		}

		results[i] = BulkResult{
			ID:      ir.videoID,
			URL:     finalURL,
			Itag:    ir.itag,
			Bitrate: ir.bitrate,
			Title:   ir.title,
		}
	}

	return results
}

// extractWithoutNTransform gets stream URL without doing n-transform
func (e *Extractor) extractWithoutNTransform(videoID string) intermediateResult {
	ir := intermediateResult{videoID: videoID}

	// Fetch visitorData (use cache)
	if err := e.fetchVisitorData(videoID); err != nil {
		// Non-fatal
	}

	// Call API
	playerResp, err := e.callPlayerAPI(videoID)
	if err != nil {
		ir.err = err
		return ir
	}

	if playerResp.PlayabilityStatus.Status != "OK" {
		ir.err = fmt.Errorf("video not playable: %s", playerResp.PlayabilityStatus.Status)
		return ir
	}

	// Find best audio stream
	stream := e.findBestAudioStream(playerResp.StreamingData.AdaptiveFormats)
	if stream == nil {
		ir.err = fmt.Errorf("no audio stream found")
		return ir
	}

	// Get stream URL (without n-transform)
	streamURL, err := e.getStreamURLRaw(videoID, stream)
	if err != nil {
		ir.err = err
		return ir
	}

	// Extract n-parameter for batch processing
	if parsedURL, err := url.Parse(streamURL); err == nil {
		ir.nParam = parsedURL.Query().Get("n")
	}

	ir.streamURL = streamURL
	ir.itag = stream.Itag
	ir.bitrate = stream.Bitrate
	ir.title = playerResp.VideoDetails.Title

	return ir
}

// getStreamURLRaw gets stream URL without n-parameter transformation
// NOTE: This is intentionally separate from getStreamURL in extractor.go because:
// - Bulk mode skips retry logic (batch n-transform handles failures)
// - Single mode has fail-forward retry for robustness
func (e *Extractor) getStreamURLRaw(videoID string, stream *Format) (string, error) {
	// If URL is directly available (pre-signed), use it
	if stream.URL != "" {
		return stream.URL, nil
	}

	// Otherwise, decrypt the signature cipher
	if stream.SignatureCipher == "" {
		return "", fmt.Errorf("no URL or signature cipher available")
	}

	// Parse the cipher parameters
	params, err := url.ParseQuery(stream.SignatureCipher)
	if err != nil {
		return "", err
	}

	baseURL := params.Get("url")
	sig := params.Get("s")
	sigParam := params.Get("sp")
	if sigParam == "" {
		sigParam = "sig"
	}

	// Initialize cipher if needed
	cipher, err := e.ensureCipher(videoID)
	if err != nil {
		return "", err
	}

	// Decrypt signature
	decryptedSig, err := cipher.DecryptSignature(sig)
	if err != nil {
		return "", err
	}

	// Add decrypted signature to URL
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	query := parsedURL.Query()
	query.Set(sigParam, decryptedSig)
	parsedURL.RawQuery = query.Encode()

	return parsedURL.String(), nil
}

// applyNParam replaces n-parameter in URL with transformed value
func applyNParam(streamURL, transformedN string) string {
	parsedURL, err := url.Parse(streamURL)
	if err != nil {
		return streamURL
	}
	query := parsedURL.Query()
	if query.Get("n") != "" {
		query.Set("n", transformedN)
		parsedURL.RawQuery = query.Encode()
	}
	return parsedURL.String()
}

// BulkExtractOrdered processes video IDs and returns results in order
// Uses bucket-based batching for rate-limit safety
// This is the programmatic API - use BulkExtract() for CLI streaming to stdout
func (e *Extractor) BulkExtractOrdered(videoIDs []string) []BulkResult {
	allResults := make([]BulkResult, 0, len(videoIDs))

	// Process in batches
	for batchStart := 0; batchStart < len(videoIDs); batchStart += DefaultBatchSize {
		batchEnd := min(batchStart+DefaultBatchSize, len(videoIDs))
		batch := videoIDs[batchStart:batchEnd]

		// Process this batch
		batchResults := e.processBatchOptimized(batch)
		allResults = append(allResults, batchResults...)

		// Inter-batch delay (except for last batch)
		if batchEnd < len(videoIDs) {
			time.Sleep(InterBatchDelay)
		}
	}

	return allResults
}
