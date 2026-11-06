package tr

import (
	"errors"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// End closes span, recording *err as an error status when it is non-nil.
func End(span trace.Span, err *error) {
	EndIgnore(span, err)
}

// EndIgnore is End, except an error matching one of ignore (errors.Is) is an
// expected outcome (e.g. a cache miss) and leaves the span status ok.
func EndIgnore(span trace.Span, err *error, ignore ...error) {
	defer span.End()
	if err != nil && *err != nil && !isAny(*err, ignore) {
		span.RecordError(*err)
		span.SetStatus(codes.Error, (*err).Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
}

func isAny(err error, targets []error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
