package knowledge

import "time"

// SetNow fixes the clock that dates written documents.
func SetNow(b *Base, now func() time.Time) { b.now = now }

// Limits the black-box tests run into.
const (
	ListLimit      = listLimit
	MaxReadLines   = maxReadLines
	MaxReadBytes   = maxReadBytes
	MaxOutline     = maxOutline
	MaxOutlineLen  = maxOutlineLen
	MaxHeading     = maxHeading
	MaxClient      = maxClient
	MaxAuthor      = maxAuthor
	MaxSubject     = maxSubject
	CommitAttempts = commitAttempts
)

// OutlineSize is what an outline counts against a read's byte cap.
var OutlineSize = outlineSize

// Sizes of the rest of a page, as they count against a read's byte cap.
var (
	MetadataSize = metadataSize
	WarningsSize = warningsSize
	TextSize     = textSize
)
