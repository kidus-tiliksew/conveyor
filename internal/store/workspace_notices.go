package store

// workspaceNoticeEventKinds are the task-less workspace events whose commit
// changes what a statement read serves without a task event: a requirement
// version, System Design version, or decision proposed, confirmed, or
// dismissed, and a workspace role granted, changed, or revoked, including an
// invitation redeemed at sign-up. Each raises one `workspace` notice for its
// workspace when it commits, so a response memo that holds the pending
// proposals or the caller's capabilities is dropped at once (DEC-140(3);
// component-persistence invariant 23; component-next-action invariant 58).
// Every backend's shared workspace event writer consults this one list.
var workspaceNoticeEventKinds = map[string]bool{
	"requirement.version_proposed":          true,
	"requirement.version_confirmed":         true,
	"requirement.version_dismissed":         true,
	"requirement.version_retired":           true,
	"requirement.archived":                  true,
	"requirement.restored":                  true,
	"system_design.version_proposed":        true,
	"system_design.version_confirmed":       true,
	"system_design.version_dismissed":       true,
	"system_design.archived":                true,
	"system_design.restored":                true,
	"decision.proposed":                     true,
	"decision.confirmed":                    true,
	"decision.dismissed":                    true,
	"decision.archived":                     true,
	"decision.supersession_sweep_dismissed": true,
	"workspace.membership_granted":          true,
	"workspace.membership_revoked":          true,
}

// WorkspaceEventRaisesNotice reports whether a committed task-less event of
// kind raises a `workspace` notice. A task event raises its `task` notice
// instead, so the caller passes only events without a task ID.
func WorkspaceEventRaisesNotice(kind string) bool {
	return workspaceNoticeEventKinds[kind]
}
