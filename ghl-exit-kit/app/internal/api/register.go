package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// MetaScope is the huma.Operation Metadata key holding the operation's
// required Scope. Set it to override the default (ScopeRead for GET and
// HEAD, ScopeAdmin for everything else), e.g. for a read-only POST such as
// a preview:
//
//	Metadata: map[string]any{api.MetaScope: api.ScopeRead}
const MetaScope = "mailroom.scope"

// MetaSensitive, set to true in huma.Operation Metadata, marks an
// operation whose response contains a secret shown only once (a new API
// token, a generated password). Its responses are sent with
// "Cache-Control: no-store" and are never stored for Idempotency-Key
// replay, so the secret is not kept at rest; a retry with the same key
// then answers 409 (the operation was already carried out).
const MetaSensitive = "mailroom.sensitive"

// MetaAlsoResponds, set in huma.Operation Metadata to a map[int]string
// (status -> description), documents further success statuses the
// operation answers with the same body schema as its default response,
// e.g. 200 for a dry run of an operation whose default is 202:
//
//	Metadata: map[string]any{api.MetaAlsoResponds: map[int]string{200: "Dry run: ..."}}
//
// Clients generated from the spec then expect them.
const MetaAlsoResponds = "mailroom.also_responds"

// securityScheme is the OpenAPI name of bearer-token authentication.
const securityScheme = "bearer"

// RouteInfo describes one registered operation, for the UI/API parity
// test (G-1) and documentation.
type RouteInfo struct {
	OperationID string
	Method      string
	Path        string // full path, including Prefix
	Scope       Scope
	Tags        []string
	Summary     string
}

// defaultErrors are documented on every operation.
var defaultErrors = []int{
	http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError,
}

// Register registers a Huma operation on a and records it in the route
// registry. It panics (at wiring time) unless OperationID, Summary,
// Description and at least one tag are set, and on duplicate operation
// IDs. op.Path is relative to Prefix ("/contacts/{id}").
//
// The handler may return core errors; they are converted with MapError.
// Internal errors are logged and answered with a generic 500.
func Register[I, O any](a *API, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	switch {
	case op.OperationID == "":
		panic(fmt.Sprintf("api: %s %s: OperationID is required", op.Method, op.Path))
	case op.Summary == "":
		panic("api: " + op.OperationID + ": Summary is required")
	case op.Description == "":
		panic("api: " + op.OperationID + ": Description is required")
	case len(op.Tags) == 0:
		panic("api: " + op.OperationID + ": at least one tag is required")
	}
	scope := defaultScope(op.Method)
	if s, ok := op.Metadata[MetaScope]; ok {
		sc, ok := s.(Scope)
		if !ok || !sc.Valid() {
			panic(fmt.Sprintf("api: %s: invalid %s metadata %v", op.OperationID, MetaScope, s))
		}
		scope = sc
	}
	if op.Metadata == nil {
		op.Metadata = map[string]any{}
	}
	op.Metadata[MetaScope] = scope
	if op.Extensions == nil {
		op.Extensions = map[string]any{}
	}
	op.Extensions["x-mailroom-scope"] = string(scope)
	op.Security = []map[string][]string{{securityScheme: {}}}
	if scope == ScopeRead {
		op.Description += "\n\nNeeds an API token with scope `read` or `admin`."
	} else {
		op.Description += "\n\nNeeds an API token with scope `admin`."
	}
	op.Middlewares = append(op.Middlewares, rejectUnknownQuery(a, queryParams(reflect.TypeFor[I]())))
	dryRun := hasDryRunParam(reflect.TypeFor[I]())
	dryRunCreate := op.DefaultStatus == http.StatusCreated && dryRun
	if dryRun {
		op.Middlewares = append(op.Middlewares, dryRunAnswers200)
	}
	if sensitive, _ := op.Metadata[MetaSensitive].(bool); sensitive {
		op.Middlewares = append(op.Middlewares, func(ctx huma.Context, next func(huma.Context)) {
			ctx.SetHeader("Cache-Control", "no-store")
			next(ctx)
		})
	}
	for _, code := range defaultErrors {
		if !slices.Contains(op.Errors, code) {
			op.Errors = append(op.Errors, code)
		}
	}

	a.mu.Lock()
	if _, dup := a.opIDs[op.OperationID]; dup {
		a.mu.Unlock()
		panic("api: duplicate OperationID " + op.OperationID)
	}
	a.opIDs[op.OperationID] = struct{}{}
	a.routes = append(a.routes, RouteInfo{
		OperationID: op.OperationID,
		Method:      op.Method,
		Path:        Prefix + op.Path,
		Scope:       scope,
		Tags:        slices.Clone(op.Tags),
		Summary:     op.Summary,
	})
	a.mu.Unlock()

	huma.Register(a.huma, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := handler(ctx, in)
		if err != nil {
			se := MapError(err)
			if se.GetStatus() >= http.StatusInternalServerError {
				a.cfg.Logger.ErrorContext(ctx, "api: operation failed", "operation", op.OperationID, "err", err)
			}
			return nil, se
		}
		return out, nil
	})
	extra, ok := op.Metadata[MetaAlsoResponds].(map[int]string)
	if _, set := op.Metadata[MetaAlsoResponds]; set && !ok {
		panic(fmt.Sprintf("api: %s: %s must be a map[int]string", op.OperationID, MetaAlsoResponds))
	}
	_, has200 := extra[http.StatusOK]
	if dryRunCreate && !has200 {
		extra = maps.Clone(extra)
		if extra == nil {
			extra = map[int]string{}
		}
		extra[http.StatusOK] = dryRunCreateResponse
	}
	if len(extra) > 0 {
		a.addResponses(op.OperationID, extra)
	}
	if dryRunCreate && !has200 {
		a.dropDryRunHeaders(op.OperationID)
	}
}

// dropDryRunHeaders removes the ETag and Location headers from the
// documented 200 (dry run) of operation id: dryRunAnswers200 never sends
// them.
func (a *API) dropDryRunHeaders(id string) {
	for _, item := range a.huma.OpenAPI().Paths {
		for _, o := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if o == nil || o.OperationID != id || o.Responses["200"] == nil {
				continue
			}
			r := *o.Responses["200"]
			r.Headers = maps.Clone(r.Headers)
			delete(r.Headers, "ETag")
			delete(r.Headers, "Location")
			o.Responses["200"] = &r
		}
	}
}

// dryRunCreateResponse documents the 200 of a create operation's dry run.
const dryRunCreateResponse = "Dry run (?dry_run=true): what would be created, validated but not stored. " +
	"Its `id` and any public ID or URL are not kept (they don't exist, and a later create may get them), and there is no ETag"

// queryParams returns the query parameter names an input type declares
// (`query:"name"` tags, embedded structs included).
func queryParams(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		if t.Kind() != reflect.Struct {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if name, _, _ := strings.Cut(f.Tag.Get("query"), ","); name != "" {
				out[name] = true
			} else if f.Anonymous {
				walk(f.Type)
			}
		}
	}
	walk(t)
	return out
}

// rejectUnknownQuery is the per-operation middleware that answers 422
// (field query.<name>) to a query parameter the operation doesn't take,
// like an unknown JSON field: a dry_run sent to an operation without dry
// runs, or a misspelled flag (?dryrun=true), would otherwise be ignored
// and the real write carried out.
func rejectUnknownQuery(a *API, allowed map[string]bool) func(huma.Context, func(huma.Context)) {
	known := slices.Sorted(maps.Keys(allowed))
	return func(ctx huma.Context, next func(huma.Context)) {
		u := ctx.URL()
		q := u.Query()
		names := slices.Sorted(maps.Keys(q))
		for _, name := range names {
			if allowed[name] {
				continue
			}
			var msg string
			switch {
			case name == "dry_run":
				msg = "this operation has no dry run: nothing was changed; the operation's parameters are listed in the API spec"
			case len(known) == 0:
				msg = fmt.Sprintf("unknown query parameter %q: this operation takes none, and nothing was changed", name)
			default:
				msg = fmt.Sprintf("unknown query parameter %q: this operation takes %s, and nothing was changed", name, strings.Join(known, ", "))
			}
			_ = huma.WriteErr(a.huma, ctx, http.StatusUnprocessableEntity, msg, &huma.ErrorDetail{Location: "query." + name, Message: msg})
			return
		}
		next(ctx)
	}
}

// hasDryRunParam reports whether an input type embeds DryRunParam.
func hasDryRunParam(t reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type == reflect.TypeFor[DryRunParam]() || (f.Anonymous && hasDryRunParam(f.Type)) {
			return true
		}
	}
	return false
}

// dryRunAnswers200 is the middleware of operations that take
// ?dry_run=true: a dry run created or changed nothing, so it answers 200
// (G-6: "200 for every dry run") and sends no ETag or Location, which
// would name a resource or a version that was never stored.
func dryRunAnswers200(ctx huma.Context, next func(huma.Context)) {
	if v, err := strconv.ParseBool(ctx.Query("dry_run")); err == nil && v {
		ctx = dryRunContext{ctx}
	}
	next(ctx)
}

// humaContext names the embedded huma.Context of dryRunContext (a field
// named Context would hide the interface's Context method).
type humaContext = huma.Context

// dryRunContext turns 201 into 200 and drops the ETag and Location headers.
type dryRunContext struct{ humaContext }

func (c dryRunContext) SetStatus(code int) {
	if code == http.StatusCreated {
		code = http.StatusOK
	}
	c.humaContext.SetStatus(code)
}

func (c dryRunContext) SetHeader(name, value string) {
	if strings.EqualFold(name, "ETag") || strings.EqualFold(name, "Location") {
		return
	}
	c.humaContext.SetHeader(name, value)
}

func (c dryRunContext) AppendHeader(name, value string) {
	if strings.EqualFold(name, "ETag") || strings.EqualFold(name, "Location") {
		return
	}
	c.humaContext.AppendHeader(name, value)
}

// Unwrap returns the wrapped context (huma.Unwrap).
func (c dryRunContext) Unwrap() huma.Context { return c.humaContext }

// addResponses adds the statuses of extra to operation id in the spec,
// with the content of its lowest documented 2xx response.
func (a *API) addResponses(id string, extra map[int]string) {
	for _, item := range a.huma.OpenAPI().Paths {
		for _, o := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if o == nil || o.OperationID != id {
				continue
			}
			var def *huma.Response
			for code := 200; code < 300 && def == nil; code++ {
				def = o.Responses[strconv.Itoa(code)]
			}
			if def == nil {
				panic("api: " + id + ": no 2xx response to copy for " + MetaAlsoResponds)
			}
			for code, desc := range extra {
				o.Responses[strconv.Itoa(code)] = &huma.Response{Description: desc, Content: def.Content, Headers: def.Headers}
			}
		}
	}
}

func defaultScope(method string) Scope {
	if method == http.MethodGet || method == http.MethodHead {
		return ScopeRead
	}
	return ScopeAdmin
}

// Routes returns every registered operation, sorted by path then method.
func (a *API) Routes() []RouteInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := slices.Clone(a.routes)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// requireScope is the Huma middleware that enforces each operation's
// scope. It fails closed: an operation without scope metadata needs admin.
func (a *API) requireScope(ctx huma.Context, next func(huma.Context)) {
	need := ScopeAdmin
	if op := ctx.Operation(); op != nil {
		if s, ok := op.Metadata[MetaScope].(Scope); ok {
			need = s
		}
	}
	p, ok := PrincipalFromContext(ctx.Context())
	if !ok {
		ctx.SetHeader("WWW-Authenticate", `Bearer realm="mailroom"`)
		_ = huma.WriteErr(a.huma, ctx, http.StatusUnauthorized, "authentication required")
		return
	}
	if !p.Scope.Allows(need) {
		_ = huma.WriteErr(a.huma, ctx, http.StatusForbidden,
			fmt.Sprintf("this operation needs scope %q; the credentials have scope %q", need, p.Scope))
		return
	}
	next(ctx)
}
