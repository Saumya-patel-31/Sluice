package policy

import (
	"sort"
	"strings"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
)

// Env is the attribute set a policy expression is evaluated against.
//
// Only what is in here can influence a decision. That is the property that
// makes a decision reproducible from its ledger entry: replaying the recorded
// subject and request through the same policy set must yield the same verdict,
// which cannot be true if expressions can reach ambient state.
type Env struct {
	roots map[string]Value
}

// NewEnv returns an empty environment.
func NewEnv() *Env { return &Env{roots: make(map[string]Value, 5)} }

// Set binds a root name.
func (e *Env) Set(name string, v Value) { e.roots[name] = v }

// Lookup resolves a root name.
func (e *Env) Lookup(name string) (Value, bool) {
	v, ok := e.roots[name]
	return v, ok
}

// RootNames returns the bound root names, for error messages.
func (e *Env) RootNames() string {
	names := make([]string, 0, len(e.roots))
	for k := range e.roots {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// SubjectValue projects an authenticated identity into the policy namespace.
func SubjectValue(s *model.Subject) Value {
	if s == nil {
		return Map(map[string]Value{
			"id": Str(""), "trust_domain": Str(""), "namespace": Str(""),
			"service": Str(""), "issuer": Str(""),
			"mtls": Bool(false), "authenticated": Bool(false),
			"claims": Map(map[string]Value{}),
		})
	}
	return Map(map[string]Value{
		"id":            Str(s.ID),
		"trust_domain":  Str(s.TrustDomain),
		"namespace":     Str(s.Namespace),
		"service":       Str(s.Service),
		"issuer":        Str(s.Issuer),
		"mtls":          Bool(s.MTLS),
		"authenticated": Bool(s.Authenticated),
		"claims":        StrMap(s.Claims),
	})
}

// RequestValue projects request attributes into the policy namespace.
func RequestValue(r *model.Request) Value {
	if r == nil {
		return Map(map[string]Value{
			"method": Str(""), "path": Str(""), "host": Str(""),
			"source_ip": Str(""), "source_geo": Str(""),
			"data_class": Str(string(model.DataInternal)),
			"bytes":      Num(0),
			"headers":    Map(map[string]Value{}),
		})
	}
	dc := r.DataClass
	if dc == "" {
		dc = model.DataInternal
	}
	// Header keys are lowercased so a policy can be written one way and match
	// regardless of how the peer capitalised them.
	headers := make(map[string]Value, len(r.Headers))
	for k, v := range r.Headers {
		headers[strings.ToLower(k)] = Str(v)
	}
	return Map(map[string]Value{
		"method":     Str(r.Method),
		"path":       Str(r.Path),
		"host":       Str(r.Host),
		"source_ip":  Str(r.SourceIP),
		"source_geo": Str(r.SourceGeo),
		"data_class": Str(string(dc)),
		"bytes":      Num(float64(r.EstimatedBytes)),
		"headers":    Map(headers),
	})
}

// BackendValue projects a candidate backend into the policy namespace. It is
// bound only while evaluating a `require` clause, since a `when` clause
// decides whether a policy applies to the request at all, before any candidate
// is under consideration.
func BackendValue(b *model.Backend) Value {
	if b == nil {
		return Null
	}
	return Map(map[string]Value{
		"id":           Str(b.ID),
		"cloud":        Str(string(b.Cloud)),
		"region":       Str(b.Region),
		"jurisdiction": Str(b.Jurisdiction),
		"grid_zone":    Str(b.GridZone),
		"tier":         Str(b.Tier),
		"enabled":      Bool(b.Enabled),
		"labels":       StrMap(b.Labels),
	})
}

// TimeValue exposes wall-clock attributes for time-of-day policies, such as
// pinning batch traffic to off-peak windows.
func TimeValue(t time.Time) Value {
	u := t.UTC()
	return Map(map[string]Value{
		"hour":       Num(float64(u.Hour())),
		"minute":     Num(float64(u.Minute())),
		"weekday":    Num(float64(int(u.Weekday()))),
		"day":        Str(strings.ToLower(u.Weekday().String())),
		"unix":       Num(float64(u.Unix())),
		"is_weekend": Bool(u.Weekday() == time.Saturday || u.Weekday() == time.Sunday),
	})
}

// BuildEnv constructs the request-scoped environment. The backend root is
// bound to null until SetBackend is called.
func BuildEnv(s *model.Subject, r *model.Request, now time.Time) *Env {
	e := NewEnv()
	e.Set("subject", SubjectValue(s))
	e.Set("request", RequestValue(r))
	e.Set("time", TimeValue(now))
	e.Set("backend", Null)
	return e
}

// SetBackend rebinds the backend root in place.
//
// A decision evaluates every constraint against every candidate, so this is
// the innermost loop of the request path. Rebinding one root beats rebuilding
// the environment per candidate, and is safe because a single decision is
// evaluated by exactly one goroutine.
func (e *Env) SetBackend(b *model.Backend) { e.roots["backend"] = BackendValue(b) }

// AttributeCatalogue returns the full addressable attribute namespace, used by
// the policy editor for completion and by the docs generator.
func AttributeCatalogue() map[string][]string {
	return map[string][]string{
		"subject": {"id", "trust_domain", "namespace", "service", "issuer", "mtls", "authenticated", `claims["..."]`},
		"request": {"method", "path", "host", "source_ip", "source_geo", "data_class", "bytes", `headers["..."]`},
		"backend": {"id", "cloud", "region", "jurisdiction", "grid_zone", "tier", "enabled", `labels["..."]`},
		"time":    {"hour", "minute", "weekday", "day", "unix", "is_weekend"},
	}
}
