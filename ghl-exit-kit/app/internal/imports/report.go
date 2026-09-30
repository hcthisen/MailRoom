package imports

import (
	"maps"
	"slices"
	"sort"

	"mailroom/internal/core"
)

// ImportReport is the outcome of an import (or of a dry run: what would
// happen). Every data row is counted exactly once:
// rows = created + updated + unchanged + skipped + duplicates_merged + rejected_count.
type ImportReport struct {
	DryRun           bool              `json:"dry_run" doc:"true when nothing was written"`
	OnExisting       string            `json:"on_existing" enum:"update,skip" doc:"What was done with existing contacts"`
	Rows             int               `json:"rows" doc:"Data rows in the file (blank rows are skipped)" example:"1200"`
	Created          int               `json:"created" doc:"New contacts" example:"1100"`
	Updated          int               `json:"updated" doc:"Existing contacts that changed (names, attributes, tags or a blocking status)" example:"60"`
	Unchanged        int               `json:"unchanged" doc:"Existing contacts that already matched the file" example:"20"`
	Skipped          int               `json:"skipped" doc:"Existing contacts left alone because on_existing is skip" example:"0"`
	DuplicatesMerged int               `json:"duplicates_merged" doc:"Rows merged into an earlier row for the same address" example:"5"`
	NotReactivated   int               `json:"not_reactivated" doc:"Existing contacts the file lists as active that stay unsubscribed, bounced, complained or pending (also counted in updated or unchanged). Erased and manually suppressed addresses are rejected instead (reason suppressed)" example:"3"`
	StatusKept       int               `json:"status_kept" doc:"Existing contacts whose status was kept because the file's status would make them easier to reactivate, e.g. bounced to unsubscribed (also counted in updated or unchanged)" example:"1"`
	RejectedCount    int               `json:"rejected_count" doc:"Rows not imported" example:"15"`
	StatusOnly       int               `json:"status_only" doc:"Contacts whose rows were rejected for an invalid field, but whose status that blocks sending (unsubscribed, bounced, complained) was still applied, so the address isn't left sendable (the rows are also counted in rejected_count)" example:"1"`
	Rejected         []ImportRejection `json:"rejected" doc:"The first 1000 rejected rows by line number (line numbers only, never addresses)"`
	StatusCounts     map[string]int    `json:"status_counts" doc:"Contacts by their status after the import: those created, updated, unchanged or skipped, plus status_only contacts whose status was applied" example:"{\"active\":1150,\"unsubscribed\":30}"`
	TagCounts        map[string]int    `json:"tag_counts" doc:"Imported contacts per tag in the file" example:"{\"newsletter\":900}"`
	Warnings         []string          `json:"warnings" doc:"Problems with the file that didn't stop the import, e.g. ignored columns"`
}

// applyStats are the counts of the write phase. They are kept in the job's
// checkpoint, so a resumed job continues them.
type applyStats struct {
	Created        int               `json:"created"`
	Updated        int               `json:"updated"`
	Unchanged      int               `json:"unchanged"`
	Skipped        int               `json:"skipped"`
	NotReactivated int               `json:"not_reactivated"`
	StatusKept     int               `json:"status_kept"`
	StatusOnly     int               `json:"status_only"`
	Rejections     int               `json:"rejections"`
	Rejected       []ImportRejection `json:"rejected"`
	StatusCounts   map[string]int    `json:"status_counts"`
	TagCounts      map[string]int    `json:"tag_counts"`
}

// clone returns a deep copy.
func (st applyStats) clone() applyStats {
	st.Rejected = slices.Clone(st.Rejected)
	st.StatusCounts = maps.Clone(st.StatusCounts)
	st.TagCounts = maps.Clone(st.TagCounts)
	return st
}

// reject counts a record that was not imported.
func (st *applyStats) reject(line int, reason string) {
	st.Rejections++
	if len(st.Rejected) < MaxRejectedListed {
		st.Rejected = append(st.Rejected, ImportRejection{Line: line, Reason: reason})
	}
}

// count records the outcome of an imported record.
func (st *applyStats) count(r *record, p plan) {
	switch p.outcome {
	case outRejected:
		st.reject(r.line, p.reason)
		return
	case outStatusOnly:
		if p.reason != "" {
			st.reject(r.line, statusOnlyReason(p.reason, r.status))
		}
		if p.appliesStatus() {
			st.StatusOnly++
			st.countStatus(p.finalStatus)
		}
		return
	case outCreated:
		st.Created++
	case outUpdated:
		st.Updated++
	case outUnchanged:
		st.Unchanged++
	case outSkipped:
		st.Skipped++
	}
	if p.notReactivated {
		st.NotReactivated++
	}
	if p.statusKept {
		st.StatusKept++
	}
	st.countStatus(p.finalStatus)
	if len(r.tags) > 0 && st.TagCounts == nil {
		st.TagCounts = map[string]int{}
	}
	for _, t := range r.tags {
		st.TagCounts[t]++
	}
}

// countStatus counts a contact's status after the import.
func (st *applyStats) countStatus(s core.ContactStatus) {
	if st.StatusCounts == nil {
		st.StatusCounts = map[string]int{}
	}
	st.StatusCounts[string(s)]++
}

// buildReport combines the parse and write phases.
func buildReport(pf *parsedFile, st applyStats, onExisting string, dryRun bool) ImportReport {
	rejected := append(slices.Clone(pf.rejected), st.Rejected...)
	sort.SliceStable(rejected, func(i, j int) bool { return rejected[i].Line < rejected[j].Line })
	if len(rejected) > MaxRejectedListed {
		rejected = rejected[:MaxRejectedListed]
	}
	rep := ImportReport{
		DryRun: dryRun, OnExisting: onExisting,
		Rows: pf.rows, Created: st.Created, Updated: st.Updated, Unchanged: st.Unchanged, Skipped: st.Skipped,
		DuplicatesMerged: pf.duplicates, NotReactivated: st.NotReactivated, StatusKept: st.StatusKept,
		RejectedCount: pf.rejections + st.Rejections, StatusOnly: st.StatusOnly, Rejected: rejected,
		StatusCounts: maps.Clone(st.StatusCounts), TagCounts: maps.Clone(st.TagCounts),
		Warnings: slices.Clone(pf.warnings),
	}
	if rep.Rejected == nil {
		rep.Rejected = []ImportRejection{}
	}
	if rep.StatusCounts == nil {
		rep.StatusCounts = map[string]int{}
	}
	if rep.TagCounts == nil {
		rep.TagCounts = map[string]int{}
	}
	if rep.Warnings == nil {
		rep.Warnings = []string{}
	}
	return rep
}
