package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// Effect is what a policy does when its `when` clause matches.
type Effect string

const (
	// EffectAllow permits the request. At least one allow must match or the
	// request is denied by default.
	EffectAllow Effect = "allow"
	// EffectDeny rejects the request. Deny overrides allow unconditionally.
	EffectDeny Effect = "deny"
	// EffectConstrain does not decide authorisation; it removes candidate
	// backends whose attributes fail the `require` clause. This is how data
	// residency is expressed: the request is fine, but only some destinations
	// are.
	EffectConstrain Effect = "constrain"
	// EffectPrefer reshapes the objective weights for matching traffic, so
	// one class of request can optimise for latency while another optimises
	// for cost, under the same router.
	EffectPrefer Effect = "prefer"
)

func parseEffect(s string) (Effect, bool) {
	switch strings.ToLower(s) {
	case "allow":
		return EffectAllow, true
	case "deny":
		return EffectDeny, true
	case "constrain":
		return EffectConstrain, true
	case "prefer":
		return EffectPrefer, true
	}
	return "", false
}

// Policy is one compiled statement.
type Policy struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Priority    int                `json:"priority"`
	Effect      Effect             `json:"effect"`
	Message     string             `json:"message,omitempty"`
	Tags        []string           `json:"tags,omitempty"`
	When        Expr               `json:"-"`
	Require     Expr               `json:"-"`
	Prefer      map[string]float64 `json:"prefer,omitempty"`
}

// MarshalJSON renders the policy with its expressions as source text so the
// API can round-trip it into the dashboard editor.
func (p *Policy) MarshalJSON() ([]byte, error) {
	type alias Policy
	out := struct {
		*alias
		WhenSrc    string `json:"when,omitempty"`
		RequireSrc string `json:"require,omitempty"`
	}{alias: (*alias)(p)}
	if p.When != nil {
		out.WhenSrc = p.When.String()
	}
	if p.Require != nil {
		out.RequireSrc = p.Require.String()
	}
	return jsonMarshal(out)
}

func (p *Policy) validate() error {
	switch p.Effect {
	case EffectAllow, EffectDeny:
		if p.Require != nil {
			return fmt.Errorf("%s policies cannot have a 'require' clause; use effect constrain", p.Effect)
		}
		if p.Prefer != nil {
			return fmt.Errorf("%s policies cannot have a 'prefer' clause", p.Effect)
		}
	case EffectConstrain:
		if p.Require == nil {
			return fmt.Errorf("constrain policies need a 'require' clause")
		}
		if p.Prefer != nil {
			return fmt.Errorf("constrain policies cannot have a 'prefer' clause")
		}
	case EffectPrefer:
		if len(p.Prefer) == 0 {
			return fmt.Errorf("prefer policies need a 'prefer' block")
		}
		if p.Require != nil {
			return fmt.Errorf("prefer policies cannot have a 'require' clause")
		}
		for k := range p.Prefer {
			if _, ok := model.ParseDimension(k); !ok {
				return fmt.Errorf("unknown objective %q in prefer block (want cost, latency, carbon or reliability)", k)
			}
		}
	default:
		return fmt.Errorf("policy has no effect")
	}
	return nil
}

// -----------------------------------------------------------------------------
// Policy set
// -----------------------------------------------------------------------------

// Set is a compiled, immutable policy document.
type Set struct {
	policies []*Policy
	source   string
	hash     string
	loadedAt time.Time
}

// Compile parses and validates a policy document.
func Compile(src string) (*Set, error) {
	pols, err := Parse(src)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(pols))
	for _, p := range pols {
		if seen[p.Name] {
			return nil, fmt.Errorf("policy: duplicate policy name %q", p.Name)
		}
		seen[p.Name] = true
	}

	// Evaluation order is by ascending priority, ties broken by name so a
	// reload of the same document always produces the same trace.
	sort.SliceStable(pols, func(i, j int) bool {
		if pols[i].Priority != pols[j].Priority {
			return pols[i].Priority < pols[j].Priority
		}
		return pols[i].Name < pols[j].Name
	})

	sum := sha256.Sum256([]byte(src))
	return &Set{
		policies: pols,
		source:   src,
		hash:     hex.EncodeToString(sum[:])[:12],
		loadedAt: time.Now(),
	}, nil
}

// Policies returns the compiled policies in evaluation order.
func (s *Set) Policies() []*Policy { return s.policies }

// Source returns the original document text.
func (s *Set) Source() string { return s.source }

// Hash is a short content digest, used as a cache-invalidation key and shown
// in the dashboard so an operator can confirm which document is live.
func (s *Set) Hash() string { return s.hash }

// LoadedAt is when the set was compiled.
func (s *Set) LoadedAt() time.Time { return s.loadedAt }

// Len returns the number of policies.
func (s *Set) Len() int { return len(s.policies) }

// -----------------------------------------------------------------------------
// Evaluation
// -----------------------------------------------------------------------------

// Input is one authorisation and candidate-selection question.
type Input struct {
	Subject    *model.Subject
	Request    *model.Request
	Candidates []model.Backend
	Now        time.Time
	// BaseObjectives are the route's configured weights, which `prefer`
	// policies may override per dimension.
	BaseObjectives model.Vector
}

// Result is the outcome of evaluating a Set.
type Result struct {
	Verdict    model.Verdict
	DenyReason string
	// Eligible lists backend IDs that survived every constraint.
	Eligible []string
	// Pruned maps a rejected backend ID to the reason it was rejected.
	Pruned map[string]string
	// Objectives is BaseObjectives after any prefer overrides.
	Objectives model.Vector
	Trace      []model.PolicyHit
}

// Evaluate runs the policy set.
//
// Semantics, in order of precedence:
//
//   - Any evaluation error denies the request. A policy that cannot be
//     evaluated is a policy whose intent is unknown, and under zero trust an
//     unknown intent is not permission.
//   - Any matching deny denies the request, regardless of priority.
//   - At least one matching allow is required. There is no implicit allow.
//   - Every matching constraint prunes the candidate set, ANDed together.
//   - Matching prefer clauses override objective weights per dimension, with
//     later (higher-priority-number) policies winning.
func (s *Set) Evaluate(in Input) Result {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	res := Result{
		Objectives: in.BaseObjectives,
		Pruned:     map[string]string{},
		Trace:      make([]model.PolicyHit, 0, len(s.policies)),
	}

	eligible := make(map[string]bool, len(in.Candidates))
	order := make([]string, 0, len(in.Candidates))
	for _, b := range in.Candidates {
		if !b.Enabled {
			res.Pruned[b.ID] = "backend disabled"
			continue
		}
		eligible[b.ID] = true
		order = append(order, b.ID)
	}

	env := BuildEnv(in.Subject, in.Request, now)

	var (
		denied     bool
		denyReason string
		allowed    bool
		failure    string
	)

	for _, p := range s.policies {
		hit := model.PolicyHit{Policy: p.Name, Effect: string(p.Effect)}

		matched := true
		if p.When != nil {
			v, err := p.When.Eval(env)
			if err != nil {
				hit.Error = err.Error()
				hit.Matched = false
				res.Trace = append(res.Trace, hit)
				if failure == "" {
					failure = fmt.Sprintf("policy %q failed to evaluate: %v", p.Name, err)
				}
				continue
			}
			b, err := v.Truthy()
			if err != nil {
				hit.Error = fmt.Sprintf("'when' clause did not produce a bool: %v", err)
				res.Trace = append(res.Trace, hit)
				if failure == "" {
					failure = fmt.Sprintf("policy %q: %s", p.Name, hit.Error)
				}
				continue
			}
			matched = b
		}
		hit.Matched = matched

		if !matched {
			res.Trace = append(res.Trace, hit)
			continue
		}

		switch p.Effect {
		case EffectDeny:
			denied = true
			if denyReason == "" {
				denyReason = p.reason()
			}
			hit.Detail = p.reason()

		case EffectAllow:
			allowed = true
			hit.Detail = "request authorised"

		case EffectConstrain:
			var removed []string
			for _, id := range order {
				if !eligible[id] {
					continue
				}
				b := findBackend(in.Candidates, id)
				env.SetBackend(b)
				v, err := p.Require.Eval(env)
				if err != nil {
					// A constraint that cannot be evaluated must not silently
					// widen the candidate set. Prune the backend and surface
					// the error.
					eligible[id] = false
					res.Pruned[id] = fmt.Sprintf("constraint %q errored: %v", p.Name, err)
					removed = append(removed, id)
					if hit.Error == "" {
						hit.Error = err.Error()
					}
					continue
				}
				ok, err := v.Truthy()
				if err != nil {
					eligible[id] = false
					res.Pruned[id] = fmt.Sprintf("constraint %q did not produce a bool", p.Name)
					removed = append(removed, id)
					if hit.Error == "" {
						hit.Error = err.Error()
					}
					continue
				}
				if !ok {
					eligible[id] = false
					res.Pruned[id] = p.constraintReason()
					removed = append(removed, id)
				}
			}
			env.SetBackend(nil)
			if len(removed) == 0 {
				hit.Detail = "no candidates pruned"
			} else {
				hit.Detail = fmt.Sprintf("pruned %d candidate(s): %s",
					len(removed), strings.Join(removed, ", "))
			}

		case EffectPrefer:
			for k, v := range p.Prefer {
				if d, ok := model.ParseDimension(k); ok {
					res.Objectives[d] = v
				}
			}
			hit.Detail = "objectives := " + formatWeights(p.Prefer)
		}

		res.Trace = append(res.Trace, hit)
	}

	for _, id := range order {
		if eligible[id] {
			res.Eligible = append(res.Eligible, id)
		}
	}

	switch {
	case failure != "":
		res.Verdict = model.VerdictDeny
		res.DenyReason = failure + " (failing closed)"
	case denied:
		res.Verdict = model.VerdictDeny
		res.DenyReason = denyReason
	case !allowed:
		res.Verdict = model.VerdictDeny
		res.DenyReason = "no policy authorised this request (default deny)"
	case len(res.Eligible) == 0:
		res.Verdict = model.VerdictNoCapacity
		res.DenyReason = "authorised, but policy constraints eliminated every backend"
	default:
		res.Verdict = model.VerdictAllow
	}
	return res
}

func (p *Policy) reason() string {
	if p.Message != "" {
		return p.Message
	}
	return "denied by policy " + p.Name
}

func (p *Policy) constraintReason() string {
	if p.Message != "" {
		return p.Message
	}
	return "failed constraint " + p.Name
}

func findBackend(list []model.Backend, id string) *model.Backend {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func formatWeights(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%.2f", k, m[k]))
	}
	return strings.Join(parts, " ")
}
