package store

// MemoryTimelineReads returns how many whole task timelines the volatile
// backend st has copied through ListEvents, which its compact and context
// reads also call. It lets an external test prove that a path reaches no
// such copy, which a wrapper around the store cannot observe.
func MemoryTimelineReads(st Store) int64 {
	if m, ok := st.(interface{ memoryTimelineReads() int64 }); ok {
		return m.memoryTimelineReads()
	}
	return -1
}

func (m *memory) memoryTimelineReads() int64 { return m.timelineReads.Load() }
