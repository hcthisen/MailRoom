package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"mailroom/internal/core"
	"mailroom/internal/segments"
)

// audSampleSize is the number of contacts the builder's live preview
// shows.
const audSampleSize = 10

func audSegmentURL(id int64) string { return fmt.Sprintf("%s/segments/%d", Prefix, id) }

// ---------------------------------------------------------------- list

type audSegmentRow struct {
	S           segments.Segment
	Description string
}

type audSegmentsData struct {
	Rows  []audSegmentRow
	Pager Pager
}

// audSegmentsList lists saved segments (list-segments). Member counts
// load per row from get-segment-count.
func (u *UI) audSegmentsList(w http.ResponseWriter, r *http.Request) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	list, err := u.d.Segments.List(r.Context(), p.After, p.Fetch())
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, list, func(s segments.Segment) int64 { return s.ID })
	d := audSegmentsData{Pager: pager}
	for _, s := range items {
		d.Rows = append(d.Rows, audSegmentRow{S: s, Description: audDescribe(s.Filter)})
	}
	return u.Page(w, r, "audience/segments", View{Title: "Segments", Nav: "segments",
		Subtitle: "Saved filters, evaluated whenever they are used.", Data: d})
}

// audSegmentCount is a segment's live member count (get-segment-count),
// loaded into the list and detail pages by htmx.
func (u *UI) audSegmentCount(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	c, err := u.d.Segments.CountSegment(r.Context(), id)
	if err != nil {
		return err
	}
	return u.Partial(w, r, "audience/segments", "segment-count", View{Data: c})
}

// ---------------------------------------------------------------- builder pages

type audBuilderData struct {
	Root       *audNode
	Tags       []string
	Statuses   []Option
	Fields     []Option
	ValueTypes []Option
	Matches    []Option
}

type audSegmentFormData struct {
	Form    *Form // name, version
	Builder audBuilderData
	Action  string
	Segment *segments.Segment // edit only
}

func (u *UI) audBuilder(r *http.Request, root *audNode, errs map[string]string) audBuilderData {
	if errs == nil {
		errs = map[string]string{}
	}
	audSetErrors(root, errs)
	return audBuilderData{Root: root, Tags: u.audTagNames(r.Context()), Statuses: audStatusOptions(""),
		Fields: audFields, ValueTypes: audValueTypes, Matches: audMatches}
}

func (u *UI) audSegmentFormView(r *http.Request, f *Form, root *audNode, seg *segments.Segment) View {
	d := audSegmentFormData{Form: f, Builder: u.audBuilder(r, root, f.Errors), Action: Prefix + "/segments", Segment: seg}
	v := View{Title: "New segment", Nav: "segments", Crumbs: []Crumb{{"Segments", Prefix + "/segments"}}, Data: d}
	if seg != nil {
		d.Action = audSegmentURL(seg.ID) + "/edit"
		v.Title = "Edit " + seg.Name
		v.Crumbs = append(v.Crumbs, Crumb{seg.Name, audSegmentURL(seg.ID)})
		v.Data = d
	}
	return v
}

// audSegmentNew is the empty builder (tag suggestions from list-tags).
func (u *UI) audSegmentNew(w http.ResponseWriter, r *http.Request) error {
	root := &audNode{Path: "filter", Depth: 1, Group: true, Match: segments.MatchAll}
	return u.Page(w, r, "audience/segment_form", u.audSegmentFormView(r, EmptyForm(), root, nil))
}

// audSegmentCreate saves a new segment (create-segment). A post with an
// "edit" value (the builder's buttons without JavaScript) only changes
// the builder and shows the form again.
func (u *UI) audSegmentCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	root := audParseBuilder(f)
	if edit := f.Get("edit"); edit != "" {
		audApplyEdit(root, edit)
		return u.Page(w, r, "audience/segment_form", u.audSegmentFormView(r, f, root, nil))
	}
	filter, err := u.audBuildFilter(root)
	if err == nil {
		var s segments.Segment
		if s, err = u.d.Segments.Create(r.Context(), segments.CreateInput{Name: f.Get("name"), Filter: filter}, false); err == nil {
			u.Flash(r, FlashSuccess, "Segment "+s.Name+" saved.")
			u.Redirect(w, r, audSegmentURL(s.ID))
			return nil
		}
	}
	if st := f.Fail(err); st != 0 {
		return u.PageStatus(w, r, st, "audience/segment_form", u.audSegmentFormView(r, f, root, nil))
	}
	return err
}

// audSegmentEdit is the builder for a saved segment (get-segment).
func (u *UI) audSegmentEdit(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	s, err := u.d.Segments.Get(r.Context(), id)
	if err != nil {
		return err
	}
	f := EmptyForm().Set("name", s.Name).Set("version", strconv.FormatInt(s.Version, 10))
	return u.Page(w, r, "audience/segment_form", u.audSegmentFormView(r, f, u.audNodeFromFilter(s.Filter), &s))
}

// audSegmentUpdate saves a segment's name and filter (update-segment);
// the version guards against overwriting a newer change (412).
func (u *UI) audSegmentUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	s, err := u.d.Segments.Get(ctx, id)
	if err != nil {
		return err
	}
	f := NewForm(r)
	root := audParseBuilder(f)
	if edit := f.Get("edit"); edit != "" {
		audApplyEdit(root, edit)
		return u.Page(w, r, "audience/segment_form", u.audSegmentFormView(r, f, root, &s))
	}
	filter, err := u.audBuildFilter(root)
	if err == nil {
		name := f.Get("name")
		if _, err = u.d.Segments.Update(ctx, id, segments.UpdateInput{Name: &name, Filter: &filter}, audVersion(f), false); err == nil {
			u.Flash(r, FlashSuccess, "Segment saved.")
			u.Redirect(w, r, audSegmentURL(id))
			return nil
		}
	}
	if st := f.Fail(err); st != 0 {
		return u.PageStatus(w, r, st, "audience/segment_form", u.audSegmentFormView(r, f, root, &s))
	}
	return err
}

// ---------------------------------------------------------------- live preview

type audPreviewData struct {
	P       segments.SegmentPreview
	Error   string
	Counted bool
	// Incomplete: a rule has no value yet; the count waits for it.
	Incomplete bool
}

// audPreview evaluates the builder's filter (preview-segment).
func (u *UI) audPreview(r *http.Request, root *audNode) (audPreviewData, error) {
	filter, err := u.audBuildFilter(root)
	if err == nil {
		var p segments.SegmentPreview
		if p, err = u.d.Segments.Preview(r.Context(), filter, audSampleSize); err == nil {
			return audPreviewData{P: p, Counted: true}, nil
		}
	}
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		if strings.Contains(ve.Message, "must not be empty") || strings.HasPrefix(ve.Message, "is required") {
			// A condition still being filled in is not an error yet.
			return audPreviewData{Incomplete: true}, nil
		}
		return audPreviewData{Error: audHumanError(ve.Field, ve.Message)}, nil
	}
	if errors.Is(err, core.ErrValidation) {
		return audPreviewData{Error: err.Error()}, nil
	}
	return audPreviewData{}, err
}

// audSegmentPreview answers the builder's live count and sample
// (preview-segment) as the preview panel fragment.
func (u *UI) audSegmentPreview(w http.ResponseWriter, r *http.Request) error {
	d, err := u.audPreview(r, audParseBuilder(NewForm(r)))
	if err != nil {
		return err
	}
	return u.Partial(w, r, "audience/segment_form", "segment-preview", View{Data: d})
}

type audBuilderResponse struct {
	Builder audBuilderData
	Preview audPreviewData
}

// audSegmentBuilder applies a structural edit (or a field change) and
// answers the re-rendered builder plus the new preview (out of band).
func (u *UI) audSegmentBuilder(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	root := audParseBuilder(f)
	audApplyEdit(root, f.Get("edit"))
	p, err := u.audPreview(r, root)
	if err != nil {
		return err
	}
	return u.Partial(w, r, "audience/segment_form", "segment-builder-response",
		View{Data: audBuilderResponse{Builder: u.audBuilder(r, root, nil), Preview: p}})
}

// ---------------------------------------------------------------- detail

type audSegmentData struct {
	S           segments.Segment
	Description string
	JSON        string
	Counts      segments.SegmentCounts
	CountErr    string
	Members     []segments.SegmentMember
	Pager       Pager
	Status      string
	Statuses    []Option
	Back        string
	Tags        []string // bulk tag suggestions
}

// audSegmentShow is a segment's page: its filter, live counts and member
// list (get-segment, get-segment-count, list-segment-members).
func (u *UI) audSegmentShow(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	s, err := u.d.Segments.Get(ctx, id)
	if err != nil {
		return err
	}
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	d := audSegmentData{S: s, Description: audDescribe(s.Filter), Status: status,
		Statuses: audStatusOptions("Any status"), Back: r.URL.RequestURI(), Tags: u.audTagNames(ctx)}
	if b, err := json.MarshalIndent(s.Filter, "", "  "); err == nil {
		d.JSON = string(b)
	}
	if d.Counts, err = u.d.Segments.CountSegment(ctx, id); err != nil {
		return err
	}
	ms, err := u.d.Segments.SegmentMembers(ctx, id, segments.MemberQuery{AfterID: p.After, Limit: p.Fetch(), Status: core.ContactStatus(status)})
	if err != nil {
		return err
	}
	d.Members, d.Pager = Paginate(r, p, ms, func(m segments.SegmentMember) int64 { return m.ID })
	return u.Page(w, r, "audience/segment", View{Title: s.Name, Subtitle: d.Description, Nav: "segments",
		Crumbs: []Crumb{{"Segments", Prefix + "/segments"}}, Data: d})
}

// audSegmentDelete deletes a segment (delete-segment). The dry run
// checks first that no unfinished broadcast uses it.
func (u *UI) audSegmentDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	s, err := u.d.Segments.Get(ctx, id)
	if err != nil {
		return err
	}
	f := NewForm(r)
	refuse := func(err error) error {
		ff := EmptyForm()
		if ff.Fail(err) == 0 {
			return err
		}
		u.Flash(r, FlashError, audFormMessage(ff))
		u.Redirect(w, r, audSegmentURL(id))
		return nil
	}
	if !Confirmed(r) {
		if err := u.d.Segments.Delete(ctx, id, audVersion(f), true); err != nil {
			return refuse(err)
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   fmt.Sprintf("Delete segment %q?", s.Name),
			Message: "Only the saved filter is deleted; its contacts stay as they are. Broadcasts already sent to it keep their recipients.",
			Details: []string{"Filter: " + audDescribe(s.Filter)},
			Action:  audSegmentURL(id) + "/delete", Fields: f.Values, ConfirmLabel: "Delete segment",
			CancelURL: audSegmentURL(id), Danger: true, Nav: "segments",
			Crumbs: []Crumb{{"Segments", Prefix + "/segments"}, {s.Name, audSegmentURL(id)}},
		})
	}
	if err := u.d.Segments.Delete(ctx, id, audVersion(f), false); err != nil {
		return refuse(err)
	}
	u.Flash(r, FlashSuccess, "Segment "+s.Name+" deleted.")
	u.Redirect(w, r, Prefix+"/segments")
	return nil
}
