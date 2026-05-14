package ytx

import (
	"context"
	"fmt"
	"time"
)

// Extract gets the best audio stream URL for a video
func (e *Extractor) Extract(videoID string) (*Result, error) {
	return e.ExtractContext(context.Background(), videoID)
}

func (e *Extractor) ExtractContext(ctx context.Context, videoID string) (*Result, error) {
	e.warnings = nil
	e.poPlayerToken = ""
	e.poURLToken = ""
	e.poChallenge = nil
	e.cipherRetried.Store(false) // Allow one retry per extraction
	var totalStart time.Time
	if e.profile {
		totalStart = time.Now()
		e.timings = Timings{} // Reset timings
	}

	// Parallel fetch: visitorData, cipher (pre-warm), and prepare for API call
	// This saves ~150-250ms by overlapping network calls
	type visitorResult struct {
		err error
	}
	type cipherResult struct {
		cipher    *Cipher
		err       error
		prewarmMs int64
	}

	visitorCh := make(chan visitorResult, 1)
	cipherCh := make(chan cipherResult, 1)
	stsCh := make(chan int, 1) // early STS for API overlap
	type poTokenResult struct {
		playerToken string
		urlToken    string
		err         error
	}
	poCh := make(chan poTokenResult, 1)

	// Fetch visitorData in parallel
	go func() {
		err := e.fetchVisitorDataContext(ctx, videoID)
		visitorCh <- visitorResult{err: err}
	}()

	// Pre-fetch cipher with early STS signaling.
	// STS is signaled as soon as player.js is downloaded so the API call can start
	// before the full cipher init (pattern detection, wrapper runtime build) completes.
	go func() {
		var prewarmStart time.Time
		if e.profile {
			prewarmStart = time.Now()
		}
		cipher, err := e.getCachedCipherContext(ctx, videoID)
		if err == nil && cipher != nil {
			// Signal STS as soon as cipher is available (even from cache)
			select {
			case stsCh <- cipher.SignatureTimestamp():
			default:
			}

			// Synchronously ensure sig runtime is ready before sending cipher result.
			// This eliminates the race where DecryptSignature hits ensureWrapperContext
			// before an async prewarm finishes, paying ~326ms bootstrap redundantly.
			if cipher.canPrecomputeSignatureRuntime() {
				_ = cipher.ensureSignatureReady()
			}
			if cipher.canPrewarmSignatureRuntime() {
				_ = cipher.Prewarm()
			}
			// N-transform engine can warm in background — not needed before decrypt.
			go func() { _ = cipher.WarmNTransformEngineContext(ctx) }()
		} else {
			select {
			case stsCh <- 0:
			default:
			}
		}

		cipherCh <- cipherResult{cipher: cipher, err: err, prewarmMs: func() int64 {
			if e.profile {
				return time.Since(prewarmStart).Milliseconds()
			}
			return 0
		}()}
	}()

	// Wait for visitorData (required for API call)
	var visitorWaitStart time.Time
	if e.profile {
		visitorWaitStart = time.Now()
	}
	visitorRes := <-visitorCh
	if e.profile {
		e.timings.VisitorDataMs = time.Since(visitorWaitStart).Milliseconds()
	}
	if visitorRes.err != nil {
		e.addWarningf("visitor data prefetch failed: %v", visitorRes.err)
	}

	if e.poMode {
		poCoordinator := e.newPOCoordinator()
		go func() {
			playerToken, urlToken, err := poCoordinator.acquireTokens(ctx, videoID)
			poCh <- poTokenResult{playerToken: playerToken, urlToken: urlToken, err: err}
		}()
	}

	// Wait briefly for STS (not full cipher) — allows API call to start sooner
	var stsWaitStart time.Time
	if e.profile {
		stsWaitStart = time.Now()
	}
	sts := 0
	stsReady := false
	if !e.config.NeedsCookies {
		sts = <-stsCh
		stsReady = true
	} else {
		select {
		case sts = <-stsCh:
			stsReady = true
		case <-time.After(stsWaitBudget):
			sts = 0
		}
	}
	if e.profile {
		e.timings.STSWaitMs = time.Since(stsWaitStart).Milliseconds()
	}

	if e.poMode {
		poRes := <-poCh
		e.poPlayerToken = poRes.playerToken
		e.poURLToken = poRes.urlToken
		if poRes.err != nil {
			return nil, NewPOError(POFailureTokenUnavailable, "po mode token unavailable", poRes.err)
		}
	}

	// Call innertube API with STS (cipher may still be finishing heavy init)
	var apiStart time.Time
	if e.profile {
		apiStart = time.Now()
	}
	e.earlySTSOverride = sts
	playerResp, err := e.callPlayerAPIContext(ctx, videoID)
	e.earlySTSOverride = 0
	if err != nil && !stsReady {
		select {
		case sts = <-stsCh:
			if sts > 0 {
				e.earlySTSOverride = sts
				playerResp, err = e.callPlayerAPIContext(ctx, videoID)
				e.earlySTSOverride = 0
			}
		default:
		}
	}
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	if e.profile {
		e.timings.PlayerAPIMs = time.Since(apiStart).Milliseconds()
	}

	// NOW wait for full cipher (needed for sig/n decryption)
	var cipherWaitStart time.Time
	if e.profile {
		cipherWaitStart = time.Now()
	}
	cipherRes := <-cipherCh
	if e.profile {
		e.timings.CipherInitMs = time.Since(cipherWaitStart).Milliseconds()
		e.timings.CipherPrewarmMs = cipherRes.prewarmMs
	}
	if cipherRes.err == nil {
		e.cipherMu.Lock()
		e.cipher = cipherRes.cipher
		e.cipherMu.Unlock()
	}

	if err := e.validatePlayability(playerResp); err != nil {
		return nil, err
	}

	// Find best audio stream
	stream := e.findBestAudioStream(playerResp.StreamingData.AdaptiveFormats)
	if stream == nil {
		return nil, fmt.Errorf("no audio stream found")
	}

	// Get stream URL (may need decryption) - cipher already initialized
	streamURL, err := e.getStreamURLContext(ctx, videoID, stream)
	if err != nil {
		return nil, fmt.Errorf("failed to get stream URL: %w", err)
	}

	result := &Result{
		URL:      streamURL,
		Itag:     stream.Itag,
		Bitrate:  stream.Bitrate,
		MimeType: stream.MimeType,
		Title:    playerResp.VideoDetails.Title,
		Author:   playerResp.VideoDetails.Author,
		Warnings: e.warningsForResult(),
	}

	if e.profile {
		e.finalizeProfile(totalStart)
		result.Timings = &e.timings
	}

	return result, nil
}

// ExtractVideo gets video and audio stream URLs (for MPV playback)
func (e *Extractor) ExtractVideo(videoID string) (*VideoResult, error) {
	return e.ExtractVideoContext(context.Background(), videoID)
}

func (e *Extractor) ExtractVideoContext(ctx context.Context, videoID string) (*VideoResult, error) {
	e.warnings = nil
	var totalStart time.Time
	if e.profile {
		totalStart = time.Now()
		e.timings = Timings{} // Reset timings
	}

	// 0. Fetch visitorData first (required since Jan 2025)
	var visitorStart time.Time
	if e.profile {
		visitorStart = time.Now()
	}
	if err := e.fetchVisitorDataContext(ctx, videoID); err != nil {
		e.addWarningf("visitor data fetch failed: %v", err)
	}
	if e.profile {
		e.timings.VisitorDataMs = time.Since(visitorStart).Milliseconds()
	}

	var apiStart time.Time
	if e.profile {
		apiStart = time.Now()
	}
	playerResp, err := e.callPlayerAPIContext(ctx, videoID)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	if e.profile {
		e.timings.PlayerAPIMs = time.Since(apiStart).Milliseconds()
	}

	if err := e.validatePlayability(playerResp); err != nil {
		return nil, err
	}

	// Find best video stream (1080p preferred)
	video := e.findBestVideoStream(playerResp.StreamingData.AdaptiveFormats)
	if video == nil {
		return nil, fmt.Errorf("no video stream found")
	}

	// Find best audio stream
	audio := e.findBestAudioStream(playerResp.StreamingData.AdaptiveFormats)
	if audio == nil {
		return nil, fmt.Errorf("no audio stream found")
	}

	// ANDROID_VR with visitorData returns direct URLs (no cipher)
	if video.URL == "" || audio.URL == "" {
		return nil, fmt.Errorf("no direct URLs - cipher required but video mode doesn't support it")
	}

	result := &VideoResult{
		VideoURL:          video.URL,
		AudioURL:          audio.URL,
		VideoItag:         video.Itag,
		AudioItag:         audio.Itag,
		Width:             video.Width,
		Height:            video.Height,
		Title:             playerResp.VideoDetails.Title,
		Author:            playerResp.VideoDetails.Author,
		Warnings:          e.warningsForResult(),
		chaptersRequested: e.fetchChapters,
	}

	if e.fetchChapters {
		result.Chapters = extractChapters(playerResp)
	}

	// Extract subtitles if requested (from same response - zero extra latency)
	if e.fetchSubs {
		subtitles := extractSubtitles(playerResp.Captions, e.subLangs)
		if len(subtitles) > 0 {
			result.Subtitles = subtitles
			result.SubURL = subtitles[0].URL
		}
	}

	if e.profile {
		e.finalizeProfile(totalStart)
		result.Timings = &e.timings
	}

	return result, nil
}

func (e *Extractor) finalizeProfile(totalStart time.Time) {
	e.timings.TotalMs = time.Since(totalStart).Milliseconds()
	profiled := e.timings.VisitorDataMs +
		e.timings.STSWaitMs +
		e.timings.PlayerAPIMs +
		e.timings.CipherInitMs +
		e.timings.SigDecryptMs +
		e.timings.NTransformMs
	if e.timings.TotalMs > profiled {
		e.timings.OtherMs = e.timings.TotalMs - profiled
	}
	if engineName := CachedEngineName(); engineName != "" {
		e.timings.JSEngine = engineName
	}
}

func (e *Extractor) validatePlayability(playerResp *PlayerResponse) error {
	if playerResp.PlayabilityStatus.Status == "OK" {
		return nil
	}

	message := fmt.Sprintf("video not playable: %s - %s",
		playerResp.PlayabilityStatus.Status,
		playerResp.PlayabilityStatus.Reason,
	)

	if !e.poMode && IsLikelyPORequiredFromPlayerResponse(playerResp) {
		return NewPOError(POFailureRequired, message, nil)
	}

	return fmt.Errorf("%s", message)
}
