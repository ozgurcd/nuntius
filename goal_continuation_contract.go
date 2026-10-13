package nuntius

import "time"

// This is a trusted in-process adapter seam, not a caller-supplied JSON grant.
// The resolver is not persisted. It must independently reobserve capabilities,
// roots and source/claims for each query, and must not reenter Manager or launch
// work. Both initial selection and the final pre-OS guard invoke it under the
// existing manager -> control interval. A reopened manager needs a new resolver;
// retained hashes, timestamps and approval records cannot reconstruct one.
type goalContinuationResolver func(goalContinuationQuery) (goalContinuationContext, error)

type goalContinuationQuery struct {
	GoalID, Lane, ParentID, Branch string
	ObservedAfter                  time.Time
}

type goalContinuationContext struct {
	Dispatch   goalDispatchContext
	ObservedAt time.Time
}
