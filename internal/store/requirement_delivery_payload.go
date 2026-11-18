package store

import "encoding/json"

// requirementDeliveryPayloadKeys names, for each event kind that
// ListRequirementDeliveryEventsForTasks returns, the payload keys that
// requirement-delivery classification reads: the merge's URL, label, and
// review provenance, the aggregate round's verdict and heads, and the
// context attachment's requirement and version (component-lineage;
// component-task-lifecycle). Every other key stays in the database.
var requirementDeliveryPayloadKeys = map[string][]string{
	"merge.confirmed":             requirementDeliveryMergeKeys,
	"merge.reconciled":            requirementDeliveryMergeKeys,
	"review.round_completed":      {"verdict", "approved_head_sha", "reviews"},
	TaskContextRequirementAdded:   requirementDeliveryAttachmentKeys,
	TaskContextRequirementActive:  requirementDeliveryAttachmentKeys,
	TaskContextRequirementRemoved: requirementDeliveryAttachmentKeys,
}

var (
	requirementDeliveryMergeKeys      = []string{"url", "title", "task_title", "head_sha", "reviewed_head_sha", "approved_head_sha", "factory_review_validated"}
	requirementDeliveryAttachmentKeys = []string{"id", "version", "unconfirmed"}
	requirementDeliveryReviewKeys     = []string{"verdict", "reviewed_commit_sha"}
)

// ProjectRequirementDeliveryPayload returns the part of a delivery event's
// payload that ListRequirementDeliveryEventsForTasks transfers. The memory
// backend applies it, and the durable backends select the same keys in SQL.
//
// Each kept key carries its stored JSON value unchanged, so a malformed value
// still fails the classifier's decode as it did before. A key that the stored
// payload lacks stays absent here; a durable backend may return it as JSON
// null, which every classification reader treats as absent. An object element
// of an array-valued reviews key keeps only its verdict and
// reviewed_commit_sha keys; every other element, and a reviews value that is
// not an array, stays as stored, and no element is dropped or reordered. A
// payload that is not a JSON object, or an event of another kind, is
// returned unchanged.
func ProjectRequirementDeliveryPayload(kind string, payload json.RawMessage) json.RawMessage {
	keys, ok := requirementDeliveryPayloadKeys[kind]
	if !ok {
		return payload
	}
	fields, ok := jsonObjectFields(payload)
	if !ok {
		return payload
	}
	out := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		value, present := fields[key]
		if !present {
			continue
		}
		if key == "reviews" {
			value = projectRequirementDeliveryReviews(value)
		}
		out[key] = value
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return payload
	}
	return encoded
}

func projectRequirementDeliveryReviews(value json.RawMessage) json.RawMessage {
	var elements []json.RawMessage
	if json.Unmarshal(value, &elements) != nil || elements == nil {
		return value
	}
	for i, element := range elements {
		fields, ok := jsonObjectFields(element)
		if !ok {
			continue
		}
		kept := make(map[string]json.RawMessage, len(requirementDeliveryReviewKeys))
		for _, key := range requirementDeliveryReviewKeys {
			if field, present := fields[key]; present {
				kept[key] = field
			}
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			return value
		}
		elements[i] = encoded
	}
	encoded, err := json.Marshal(elements)
	if err != nil {
		return value
	}
	return encoded
}

// jsonObjectFields decodes a JSON object. JSON null and every other shape
// report false.
func jsonObjectFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	return fields, true
}
