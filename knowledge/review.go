package knowledge

// ExtractionReviewRequired distinguishes rejected model output from transport
// failures. Workers persist it for review rather than retrying or committing it.
type ExtractionReviewRequired struct{ Err error }

func (e *ExtractionReviewRequired) Error() string {
	return "extractive selection needs review: " + e.Err.Error()
}
func (e *ExtractionReviewRequired) Unwrap() error { return e.Err }
