package nextaction

// CompactFrom copies the shared fields, including the catalogue label.
func CompactFrom(na NextAction) Compact {
	return Compact{
		Label: na.Label, Actor: na.Actor, Kind: na.Kind, Judgment: na.Judgment,
		Summary: na.Summary, Deadline: na.Deadline, BlockedBy: append([]BlockerRef(nil), na.BlockedBy...),
	}
}
