package ui

import (
	"net/http"

	"mailroom/internal/imports"
)

// The "audience" page group: contacts, tags, suppressions, CSV imports
// (under the Contacts nav item) and segments with the filter builder.
// Every action names the API operation whose service method it calls
// (PRD G-1). Identifiers of this group start with "aud" so they can't
// clash with other groups in the package.

// audImportMaxBody is the body limit of the CSV upload: the file limit of
// the imports service plus the multipart overhead and, for the embedded
// commit form, the base64 growth of a small file.
const audImportMaxBody = imports.MaxFileBytes + imports.MaxFileBytes/8 + 1<<20

func init() {
	registerGroup("audience", func(u *UI) {
		get, post := http.MethodGet, http.MethodPost

		// Contacts.
		u.handle(Action{Method: get, Path: Prefix + "/contacts", OperationID: "list-contacts",
			Uses: []string{"list-tags", "list-segments"}, Handler: u.audContactsList})
		u.handle(Action{Method: get, Path: Prefix + "/contacts/new", OperationID: "list-tags",
			Handler: u.audContactNew})
		u.handle(Action{Method: post, Path: Prefix + "/contacts", OperationID: "create-contact",
			Uses: []string{"list-tags"}, Handler: u.audContactCreate})
		u.handle(Action{Method: post, Path: Prefix + "/contacts/bulk-tag", OperationID: "bulk-tag",
			Handler: u.audBulkTag})
		u.handle(Action{Method: get, Path: Prefix + "/contacts/{id}", OperationID: "get-contact",
			Uses: []string{"get-contact-timeline", "list-contact-enrollments", "list-tags"}, Handler: u.audContactShow})
		u.handle(Action{Method: get, Path: Prefix + "/contacts/{id}/edit", OperationID: "get-contact",
			Handler: u.audContactEdit})
		u.handle(Action{Method: post, Path: Prefix + "/contacts/{id}/edit", OperationID: "update-contact",
			Handler: u.audContactUpdate})
		u.handle(Action{Method: post, Path: Prefix + "/contacts/{id}/tags", OperationID: "add-contact-tags",
			Handler: u.audContactAddTags})
		u.handle(Action{Method: post, Path: Prefix + "/contacts/{id}/tags/remove", OperationID: "remove-contact-tag",
			Handler: u.audContactRemoveTag})
		u.handle(Action{Method: post, Path: Prefix + "/contacts/{id}/status", OperationID: "set-contact-status",
			Handler: u.audContactStatus})
		u.handle(Action{Method: get, Path: Prefix + "/contacts/{id}/export", OperationID: "export-contact",
			Handler: u.audContactExport})
		u.handle(Action{Method: post, Path: Prefix + "/contacts/{id}/erase", OperationID: "erase-contact",
			Handler: u.audContactErase})

		// Tags.
		u.handle(Action{Method: get, Path: Prefix + "/tags", OperationID: "list-tags", Handler: u.audTagsList})
		u.handle(Action{Method: post, Path: Prefix + "/tags", OperationID: "create-tag", Handler: u.audTagCreate})
		u.handle(Action{Method: get, Path: Prefix + "/tags/{id}", OperationID: "get-tag",
			Uses: []string{"list-tags"}, Handler: u.audTagShow})
		u.handle(Action{Method: post, Path: Prefix + "/tags/{id}/rename", OperationID: "rename-tag",
			Handler: u.audTagRename})
		u.handle(Action{Method: post, Path: Prefix + "/tags/{id}/merge", OperationID: "merge-tag",
			Uses: []string{"get-tag", "list-tags"}, Handler: u.audTagMerge})
		u.handle(Action{Method: post, Path: Prefix + "/tags/{id}/delete", OperationID: "delete-tag",
			Uses: []string{"get-tag"}, Handler: u.audTagDelete})

		// Suppressions.
		u.handle(Action{Method: get, Path: Prefix + "/suppressions", OperationID: "list-suppressions",
			Handler: u.audSuppressionsList})
		u.handle(Action{Method: post, Path: Prefix + "/suppressions", OperationID: "add-suppression",
			Handler: u.audSuppressionAdd})
		u.handle(Action{Method: post, Path: Prefix + "/suppressions/remove", OperationID: "remove-suppression",
			Handler: u.audSuppressionRemove})

		// CSV imports.
		u.handle(Action{Method: get, Path: Prefix + "/imports", OperationID: "list-imports", Handler: u.audImportsList})
		u.handle(Action{Method: post, Path: Prefix + "/imports", OperationID: "create-import",
			MaxBodyBytes: audImportMaxBody, Handler: u.audImportCreate})
		u.handle(Action{Method: get, Path: Prefix + "/imports/{job_id}", OperationID: "get-import",
			Handler: u.audImportShow})

		// Segments.
		u.handle(Action{Method: get, Path: Prefix + "/segments", OperationID: "list-segments",
			Handler: u.audSegmentsList})
		u.handle(Action{Method: get, Path: Prefix + "/segments/new", OperationID: "list-tags",
			Handler: u.audSegmentNew})
		u.handle(Action{Method: post, Path: Prefix + "/segments", OperationID: "create-segment",
			Uses: []string{"list-tags"}, Handler: u.audSegmentCreate})
		u.handle(Action{Method: post, Path: Prefix + "/segments/preview", OperationID: "preview-segment",
			Handler: u.audSegmentPreview})
		u.handle(Action{Method: post, Path: Prefix + "/segments/builder", OperationID: "preview-segment",
			Uses: []string{"list-tags"}, Handler: u.audSegmentBuilder})
		u.handle(Action{Method: get, Path: Prefix + "/segments/{id}", OperationID: "get-segment",
			Uses: []string{"get-segment-count", "list-segment-members", "list-tags"}, Handler: u.audSegmentShow})
		u.handle(Action{Method: get, Path: Prefix + "/segments/{id}/count", OperationID: "get-segment-count",
			Handler: u.audSegmentCount})
		u.handle(Action{Method: get, Path: Prefix + "/segments/{id}/edit", OperationID: "get-segment",
			Uses: []string{"list-tags"}, Handler: u.audSegmentEdit})
		u.handle(Action{Method: post, Path: Prefix + "/segments/{id}/edit", OperationID: "update-segment",
			Uses: []string{"list-tags"}, Handler: u.audSegmentUpdate})
		u.handle(Action{Method: post, Path: Prefix + "/segments/{id}/delete", OperationID: "delete-segment",
			Handler: u.audSegmentDelete})
	})
}
