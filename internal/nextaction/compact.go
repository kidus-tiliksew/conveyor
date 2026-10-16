package nextaction

// CompactFrom copies the shared fields, including the catalogue label. Blocks
// is always an array, empty when nothing waits on the task.
func CompactFrom(na NextAction) Compact {
	return Compact{
		Label: na.Label, Actor: na.Actor, Kind: na.Kind, Judgment: na.Judgment,
		Summary: na.Summary, Deadline: na.Deadline, BlockedBy: append([]BlockerRef(nil), na.BlockedBy...),
		Blocks: append([]BlockerRef{}, na.Blocks...),
	}
}
