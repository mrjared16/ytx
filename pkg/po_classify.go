package ytx

import (
	"errors"
	"fmt"
	"strings"
)

type POFailureKind string

const (
	POFailureRequired               POFailureKind = "po_required"
	POFailureChallengeUnavailable   POFailureKind = "challenge_unavailable"
	POFailureAttGetChallenge        POFailureKind = "att_get_challenge_failure"
	POFailureRuntimeMint            POFailureKind = "runtime_mint_failure"
	POFailureTokenUnavailable       POFailureKind = "token_unavailable"
	POFailureVerificationInconcluse POFailureKind = "verification_inconclusive"
	POFailureExtractorBug           POFailureKind = "extractor_bug"
)

type POError struct {
	Kind    POFailureKind
	Message string
	Cause   error
}

func (e *POError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		if e.Cause != nil {
			return fmt.Sprintf("%s: %v", e.Message, e.Cause)
		}
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return string(e.Kind)
}

func (e *POError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewPOError(kind POFailureKind, msg string, cause error) error {
	return &POError{Kind: kind, Message: msg, Cause: cause}
}

func IsPOErrorKind(err error, kind POFailureKind) bool {
	var poErr *POError
	if !errors.As(err, &poErr) {
		return false
	}
	return poErr.Kind == kind
}

func ClassifyPOFailure(err error) POFailureKind {
	if err == nil {
		return ""
	}
	var poErr *POError
	if errors.As(err, &poErr) {
		return poErr.Kind
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "challenge") || strings.Contains(lower, "botguard") || strings.Contains(lower, "po token") {
		return POFailureRequired
	}
	return POFailureExtractorBug
}

func IsLikelyPORequiredError(err error) bool {
	if err == nil {
		return false
	}
	kind := ClassifyPOFailure(err)
	if kind == POFailureRequired || kind == POFailureChallengeUnavailable || kind == POFailureAttGetChallenge || kind == POFailureTokenUnavailable {
		return true
	}

	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "confirm you're not a bot"):
		return true
	case strings.Contains(lower, "page needs to be reloaded"):
		return true
	case strings.Contains(lower, "service integrity"):
		return true
	case strings.Contains(lower, "botguard"):
		return true
	case strings.Contains(lower, "challenge") && strings.Contains(lower, "music"):
		return true
	default:
		return false
	}
}

func IsLikelyPORequiredFromPlayerResponse(resp *PlayerResponse) bool {
	if resp == nil {
		return false
	}
	lowerReason := strings.ToLower(resp.PlayabilityStatus.Reason)
	if strings.Contains(lowerReason, "confirm you're not a bot") || strings.Contains(lowerReason, "page needs to be reloaded") {
		return true
	}

	status := strings.ToUpper(resp.PlayabilityStatus.Status)
	if status != "UNPLAYABLE" && status != "ERROR" {
		return false
	}

	for _, p := range resp.ResponseContext.ServiceTrackingParams {
		for _, kv := range p.Params {
			if kv.Key == "e" && (strings.Contains(kv.Value, "51217476") || strings.Contains(kv.Value, "51217102")) {
				return true
			}
			if strings.EqualFold(kv.Key, "html5_generate_content_po_token") && strings.EqualFold(kv.Value, "true") {
				return true
			}
		}
	}

	return false
}
