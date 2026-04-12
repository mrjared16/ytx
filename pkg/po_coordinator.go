package ytx

import "context"

type poCoordinator struct {
	extractor *Extractor
}

func (e *Extractor) newPOCoordinator() *poCoordinator {
	return &poCoordinator{extractor: e}
}

func (c *poCoordinator) acquireTokens(ctx context.Context, videoID string) (string, string, error) {
	if c == nil || c.extractor == nil {
		return "", "", NewPOError(POFailureExtractorBug, "po coordinator not initialized", nil)
	}
	return c.extractor.ensurePOTokenContext(ctx, videoID)
}
