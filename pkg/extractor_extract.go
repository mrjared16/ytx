package ytx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// videoProbeTimeout bounds each near-end probe without defeating caller
// cancellation (derived from the caller's context).
const videoProbeTimeout = 10 * time.Second

// videoPlayerAttemptTimeout bounds each innertube player attempt without
// defeating caller cancellation (derived from the caller's context, so the
// earliest deadline always wins).
const videoPlayerAttemptTimeout = 15 * time.Second

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
			// Signal STS as soon as cipher is available (even from cache).
			// NO extractor diagnostics are written here: this prefetch can
			// outlive the main goroutine after cancellation, so warnings and
			// timings ride on the returned cipher and are published by the
			// main consumer only after it receives the cipher result.
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

	// Wait for positive STS (not full cipher): the player API may start before
	// the full cipher init completes. The wait is bound by the caller context;
	// a 0 signal means the cipher fetch failed, which surfaces the cipher
	// error below instead of a zero-STS POST.
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
			stsReady = sts > 0
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !stsReady && e.config.NeedsCookies {
		// The shared cipher fetch failed (0 signal): propagate its error
		// instead of POSTing with a zero/invalid signatureTimestamp. The
		// cipher result is consumed exactly once, here.
		select {
		case cipherRes := <-cipherCh:
			if cipherRes.err != nil {
				return nil, cipherRes.err
			}
			return nil, fmt.Errorf("cipher produced no signatureTimestamp")
		case <-ctx.Done():
			return nil, ctx.Err()
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
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	if e.profile {
		e.timings.PlayerAPIMs = time.Since(apiStart).Milliseconds()
	}

	// NOW wait for full cipher (needed for sig/n decryption). The receive
	// always selects the caller context: cancellation stays prompt and never
	// blocks behind the shared flight.
	var cipherWaitStart time.Time
	if e.profile {
		cipherWaitStart = time.Now()
	}
	var cipherRes cipherResult
	select {
	case cipherRes = <-cipherCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.profile {
		e.timings.CipherInitMs = time.Since(cipherWaitStart).Milliseconds()
		e.timings.CipherPrewarmMs = cipherRes.prewarmMs
	}
	if cipherRes.err == nil {
		// The detached prefetch never writes extractor diagnostics; the
		// completed cipher carries its creation warnings/timings. Publish
		// them HERE exactly once, from the main consumer, only after this
		// receive succeeded — so a canceled extractor never leaves partial
		// writes and the concurrently-written visitor warning stays race-free.
		if cipherRes.cipher != nil {
			e.absorbCipherWarnings(cipherRes.cipher)
			if cipherRes.cipher.PlayerJSFetchMs > 0 {
				e.timings.PlayerJSFetchMs = cipherRes.cipher.PlayerJSFetchMs
			}
			if cipherRes.cipher.CipherAnalyzeMs > 0 {
				e.timings.CipherAnalyzeMs = cipherRes.cipher.CipherAnalyzeMs
			}
			if cipherRes.cipher.AnalyzeDetail != nil {
				e.timings.CipherDetail = cipherRes.cipher.AnalyzeDetail
			}
		}
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
	// The native video chain is explicitly GUEST-ONLY. An authenticated
	// (cookie-bearing) configuration belongs to the music/audio path; running
	// the guest-only visitor flight under a music auth key would share (and
	// collide with) the music flight and could pollute the authenticated
	// visitor caches. Rejected before any state read or network I/O.
	if e.config.NeedsCookies {
		return nil, errNativeVideoRequiresGuest
	}

	e.warnings = nil
	var totalStart time.Time
	if e.profile {
		totalStart = time.Now()
		e.timings = Timings{} // Reset timings
	}

	// 0. Bootstrap/reuse guest visitorData once for the whole chain.
	// The "video" auth key keeps this isolated from authenticated music cache.
	// The value stays an extraction-local candidate until the winning chain
	// validates it; a failed run commits nothing.
	var visitorStart time.Time
	if e.profile {
		visitorStart = time.Now()
	}
	candidate, err := e.fetchVideoVisitorCandidate(ctx, videoID)
	if err != nil {
		e.addWarningf("visitor data fetch failed: %v", err)
	}
	if e.profile {
		e.timings.VisitorDataMs = time.Since(visitorStart).Milliseconds()
	}
	visitorData := candidate

	var apiStart time.Time
	if e.profile {
		apiStart = time.Now()
	}

	outcome := e.runVideoChain(ctx, videoID, visitorData, apiStart, totalStart)
	if outcome.result != nil {
		return outcome.result, nil
	}

	// Self-heal: a poisoned cached guest visitor surfaces as LOGIN_REQUIRED.
	// On the first guest LOGIN_REQUIRED in an extraction, force exactly one
	// fresh WEB bootstrap bypassing memory/disk cache and retry the chain
	// from the start. Only when the first pass ran to completion without a
	// terminal stop (cancellation/429). A failed refresh retains the original
	// attempt evidence and exhausts normally.
	if outcome.guestLogin && outcome.cause == nil && outcome.completed && !e.config.NeedsCookies {
		if ctxErr := ctx.Err(); ctxErr != nil {
			outcome.cause = ctxErr
			outcome.attempts = append(outcome.attempts, VideoAttemptError{Kind: VideoAttemptCancelled, Err: ctxErr})
		} else {
			refreshed, ferr := e.forceRefreshVideoVisitorCandidate(ctx, videoID)
			if ferr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					outcome.cause = ctxErr
					outcome.attempts = append(outcome.attempts, VideoAttemptError{Kind: VideoAttemptCancelled, Err: ctxErr})
				} else {
					e.addWarningf("video visitor refresh failed: %v", ferr)
				}
			} else {
				retry := e.runVideoChain(ctx, videoID, refreshed, apiStart, totalStart)
				outcome.attempts = append(outcome.attempts, retry.attempts...)
				if retry.cause != nil {
					outcome.cause = retry.cause
				}
				if retry.result != nil {
					return retry.result, nil
				}
			}
		}
	}

	return nil, &VideoExhaustedError{Attempts: outcome.attempts, Cause: outcome.cause}
}

// videoChainOutcome is the result of one sequential pass over the profile chain.
type videoChainOutcome struct {
	result     *VideoResult
	attempts   []VideoAttemptError
	cause      error
	guestLogin bool
	completed  bool
}

// runVideoChain runs one sequential pass over the immutable profile chain.
// The guide-bootstrap visitor stays constant across attempts (matching
// ytcui-dl): response visitorData is never adopted or persisted here. Only
// the winning candidate's working visitor is persisted after both near-end
// probes pass. It reports whether any guest LOGIN_REQUIRED was seen so the
// caller can force a single fresh bootstrap and retry.
func (e *Extractor) runVideoChain(ctx context.Context, videoID, visitorData string, apiStart, totalStart time.Time) videoChainOutcome {
	// 1. Sequential immutable profile chain. No config mutation, no fan-out,
	// no cross-client stream mixing, no cipher/PO/JS on this path.
	var outcome videoChainOutcome
	outcome.completed = true
	for _, profile := range videoClientChain {
		if err := ctx.Err(); err != nil {
			outcome.cause = err
			outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptCancelled, Err: err})
			outcome.completed = false
			break
		}

		attemptCtx, attemptCancel := context.WithTimeout(ctx, videoPlayerAttemptTimeout)
		playerResp, err := e.callPlayerAPIWithProfile(attemptCtx, videoID, profile, visitorData)
		attemptCancel()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				outcome.cause = ctxErr
				outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptCancelled, Err: ctxErr})
				outcome.completed = false
				break
			}
			var httpErr *playerHTTPError
			if errors.As(err, &httpErr) {
				if httpErr.Status == http.StatusTooManyRequests {
					outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptHTTP, HTTPStatus: httpErr.Status, Err: err})
					outcome.completed = false
					break
				}
				outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptHTTP, HTTPStatus: httpErr.Status, Err: err})
				continue
			}
			outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptDecode, Err: err})
			continue
		}

		if playerResp.PlayabilityStatus.Status != "OK" {
			if playerResp.PlayabilityStatus.Status == "LOGIN_REQUIRED" && !e.config.NeedsCookies {
				outcome.guestLogin = true
			}
			outcome.attempts = append(outcome.attempts, VideoAttemptError{
				Client: profile.Name,
				Kind:   VideoAttemptPlayability,
				Status: playerResp.PlayabilityStatus.Status,
				Reason: playerResp.PlayabilityStatus.Reason,
			})
			continue
		}

		// Partition direct-URL formats from ciphered ones before selection so
		// a preferred ciphered format can never shadow an available direct
		// stream. Only adaptive direct streams qualify; muxed-only is rejected.
		adaptive := playerResp.StreamingData.AdaptiveFormats
		direct, ciphered := partitionVideoFormats(adaptive)
		if len(adaptive) > 0 && len(direct) == 0 && len(ciphered) == 0 {
			// True SABR: adaptive entries exist but carry neither direct
			// URLs nor cipher data.
			outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptSABROnly, Status: playerResp.PlayabilityStatus.Status})
			continue
		}

		video := e.findBestVideoStream(direct)
		audio := e.findBestAudioStream(direct)
		if video == nil || audio == nil {
			kind := VideoAttemptNoStream
			if len(ciphered) > 0 {
				kind = VideoAttemptCipherOnly
			}
			outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: kind, Status: playerResp.PlayabilityStatus.Status})
			continue
		}

		// 2. Near-end validation with the winning profile's UA before returning.
		if err := e.validateStreamEnds(ctx, profile, video, audio); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				outcome.cause = ctxErr
				outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptCancelled, Err: ctxErr})
				outcome.completed = false
				break
			}
			var httpErr *playerHTTPError
			if errors.As(err, &httpErr) && httpErr.Status == http.StatusTooManyRequests {
				outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptProbe, HTTPStatus: httpErr.Status, Err: err})
				outcome.completed = false
				break
			}
			outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptProbe, Err: err})
			continue
		}

		// 3. Cancellation wins over a successful probe round. Both near-end
		// probes already succeeded, but the caller's context may have been
		// canceled while they were in flight (including during the final body
		// read, where the probe still returns its valid byte). A canceled
		// extraction must never publish a winner, so the chain terminates with
		// the caller's own cancellation identity BEFORE any visitor commit,
		// result publication, or cache write below.
		//
		// This check must stay immediately adjacent to the commit it guards:
		// the window between observing ctx.Err()==nil and the commit is the
		// only thing standing between a canceled caller and a published winner,
		// and any refactor that inserts work in between re-opens it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			outcome.cause = ctxErr
			outcome.attempts = append(outcome.attempts, VideoAttemptError{Client: profile.Name, Kind: VideoAttemptCancelled, Err: ctxErr})
			outcome.completed = false
			break
		}

		// 4. Commit the validated visitor candidate for future extractions.
		// Only the exact candidate that passed stream selection and both
		// near-end probes may refresh extractor/runtime/disk state; failed and
		// non-winning candidates never touch shared state. The chain's own
		// parameter (not e.visitorData) is committed so a concurrent change
		// cannot substitute a different value into this run's commit.
		if !e.config.NeedsCookies {
			e.commitVideoVisitor(visitorData)
		}

		if e.profile {
			e.timings.PlayerAPIMs = time.Since(apiStart).Milliseconds()
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
			Client:            profile.Name,
			UserAgent:         profile.UserAgent,
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

		outcome.result = result
		return outcome
	}

	return outcome
}

// partitionVideoFormats splits adaptive formats into direct-URL formats and
// ciphered (signatureCipher, no direct URL) formats. URL-less entries with no
// cipher data belong to neither group (true SABR).
func partitionVideoFormats(formats []Format) (direct, ciphered []Format) {
	for _, f := range formats {
		switch {
		case f.URL != "":
			direct = append(direct, f)
		case f.SignatureCipher != "":
			ciphered = append(ciphered, f)
		}
	}
	return direct, ciphered
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

// validateStreamEnds probes the selected video and audio URLs concurrently
// (max two goroutines) with the winning profile's UA. Both must prove their
// final byte is fetchable; any failure falls through to the next client.
//
// BOTH probe outcomes are always retained: a terminal first result releases
// the sibling promptly and then still joins it, so a 429 (or caller
// cancellation) observed by the sibling is never discarded. Classification
// priority is fixed and order-independent:
//  1. caller cancellation/deadline (checked on the caller's own context),
//  2. ANY observed HTTP 429, in either delivery order,
//  3. the local per-probe deadline,
//  4. ordinary failures, first observed wins.
func (e *Extractor) validateStreamEnds(ctx context.Context, profile videoClientProfile, video, audio *Format) error {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type probeResult struct{ err error }
	results := make(chan probeResult, 2)
	go func() {
		results <- probeResult{e.probeStreamEnd(probeCtx, video.URL, profile.UserAgent, video.ContentLength)}
	}()
	go func() {
		results <- probeResult{e.probeStreamEnd(probeCtx, audio.URL, profile.UserAgent, audio.ContentLength)}
	}()

	// A terminal first result means the round is already decided: release the
	// sibling now, then still collect its outcome so the classification below
	// sees both errors. The channel is buffered (cap 2), so neither goroutine
	// can leak.
	first := <-results
	if isTerminalProbeError(first.err) {
		cancel()
	}
	second := <-results

	observed := []error{first.err, second.err}

	// 1. Caller cancellation has the highest priority and keeps the caller's
	// own cancellation identity.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// 2. Any observed 429 outranks a local timeout or an ordinary sibling
	// failure, regardless of which result arrived first.
	for _, err := range observed {
		if isRateLimitedProbeError(err) {
			return err
		}
	}

	// 3. The per-probe deadline (not the caller's) is terminal.
	for _, err := range observed {
		if isLocalProbeTimeoutError(err) {
			return err
		}
	}

	// 4. Ordinary failure: the first observed one wins.
	for _, err := range observed {
		if err != nil {
			return err
		}
	}
	return nil
}

// isTerminalProbeError reports whether a probe failure means the round can no
// longer succeed: caller cancellation/deadline or HTTP 429. A terminal result
// releases the sibling probe immediately, but its outcome is still retained.
func isTerminalProbeError(err error) bool {
	return isLocalProbeTimeoutError(err) || isRateLimitedProbeError(err) || errors.Is(err, context.Canceled)
}

// isRateLimitedProbeError reports whether a probe failure is an HTTP 429.
func isRateLimitedProbeError(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *playerHTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusTooManyRequests
}

// isLocalProbeTimeoutError reports whether a probe failure is the per-probe
// deadline expiring (videoProbeTimeout), as opposed to the caller's context.
func isLocalProbeTimeoutError(err error) bool {
	return err != nil && errors.Is(err, context.DeadlineExceeded)
}

// mediaHostSuffix is the approved media host boundary. Only hosts strictly
// below this suffix are probed: "rr1.googlevideo.com" and other per-stream
// subdomains qualify. The leading dot is load-bearing, so a lookalike host
// that merely ENDS WITH the same letters ("evilgooglevideo.com") and a host
// that merely STARTS with it ("googlevideo.com.evil.example") are both
// rejected. The bare apex "googlevideo.com" is NOT approved: YouTube always
// serves media from a named subdomain, so there is no justified case for it.
const mediaHostSuffix = ".googlevideo.com"

// validateMediaStreamURL enforces the probe URL policy BEFORE any network I/O:
// HTTPS only, no userinfo, a non-empty host, no IP-literal host, an approved
// media host boundary, a default-or-explicit-443 port, and a strictly parseable
// query. It never resolves DNS and never performs I/O; it is pure validation
// of the URL the extractor is about to probe.
func validateMediaStreamURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("probe URL is unparseable: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("probe URL scheme %q is not https", parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("probe URL must not carry userinfo")
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("probe URL has no host")
	}
	if net.ParseIP(host) != nil {
		return nil, fmt.Errorf("probe URL host %q is an IP literal", host)
	}
	if !strings.HasSuffix(strings.ToLower(host), mediaHostSuffix) || len(host) <= len(mediaHostSuffix) {
		return nil, fmt.Errorf("probe URL host %q is not an approved media host", host)
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return nil, fmt.Errorf("probe URL port %q is not the standard https port", port)
	}
	// Strict query parse: a malformed escape is an explicit rejection, never a
	// silently rebuilt query with unrelated parameters dropped.
	if _, err := url.ParseQuery(parsed.RawQuery); err != nil {
		return nil, fmt.Errorf("probe URL has a malformed query: %w", err)
	}
	return parsed, nil
}

// validatedProbeClient returns a shallow copy of the extractor HTTP client with
// a redirect policy that re-validates every redirect destination under the same
// media URL policy. The shared client is NEVER mutated: Transport, Jar and
// Timeout are copied by reference so connection pooling, cookies and timeouts
// are preserved exactly.
//
// An existing CheckRedirect policy is preserved and its RESULT is honoured
// verbatim: any error it returns (including the sentinel http.ErrUseLastResponse,
// which makes net/http return the redirect response itself) propagates unchanged
// and short-circuits. When it returns nil, the FINAL destination is re-validated:
// the callback receives a mutable *http.Request and may have rewritten req.URL
// to something the pre-callback validation never saw, so trusting that earlier
// check would let an unapproved host through. When the client has no policy of
// its own, the standard 10-redirect limit is applied.
func (e *Extractor) validatedProbeClient() *http.Client {
	base := e.httpClient
	if base == nil {
		base = http.DefaultClient
	}
	previous := base.CheckRedirect
	clone := *base
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if _, err := validateMediaStreamURL(req.URL.String()); err != nil {
			return fmt.Errorf("probe rejected redirect destination %q: %w", req.URL.Redacted(), err)
		}
		if previous == nil {
			if len(via) >= 10 {
				return fmt.Errorf("probe stopped after %d redirects", len(via))
			}
			return nil
		}
		// Honour the caller's policy result first, including its sentinels.
		if err := previous(req, via); err != nil {
			return err
		}
		// The callback may have mutated the request; re-check the destination
		// it actually left behind.
		if _, err := validateMediaStreamURL(req.URL.String()); err != nil {
			return fmt.Errorf("probe rejected redirect destination %q after the client redirect policy: %w", req.URL.Redacted(), err)
		}
		return nil
	}
	return &clone
}

// probeStreamEnd verifies the final byte of a stream URL is fetchable.
// When contentLength is known it requests the final byte directly; otherwise
// it requests byte 0, parses the Content-Range total, then requests the final
// byte. Both steps require 206 with a matching Content-Range.
func (e *Extractor) probeStreamEnd(ctx context.Context, streamURL, userAgent, contentLength string) error {
	ctx, cancel := context.WithTimeout(ctx, videoProbeTimeout)
	defer cancel()

	if total, ok := parseContentLength(contentLength); ok && total > 0 {
		return e.probeRange(ctx, streamURL, userAgent, total-1, total-1, total)
	}

	contentRange, err := e.doRangeProbe(ctx, streamURL, userAgent, 0, 0)
	if err != nil {
		return err
	}
	start, end, total, ok := parseContentRange(contentRange)
	if !ok || start != 0 || end != 0 || total <= 0 {
		return fmt.Errorf("probe failed: discovery Content-Range %q is not 0-0/total", contentRange)
	}
	return e.probeRange(ctx, streamURL, userAgent, total-1, total-1, total)
}

// probeRange fetches one byte range and requires 206 with an exact
// Content-Range match: requested start/end and the expected total.
func (e *Extractor) probeRange(ctx context.Context, streamURL, userAgent string, start, end, total int64) error {
	contentRange, err := e.doRangeProbe(ctx, streamURL, userAgent, start, end)
	if err != nil {
		return err
	}
	gotStart, gotEnd, gotTotal, ok := parseContentRange(contentRange)
	if !ok || gotStart != start || gotEnd != end || gotTotal != total {
		return fmt.Errorf("probe failed: Content-Range %q does not exactly match range %d-%d/%d", contentRange, start, end, total)
	}
	return nil
}

// doRangeProbe builds and executes a single ranged GET with the given UA,
// reading and closing only minimal data. Factored for httptest coverage.
func (e *Extractor) doRangeProbe(ctx context.Context, streamURL, userAgent string, start, end int64) (string, error) {
	// Validate the URL BEFORE any I/O so a non-approved destination never
	// reaches the transport.
	if _, err := validateMediaStreamURL(streamURL); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10))
	req.Header.Set("User-Agent", userAgent)

	resp, err := e.validatedProbeClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("probe request failed: %w", err)
	}
	defer resp.Body.Close()

	// Classify KNOWN status/header failures BEFORE reading the body, so a
	// rate-limited or error response is never delayed by (or blocked on)
	// body delivery. The 429 classification must survive regardless of body
	// state so the video chain still stops on it.
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", &playerHTTPError{Status: resp.StatusCode, Body: "probe rate limited"}
	}
	if resp.StatusCode != http.StatusPartialContent {
		return "", fmt.Errorf("probe failed: got HTTP %d, want 206", resp.StatusCode)
	}
	contentRange := resp.Header.Get("Content-Range")
	if contentRange == "" {
		return "", fmt.Errorf("probe failed: missing Content-Range")
	}

	// Read only the single requested byte: enough to prove the body actually
	// delivers data while keeping the read bounded and the body closable.
	var probeByte = make([]byte, 1)
	_, readErr := io.ReadFull(resp.Body, probeByte)
	// A 206 with a matching Content-Range must still deliver the requested
	// byte: a header-only response with an empty body does not prove the
	// stream end is fetchable, and a failed read must surface its cause.
	if errors.Is(readErr, io.EOF) {
		return "", fmt.Errorf("probe failed: empty body for range %d-%d", start, end)
	}
	if readErr != nil {
		return "", fmt.Errorf("probe failed: reading probe body: %w", readErr)
	}
	return contentRange, nil
}

// parseContentRange parses "bytes {start}-{end}/{total}".
func parseContentRange(v string) (start, end, total int64, ok bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, 0, 0, false
	}
	rest := strings.TrimPrefix(v, "bytes ")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return 0, 0, 0, false
	}
	rangePart, totalPart := rest[:slash], rest[slash+1:]
	dash := strings.Index(rangePart, "-")
	if dash < 0 {
		return 0, 0, 0, false
	}
	var err error
	if start, err = strconv.ParseInt(rangePart[:dash], 10, 64); err != nil {
		return 0, 0, 0, false
	}
	if end, err = strconv.ParseInt(rangePart[dash+1:], 10, 64); err != nil {
		return 0, 0, 0, false
	}
	if total, err = strconv.ParseInt(totalPart, 10, 64); err != nil {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

// parseContentLength parses the innertube contentLength string.
func parseContentLength(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
