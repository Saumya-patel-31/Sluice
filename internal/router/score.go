package router

import (
	"math"
	"sort"

	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/signals"
)

// clamp01 confines x to [0,1].
func clamp01(x float64) float64 {
	switch {
	case math.IsNaN(x):
		return 0
	case x < 0:
		return 0
	case x > 1:
		return 1
	}
	return x
}

// normalize min-max scales a dimension across the candidate set so 0 is the
// best observed value and 1 the worst.
//
// Normalising per decision rather than against fixed absolute ranges is what
// makes the objective weights mean something. "Cost matters 40%" is only
// interpretable if cost and latency are on a common scale, and the only scale
// both share is their spread across the candidates actually available right
// now. The cost is that scores are relative: a candidate set where every
// backend is equally slow shows no latency discrimination, which is correct —
// there is no latency decision to make.
func normalize(values []float64) []float64 {
	out := make([]float64, len(values))
	if len(values) == 0 {
		return out
	}
	lo, hi := values[0], values[0]
	for _, v := range values[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	span := hi - lo
	if span <= 1e-12 {
		// Every candidate is identical on this dimension, so it cannot
		// discriminate. Returning zeros rather than 0.5 keeps a degenerate
		// dimension from contributing a constant penalty that would dilute
		// the dimensions that do discriminate.
		return out
	}
	for i, v := range values {
		out[i] = (v - lo) / span
	}
	return out
}

// ScoreCandidates computes the full scoring derivation for a candidate set.
//
// The returned records carry the raw signals, their normalised form, and the
// weighted contribution of each dimension, which together reproduce the
// arithmetic exactly. That is what the dashboard renders when an operator asks
// why a particular region won.
func ScoreCandidates(states []signals.BackendState, objectives model.Vector) []model.CandidateScore {
	n := len(states)
	out := make([]model.CandidateScore, n)
	if n == 0 {
		return out
	}

	w := objectives.Normalized()

	raw := make([][]float64, model.NumDimensions)
	for d := model.Dimension(0); d < model.NumDimensions; d++ {
		col := make([]float64, n)
		for i, s := range states {
			col[i] = s.Vector()[d]
		}
		raw[d] = col
	}

	norm := make([][]float64, model.NumDimensions)
	for d := model.Dimension(0); d < model.NumDimensions; d++ {
		norm[d] = normalize(raw[d])
	}

	for i, s := range states {
		c := model.CandidateScore{
			BackendID: s.Backend.ID,
			Cloud:     s.Backend.Cloud,
			Region:    s.Backend.Region,
			Eligible:  true,
		}
		var penalty float64
		for d := model.Dimension(0); d < model.NumDimensions; d++ {
			c.Raw[d] = raw[d][i]
			c.Normalized[d] = norm[d][i]
			c.Contribution[d] = norm[d][i] * w[d]
			penalty += c.Contribution[d]
		}
		c.Score = clamp01(1 - penalty)
		out[i] = c
	}
	return out
}

// softmaxWeights converts scores into a traffic distribution.
//
// A plain argmax would send every request to the current winner, which is
// wrong for three reasons: it makes the system oscillate when two backends are
// nearly tied, it starves the losers of the traffic needed to keep their
// latency signals honest, and it converts a small measurement error into a
// total traffic shift. A softmax over scores degrades gracefully — near-ties
// split traffic nearly evenly, and a clear winner still takes almost
// everything.
//
// Temperature is the knob: as it approaches zero the distribution approaches
// argmax; as it grows the distribution approaches uniform. Bias enters as a
// multiplicative prior, which is the natural way to express "we have a
// commercial commitment to this provider" without corrupting the measurements.
func softmaxWeights(scores, bias []float64, temperature float64) []float64 {
	n := len(scores)
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	if n == 1 {
		out[0] = 1
		return out
	}
	if temperature <= 1e-9 {
		// Degenerate temperature: winner takes all, ties split evenly.
		best, count := math.Inf(-1), 0
		for i, s := range scores {
			eff := s * biasAt(bias, i)
			if eff > best+1e-12 {
				best, count = eff, 1
			} else if math.Abs(eff-best) <= 1e-12 {
				count++
			}
		}
		for i, s := range scores {
			if math.Abs(s*biasAt(bias, i)-best) <= 1e-12 {
				out[i] = 1 / float64(count)
			}
		}
		return out
	}

	// Subtract the maximum exponent before exponentiating. Scores are bounded
	// in [0,1] so overflow is not reachable today, but temperature is operator
	// input and a very small value makes the exponent arbitrarily large.
	maxZ := math.Inf(-1)
	for _, s := range scores {
		if z := s / temperature; z > maxZ {
			maxZ = z
		}
	}

	var total float64
	for i, s := range scores {
		e := math.Exp(s/temperature-maxZ) * biasAt(bias, i)
		out[i] = e
		total += e
	}
	if total <= 0 {
		for i := range out {
			out[i] = 1 / float64(n)
		}
		return out
	}
	for i := range out {
		out[i] /= total
	}
	return out
}

func biasAt(bias []float64, i int) float64 {
	if i < len(bias) && bias[i] > 0 {
		return bias[i]
	}
	return 1
}

// renormalize scales weights to sum to 1, returning a uniform distribution
// when every weight is zero.
func renormalize(w []float64) {
	var total float64
	for _, x := range w {
		total += x
	}
	if total <= 0 {
		if len(w) == 0 {
			return
		}
		for i := range w {
			w[i] = 1 / float64(len(w))
		}
		return
	}
	for i := range w {
		w[i] /= total
	}
}

// applyCapacityCaps limits each backend's share to what it can actually serve,
// redistributing the overflow across those with headroom.
//
// Without this the router will happily route 90% of traffic to whichever
// region is cheapest, discover it cannot take the load, and then trip its
// breaker — converting a cost optimisation into an outage. Water-filling
// converges quickly because each round either fixes the distribution or caps
// at least one more backend.
func applyCapacityCaps(weights []float64, capacityRPS []float64, totalRPS float64) {
	if totalRPS <= 0 {
		return
	}
	capped := make([]bool, len(weights))
	for round := 0; round < len(weights)+1; round++ {
		var overflow, freeWeight float64
		changed := false

		for i := range weights {
			if capped[i] {
				continue
			}
			cap := capacityRPS[i]
			if cap <= 0 {
				freeWeight += weights[i]
				continue
			}
			maxShare := cap / totalRPS
			if weights[i] > maxShare {
				overflow += weights[i] - maxShare
				weights[i] = maxShare
				capped[i] = true
				changed = true
			} else {
				freeWeight += weights[i]
			}
		}
		if !changed || overflow <= 1e-12 {
			return
		}
		if freeWeight <= 1e-12 {
			// Nowhere to put the overflow: demand exceeds total capacity.
			// Leave the caps in place; the weights will sum to less than 1
			// and the caller renormalises, which distributes the excess
			// proportionally to capacity — the least-bad option.
			renormalize(weights)
			return
		}
		scale := (freeWeight + overflow) / freeWeight
		for i := range weights {
			if !capped[i] {
				weights[i] *= scale
			}
		}
	}
}

// applyExplorationFloor guarantees every eligible backend a minimum share.
//
// A backend at exactly zero weight stops producing live latency and error
// samples, so the only evidence about it comes from synthetic probes, which
// exercise a different code path than real requests do. A small floor keeps
// real signal flowing from every candidate, at a bounded cost.
func applyExplorationFloor(weights []float64, floor float64) {
	if floor <= 0 || len(weights) == 0 {
		return
	}
	if floor*float64(len(weights)) >= 1 {
		// The floor cannot be satisfied for every candidate; fall back to a
		// uniform split rather than producing weights that sum above one.
		for i := range weights {
			weights[i] = 1 / float64(len(weights))
		}
		return
	}
	var deficit, surplus float64
	for _, w := range weights {
		if w < floor {
			deficit += floor - w
		} else {
			surplus += w - floor
		}
	}
	if deficit <= 0 || surplus <= 0 {
		return
	}
	scale := (surplus - deficit) / surplus
	for i := range weights {
		if weights[i] < floor {
			weights[i] = floor
		} else {
			weights[i] = floor + (weights[i]-floor)*scale
		}
	}
	renormalize(weights)
}

// pruneDust zeroes weights below min and redistributes them proportionally.
// Sub-percent weights are noise in a config push and clutter in a dashboard,
// and they never carry enough traffic to produce a usable signal anyway.
func pruneDust(weights []float64, min float64) {
	if min <= 0 {
		return
	}
	var kept float64
	for i, w := range weights {
		if w < min {
			weights[i] = 0
		} else {
			kept += w
		}
	}
	if kept <= 0 {
		// Everything was dust; keep the single largest so traffic still flows.
		best, bi := -1.0, -1
		for i, w := range weights {
			if w > best {
				best, bi = w, i
			}
		}
		if bi >= 0 {
			for i := range weights {
				weights[i] = 0
			}
			weights[bi] = 1
		}
		return
	}
	for i := range weights {
		weights[i] /= kept
	}
}

// l1Distance is the total variation between two weight vectors, used as the
// churn metric that gates republication.
func l1Distance(a, b []float64) float64 {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	var d float64
	for i := 0; i < n; i++ {
		var x, y float64
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		d += math.Abs(x - y)
	}
	return d
}

// sortCandidatesByWeight orders candidates for display: eligible first, then
// by descending weight, then by descending score so ties are stable.
func sortCandidatesByWeight(cs []model.CandidateScore) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Eligible != cs[j].Eligible {
			return cs[i].Eligible
		}
		if math.Abs(cs[i].Weight-cs[j].Weight) > 1e-9 {
			return cs[i].Weight > cs[j].Weight
		}
		if math.Abs(cs[i].Score-cs[j].Score) > 1e-9 {
			return cs[i].Score > cs[j].Score
		}
		return cs[i].BackendID < cs[j].BackendID
	})
}
