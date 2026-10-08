package store

// The activity delta cursor is an event position, never a numeric event ID.
// Event IDs are immutable identity, not commit order: a later event can carry
// a lower ID on PostgreSQL and SingleStore. A delta therefore keeps every
// marker whose last event is strictly after the captured frontier tuple
// (component-persistence; component-http-api).

// ActivityMarkerPosition is the (at, id) position of a marker's last event.
// A marker without an event has the zero position.
func ActivityMarkerPosition(marker ActivityMarker) TaskEventPosition {
	return TaskEventPosition{At: marker.LastEventAt.UTC(), ID: marker.LastEventID}
}

// ActivityPositionAfter reports whether a sorts strictly after b in (at, id)
// order.
func ActivityPositionAfter(a, b TaskEventPosition) bool {
	return a.At.After(b.At) || a.At.Equal(b.At) && a.ID > b.ID
}

// ActivityFrontier is the maximum marker position over one page, compared as
// a whole tuple. The two fields are never maximized independently. It
// returns the zero position when no marker has an event.
func ActivityFrontier(markers []ActivityMarker) TaskEventPosition {
	var frontier TaskEventPosition
	for _, marker := range markers {
		if position := ActivityMarkerPosition(marker); ActivityPositionAfter(position, frontier) {
			frontier = position
		}
	}
	return frontier
}

// ActivityMarkerAfter reports whether a marker's last event is strictly after
// the frontier a client captured. A newer event with a lower ID is after it.
func ActivityMarkerAfter(marker ActivityMarker, frontier TaskEventPosition) bool {
	return ActivityPositionAfter(ActivityMarkerPosition(marker), frontier)
}
