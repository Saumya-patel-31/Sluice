// Command sluicectl is the operator CLI for a running Sluice control plane.
//
// It exists mostly for the `explain` subcommand: when a routing decision
// surprises someone, the answer should be reachable from a terminal in one
// command, not only from a dashboard.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

var (
	addr    string
	token   string
	timeout time.Duration
)

func main() {
	flag.StringVar(&addr, "addr", envOr("SLUICE_ADDR", "http://localhost:8080"), "control plane base URL")
	flag.StringVar(&token, "token", envOr("SLUICE_API_TOKEN", ""),
		"bearer token for mutating commands (also $SLUICE_API_TOKEN)")
	flag.DurationVar(&timeout, "timeout", 15*time.Second, "request timeout")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "status":
		err = cmdStatus()
	case "backends":
		err = cmdBackends()
	case "routes":
		err = cmdRoutes()
	case "decisions":
		err = cmdDecisions(args[1:])
	case "explain":
		if len(args) < 2 {
			err = fmt.Errorf("explain needs a decision id")
		} else {
			err = cmdExplain(args[1])
		}
	case "policy":
		err = cmdPolicy(args[1:])
	case "incident":
		err = cmdIncident(args[1:])
	case "watch":
		err = cmdWatch()
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "sluicectl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sluicectl — operate a Sluice control plane

usage: sluicectl [flags] <command> [args]

commands:
  status                     control plane version, policy hash and uptime
  backends                   registered backends with their live signals
  routes                     routes and the traffic distribution in effect
  decisions [--verdict=x]    recent decisions  [--limit=n] [--cloud=x] [--path=x]
  explain <decision-id>      the full derivation of one decision
  policy get                 print the live policy document
  policy apply <file>        compile and install a policy document
  policy test <file>         backtest a document against retained decisions
  incident <backend> <kind>  inject a fault (demo mode only)
                             kinds: brownout outage price_spike carbon_spike
  watch                      tail decisions as they are made

flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, "\nthe control plane URL also comes from $SLUICE_ADDR\n")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

/* ── Transport ─────────────────────────────────────────────────────────── */

func get(path string, out any) error {
	return do(http.MethodGet, path, nil, out)
}

func do(method, path string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, strings.TrimSuffix(addr, "/")+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("%s is not reachable: %w", addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Line    int    `json:"line"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = json.Unmarshal(raw, &e)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && token == "":
			return fmt.Errorf("%s requires a bearer token; set $SLUICE_API_TOKEN or pass --token", path)
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("%s rejected the supplied token", path)
		case e.Line > 0:
			return fmt.Errorf("line %d: %s", e.Line, firstNonEmpty(e.Message, e.Error))
		case e.Error != "":
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func tw() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

/* ── Commands ──────────────────────────────────────────────────────────── */

func cmdStatus() error {
	var s struct {
		Version        string    `json:"version"`
		UptimeSeconds  float64   `json:"uptimeSeconds"`
		PolicyHash     string    `json:"policyHash"`
		PolicyCount    int       `json:"policyCount"`
		PolicyPath     string    `json:"policyPath"`
		PolicyLoaded   time.Time `json:"policyLoadedAt"`
		Generation     uint64    `json:"generation"`
		DemoMode       bool      `json:"demoMode"`
		Backends       int       `json:"backends"`
		Routes         int       `json:"routes"`
		PriceTableAsOf string    `json:"priceTableAsOf"`
		CarbonModel    struct {
			EnergyKWhPerGB float64 `json:"energyKwhPerGb"`
		} `json:"carbonModel"`
	}
	if err := get("/api/status", &s); err != nil {
		return err
	}

	w := tw()
	fmt.Fprintf(w, "version\t%s\n", s.Version)
	fmt.Fprintf(w, "uptime\t%s\n", time.Duration(s.UptimeSeconds*float64(time.Second)).Round(time.Second))
	fmt.Fprintf(w, "policy\t%s (%d rules)\n", s.PolicyHash, s.PolicyCount)
	if s.PolicyPath != "" {
		fmt.Fprintf(w, "policy file\t%s\n", s.PolicyPath)
	}
	fmt.Fprintf(w, "loaded\t%s\n", s.PolicyLoaded.Format(time.RFC3339))
	fmt.Fprintf(w, "plans pushed\t%d\n", s.Generation)
	fmt.Fprintf(w, "backends\t%d\n", s.Backends)
	fmt.Fprintf(w, "routes\t%d\n", s.Routes)
	fmt.Fprintf(w, "demo mode\t%v\n", s.DemoMode)
	fmt.Fprintf(w, "price table\t%s (list prices; live where an API is configured)\n", s.PriceTableAsOf)
	fmt.Fprintf(w, "carbon model\t%.4f kWh/GB network energy\n", s.CarbonModel.EnergyKWhPerGB)
	return w.Flush()
}

type overview struct {
	Backends []struct {
		Backend struct {
			ID           string `json:"id"`
			Cloud        string `json:"cloud"`
			Region       string `json:"region"`
			Jurisdiction string `json:"jurisdiction"`
			Tier         string `json:"tier"`
		} `json:"backend"`
		Egress      struct{ Value float64 } `json:"egress"`
		CarbonPerGB struct{ Value float64 } `json:"carbonPerGb"`
		LatencyP95  struct{ Value float64 } `json:"latencyP95"`
		ErrorRate   struct{ Value float64 } `json:"errorRate"`
		Breaker     struct{ State string }  `json:"breaker"`
		Share       float64                 `json:"share"`
		RPS         float64                 `json:"rps"`
		SpentUSD    float64                 `json:"spentUsd"`
	} `json:"backends"`
	Routes []struct {
		Route struct {
			ID           string  `json:"id"`
			PathPrefix   string  `json:"pathPrefix"`
			LatencySLOMs float64 `json:"latencySloMs"`
		} `json:"route"`
		RPS          float64            `json:"rps"`
		ProjectedP95 float64            `json:"projectedP95Ms"`
		SLOMet       bool               `json:"sloMet"`
		Generation   uint64             `json:"generation"`
		Weights      map[string]float64 `json:"weights"`
	} `json:"routes"`
	KPIs struct {
		DecisionsPerSecond float64 `json:"decisionsPerSecond"`
		SavedUSD           float64 `json:"savedUsd"`
		SavedGrams         float64 `json:"savedGrams"`
		SavingsUSDPerHour  float64 `json:"savingsUsdPerHour"`
		BlendedP95Ms       float64 `json:"blendedP95Ms"`
		AllowRate          float64 `json:"allowRate"`
	} `json:"kpis"`
}

func cmdBackends() error {
	var ov overview
	if err := get("/api/overview?points=8", &ov); err != nil {
		return err
	}
	sort.Slice(ov.Backends, func(i, j int) bool { return ov.Backends[i].Share > ov.Backends[j].Share })

	w := tw()
	fmt.Fprintln(w, "BACKEND\tCLOUD\tJURIS\tTIER\tSHARE\tRPS\t$/GB\tgCO2e/GB\tP95\tERR\tBREAKER\tSPENT")
	for _, b := range ov.Backends {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.1f%%\t%.1f\t%.4f\t%.2f\t%.0fms\t%.2f%%\t%s\t$%.4f\n",
			b.Backend.ID, b.Backend.Cloud, b.Backend.Jurisdiction, b.Backend.Tier,
			b.Share*100, b.RPS, b.Egress.Value, b.CarbonPerGB.Value,
			b.LatencyP95.Value, b.ErrorRate.Value*100, b.Breaker.State, b.SpentUSD)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "TOTAL\t\t\t\t\t%.1f/s\t\t\t%.0fms blended\t\t\t$%.4f saved\n",
		ov.KPIs.DecisionsPerSecond, ov.KPIs.BlendedP95Ms, ov.KPIs.SavedUSD)
	return w.Flush()
}

func cmdRoutes() error {
	var ov overview
	if err := get("/api/overview?points=8", &ov); err != nil {
		return err
	}
	for _, r := range ov.Routes {
		slo := "none"
		if r.Route.LatencySLOMs > 0 {
			slo = fmt.Sprintf("%.0fms", r.Route.LatencySLOMs)
		}
		status := "within SLO"
		if !r.SLOMet {
			status = "BREACHING SLO"
		}
		fmt.Printf("\n%s  (%s)\n  %.1f req/s · projected p95 %.0fms · SLO %s · %s · %d plans pushed\n",
			r.Route.ID, r.Route.PathPrefix, r.RPS, r.ProjectedP95, slo, status, r.Generation)

		type kv struct {
			id string
			w  float64
		}
		var rows []kv
		for id, wt := range r.Weights {
			rows = append(rows, kv{id, wt})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].w > rows[j].w })

		w := tw()
		for _, row := range rows {
			if row.w <= 0 {
				continue
			}
			fmt.Fprintf(w, "    %s\t%s\t%.1f%%\n", row.id, bar(row.w, 28), row.w*100)
		}
		_ = w.Flush()
	}
	return nil
}

func bar(frac float64, width int) string {
	n := int(frac*float64(width) + 0.5)
	if n > width {
		n = width
	}
	return strings.Repeat("█", n) + strings.Repeat("·", width-n)
}

type briefDecision struct {
	ID             string    `json:"id"`
	TS             time.Time `json:"ts"`
	Verdict        string    `json:"verdict"`
	DenyReason     string    `json:"denyReason"`
	Subject        string    `json:"subject"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	ChosenBackend  string    `json:"chosenBackend"`
	SavedUSD       float64   `json:"savedUsd"`
	SavedGrams     float64   `json:"savedGrams"`
	LatencyDeltaMs float64   `json:"latencyDeltaMs"`
	ComputeMicros  int64     `json:"computeMicros"`
}

func cmdDecisions(args []string) error {
	fs := flag.NewFlagSet("decisions", flag.ContinueOnError)
	verdict := fs.String("verdict", "", "allow, deny or no_capacity")
	cloud := fs.String("cloud", "", "aws, gcp or azure")
	path := fs.String("path", "", "substring of the request path")
	limit := fs.Int("limit", 25, "how many to show")
	if err := fs.Parse(args); err != nil {
		return err
	}

	q := fmt.Sprintf("/api/decisions?limit=%d", *limit)
	for k, v := range map[string]string{"verdict": *verdict, "cloud": *cloud, "path": *path} {
		if v != "" {
			q += "&" + k + "=" + v
		}
	}

	var out struct {
		Decisions []briefDecision `json:"decisions"`
	}
	if err := get(q, &out); err != nil {
		return err
	}

	w := tw()
	fmt.Fprintln(w, "TIME\tVERDICT\tSUBJECT\tPATH\tDESTINATION\tSAVED\tΔP95\tµs")
	for _, d := range out.Decisions {
		dest := d.ChosenBackend
		if dest == "" {
			dest = truncate(d.DenyReason, 40)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%+.1f\t%d\n",
			d.TS.Format("15:04:05.000"), d.Verdict, shortSubject(d.Subject),
			truncate(d.Path, 30), dest, money(d.SavedUSD), d.LatencyDeltaMs, d.ComputeMicros)
	}
	return w.Flush()
}

func cmdExplain(id string) error {
	var d struct {
		ID              string             `json:"id"`
		TS              time.Time          `json:"ts"`
		RouteID         string             `json:"routeId"`
		Verdict         string             `json:"verdict"`
		DenyReason      string             `json:"denyReason"`
		ChosenBackend   string             `json:"chosenBackend"`
		BaselineBackend string             `json:"baselineBackend"`
		SavedUSD        float64            `json:"savedUsd"`
		SavedGrams      float64            `json:"savedGrams"`
		LatencyDelta    float64            `json:"latencyDeltaMs"`
		ComputeMicros   int64              `json:"computeMicros"`
		Cached          bool               `json:"cached"`
		Objectives      map[string]float64 `json:"objectives"`
		Subject         struct {
			ID            string `json:"id"`
			TrustDomain   string `json:"trustDomain"`
			MTLS          bool   `json:"mtls"`
			Authenticated bool   `json:"authenticated"`
		} `json:"subject"`
		Request struct {
			Method         string `json:"method"`
			Path           string `json:"path"`
			DataClass      string `json:"dataClass"`
			SourceIP       string `json:"sourceIp"`
			EstimatedBytes int64  `json:"estimatedBytes"`
		} `json:"request"`
		PolicyTrace []struct {
			Policy  string `json:"policy"`
			Effect  string `json:"effect"`
			Matched bool   `json:"matched"`
			Detail  string `json:"detail"`
			Error   string `json:"error"`
		} `json:"policyTrace"`
		Candidates []struct {
			BackendID    string             `json:"backendId"`
			Eligible     bool               `json:"eligible"`
			Reason       string             `json:"reason"`
			Raw          map[string]float64 `json:"raw"`
			Contribution map[string]float64 `json:"contribution"`
			Score        float64            `json:"score"`
			Weight       float64            `json:"weight"`
		} `json:"candidates"`
	}
	if err := get("/api/decisions/"+id, &d); err != nil {
		return err
	}

	fmt.Printf("%s  %s %s\n", d.ID, d.Request.Method, d.Request.Path)
	fmt.Printf("%s · route %s · %dµs", d.TS.Format(time.RFC3339Nano), d.RouteID, d.ComputeMicros)
	if d.Cached {
		fmt.Print(" · policy verdict cached")
	}
	fmt.Printf("\n\nVERDICT: %s", strings.ToUpper(d.Verdict))
	if d.DenyReason != "" {
		fmt.Printf(" — %s", d.DenyReason)
	}
	fmt.Println()

	fmt.Println("\nIDENTITY")
	w := tw()
	fmt.Fprintf(w, "  subject\t%s\n", d.Subject.ID)
	fmt.Fprintf(w, "  trust domain\t%s\n", orDash(d.Subject.TrustDomain))
	fmt.Fprintf(w, "  transport\t%s\n", transport(d.Subject.MTLS, d.Subject.Authenticated))
	fmt.Fprintf(w, "  data class\t%s\n", orDash(d.Request.DataClass))
	fmt.Fprintf(w, "  source\t%s\n", orDash(d.Request.SourceIP))
	_ = w.Flush()

	fmt.Println("\nPOLICY TRACE")
	for _, t := range d.PolicyTrace {
		mark := "·"
		if t.Matched {
			mark = "✓"
		}
		if t.Error != "" {
			mark = "!"
		}
		fmt.Printf("  %s %-40s %-10s", mark, t.Policy, t.Effect)
		switch {
		case t.Error != "":
			fmt.Printf(" ERROR: %s", t.Error)
		case t.Detail != "":
			fmt.Printf(" %s", t.Detail)
		case !t.Matched:
			fmt.Print(" did not match")
		}
		fmt.Println()
	}

	fmt.Printf("\nOBJECTIVES  ")
	keys := make([]string, 0, len(d.Objectives))
	for k := range d.Objectives {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%.0f%%  ", k, d.Objectives[k]*100)
	}
	fmt.Println()

	fmt.Println("\nCANDIDATES  (score = 1 − Σ weighted penalties; higher wins)")
	cw := tw()
	fmt.Fprintln(cw, "  BACKEND\tSCORE\tWEIGHT\t$/GB\tP95\tgCO2e/GB\tERR\tNOTE")
	for _, c := range d.Candidates {
		note := c.Reason
		if c.BackendID == d.ChosenBackend {
			note = "◀ chosen"
		} else if note == "" && !c.Eligible {
			note = "ineligible"
		}
		fmt.Fprintf(cw, "  %s\t%.3f\t%.1f%%\t%.4f\t%.0fms\t%.2f\t%.2f%%\t%s\n",
			c.BackendID, c.Score, c.Weight*100,
			c.Raw["cost"], c.Raw["latency"], c.Raw["carbon"], c.Raw["reliability"]*100, note)
	}
	_ = cw.Flush()

	if d.BaselineBackend != "" {
		fmt.Printf("\nCOUNTERFACTUAL\n")
		fmt.Printf("  a latency-only balancer would have picked %s\n", d.BaselineBackend)
		fmt.Printf("  cost    %s on %s\n", money(d.SavedUSD), byteSize(d.Request.EstimatedBytes))
		fmt.Printf("  carbon  %+.4f g\n", d.SavedGrams)
		fmt.Printf("  latency %+.1f ms p95\n", d.LatencyDelta)
	}
	return nil
}

func cmdPolicy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("policy needs a subcommand: get, apply or test")
	}
	switch args[0] {
	case "get":
		var v struct {
			Source string `json:"source"`
		}
		if err := get("/api/policy", &v); err != nil {
			return err
		}
		fmt.Print(v.Source)
		return nil

	case "apply", "test":
		if len(args) < 2 {
			return fmt.Errorf("%s needs a file", args[0])
		}
		src, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]string{"source": string(src)})

		if args[0] == "apply" {
			var out struct {
				Hash     string `json:"hash"`
				Policies int    `json:"policies"`
			}
			if err := do(http.MethodPut, "/api/policy", strings.NewReader(string(body)), &out); err != nil {
				return err
			}
			fmt.Printf("installed %d policies (%s)\n", out.Policies, out.Hash)
			return nil
		}

		var out struct {
			OK           bool   `json:"ok"`
			Error        string `json:"error"`
			Line         int    `json:"line"`
			Replayed     int    `json:"replayed"`
			Unchanged    int    `json:"unchanged"`
			NewlyDenied  int    `json:"newlyDenied"`
			NewlyAllowed int    `json:"newlyAllowed"`
			NarrowedPool int    `json:"narrowedPool"`
			WidenedPool  int    `json:"widenedPool"`
			Samples      []struct {
				Change      string `json:"change"`
				Path        string `json:"path"`
				Subject     string `json:"subject"`
				EligibleWas int    `json:"eligibleWas"`
				EligibleNow int    `json:"eligibleNow"`
				Reason      string `json:"reason"`
			} `json:"samples"`
		}
		if err := do(http.MethodPost, "/api/policy/backtest?limit=1000",
			strings.NewReader(string(body)), &out); err != nil {
			return err
		}
		if !out.OK {
			return fmt.Errorf("line %d: %s", out.Line, out.Error)
		}
		fmt.Printf("replayed %d retained decisions through both documents\n\n", out.Replayed)
		w := tw()
		fmt.Fprintf(w, "  newly denied\t%d\n", out.NewlyDenied)
		fmt.Fprintf(w, "  newly allowed\t%d\n", out.NewlyAllowed)
		fmt.Fprintf(w, "  pool narrowed\t%d\n", out.NarrowedPool)
		fmt.Fprintf(w, "  pool widened\t%d\n", out.WidenedPool)
		fmt.Fprintf(w, "  unchanged\t%d\n", out.Unchanged)
		_ = w.Flush()

		if len(out.Samples) > 0 {
			fmt.Println("\nchanged decisions:")
			for _, s := range out.Samples {
				fmt.Printf("  %-14s %-34s %s (eligible %d→%d) %s\n",
					s.Change, truncate(s.Path, 34), shortSubject(s.Subject),
					s.EligibleWas, s.EligibleNow, s.Reason)
			}
		}
		// A document that denies traffic which is flowing today is the one
		// case worth an explicit non-zero exit, so this composes with CI.
		if out.NewlyDenied > 0 {
			return fmt.Errorf("%d decisions that are allowed today would be refused", out.NewlyDenied)
		}
		return nil
	}
	return fmt.Errorf("unknown policy subcommand %q", args[0])
}

func cmdIncident(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("incident needs a backend and a kind")
	}
	fs := flag.NewFlagSet("incident", flag.ContinueOnError)
	seconds := fs.Float64("seconds", 90, "how long the fault lasts")
	magnitude := fs.Float64("magnitude", 0, "multiplier, or error rate for an outage (0 picks a default)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	body, _ := json.Marshal(map[string]any{
		"backendId": args[0], "kind": args[1],
		"seconds": *seconds, "magnitude": *magnitude,
		"note": "injected from sluicectl",
	})
	var out struct {
		ID     string    `json:"id"`
		EndsAt time.Time `json:"endsAt"`
	}
	if err := do(http.MethodPost, "/api/incidents", strings.NewReader(string(body)), &out); err != nil {
		return err
	}
	fmt.Printf("%s injected on %s until %s (%s)\n",
		args[1], args[0], out.EndsAt.Format(time.TimeOnly), out.ID)
	return nil
}

// cmdWatch tails the decision stream.
func cmdWatch() error {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimSuffix(addr, "/")+"/api/stream?feedMs=250&points=8", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// No client timeout: this is a long-lived stream by design.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("%s is not reachable: %w", addr, err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "decisions":
			var payload struct {
				Decisions []briefDecision `json:"decisions"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				continue
			}
			for _, d := range payload.Decisions {
				dest := d.ChosenBackend
				if dest == "" {
					dest = truncate(d.DenyReason, 44)
				}
				fmt.Printf("%s  %-11s %-22s %-30s %-22s %8s %+7.1fms\n",
					d.TS.Format("15:04:05.000"), d.Verdict, shortSubject(d.Subject),
					truncate(d.Path, 30), dest, money(d.SavedUSD), d.LatencyDeltaMs)
			}
		}
	}
	return sc.Err()
}

/* ── Formatting ────────────────────────────────────────────────────────── */

func shortSubject(id string) string {
	if id == "" || id == "anonymous" {
		return "anonymous"
	}
	if i := strings.Index(id, "/ns/"); i >= 0 {
		return strings.ReplaceAll(id[i+4:], "/sa/", "/")
	}
	return truncate(id, 22)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func money(v float64) string {
	a := v
	if a < 0 {
		a = -a
	}
	switch {
	case a == 0:
		return "$0"
	case a >= 0.01:
		return fmt.Sprintf("%+.4f", v)
	case a >= 1e-7:
		return fmt.Sprintf("%+.1fµ$", v*1e6)
	}
	return "~$0"
}

func byteSize(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.0f %s", f, units[i])
}

func transport(mtls, authed bool) string {
	switch {
	case mtls:
		return "mutual TLS"
	case authed:
		return "bearer token"
	}
	return "unauthenticated"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
