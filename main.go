package main

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/o11ydev/oy-toolkit/util/client"
	"github.com/o11ydev/oy-toolkit/util/cmd"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	//go:embed alerts.html.tmpl
	tmpl       string
	outputFile = kingpin.Flag(
		"output.file",
		"HTML output file",
	).Default("alerts.html").String()
	refreshInterval = kingpin.Flag(
		"refresh.interval",
		"Regenerate the file and reload the page at this interval (e.g. 30s). 0 generates once",
	).Default("0s").Duration()
	mode string

	// "generate" (the default) writes the file; "serve" also hosts it.
	_             = kingpin.Command("generate", "Generate the HTML file (default).").Default().Action(setMode("generate"))
	serveCmd      = kingpin.Command("serve", "Regenerate the HTML file periodically and serve it over HTTP.").Action(setMode("serve"))
	listenAddress = serveCmd.Flag(
		"web.listen-address",
		"Address to listen on for the serve command",
	).Default(":8080").String()
	groupByFlag = kingpin.Flag(
		"group.by",
		"Comma-separated list of labels to group hexagons by, outermost group first",
	).Default("job").String()
)

type Node struct {
	Job      string
	Instance string
}

// Entity is one hexagon: one alert rule evaluated on one instance.
type Entity struct {
	Name   string            `json:"name"`  // instance
	Alert  string            `json:"alert"` // alert rule name
	State  string            `json:"state"` // firing, pending or present (metrics exist, alert not active)
	Labels map[string]string `json:"labels"`
}

// report is the data handed to the HTML template.
type report struct {
	GroupBy  []string `json:"groupBy"`
	Entities []Entity `json:"entities"`
	// RefreshSeconds is how often the page reloads itself; 0 disables it.
	RefreshSeconds int    `json:"refreshSeconds"`
	Generated      string `json:"generated"`
}

const defaultServeInterval = 30 * time.Second

func main() {
	c := client.InitCliFlags()
	logger := cmd.InitCmd("oy-alerts-dashboard")

	promClient, err := client.NewClient(c)
	if err != nil {
		level.Error(logger).Log("msg", "Can't create Prometheus client", "err", err)
		os.Exit(1)
	}

	if mode == "serve" {
		if err := serve(logger, promClient); err != nil {
			level.Error(logger).Log("msg", "Can't serve", "err", err)
			os.Exit(1)
		}
		return
	}

	if *refreshInterval <= 0 {
		if err := generate(logger, promClient, 0, true); err != nil {
			level.Error(logger).Log("msg", "Can't generate report", "err", err)
			os.Exit(1)
		}
		return
	}

	// Keep regenerating the file; the page reloads itself at the same interval.
	for {
		if err := generate(logger, promClient, *refreshInterval, true); err != nil {
			level.Error(logger).Log("msg", "Can't generate report", "err", err)
		}
		time.Sleep(*refreshInterval)
	}
}

// serve regenerates the HTML file periodically and hosts it over HTTP.
func serve(logger log.Logger, promClient api.Client) error {
	interval := *refreshInterval
	if interval <= 0 {
		interval = defaultServeInterval
	}

	refresh := func() {
		if err := generate(logger, promClient, interval, false); err != nil {
			level.Error(logger).Log("msg", "Can't generate report", "err", err)
			return
		}
		level.Debug(logger).Log("msg", "Report regenerated", "file", *outputFile)
	}
	refresh()
	go func() {
		for range time.Tick(interval) {
			refresh()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if _, err := os.Stat(*outputFile); err != nil {
			http.Error(w, "report not generated yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, *outputFile)
	})

	level.Info(logger).Log("msg", "Serving dashboard", "address", *listenAddress, "file", *outputFile, "interval", interval)
	return http.ListenAndServe(*listenAddress, mux)
}

// generate fetches the data and writes the HTML file. When showProgress is
// false (server mode) no progress bars are drawn.
func generate(logger log.Logger, promClient api.Client, interval time.Duration, showProgress bool) error {
	pw := progress.NewWriter()
	pw.ShowETA(false)
	pw.ShowValue(false)
	pw.ShowPercentage(false)
	pw.ShowTime(false)
	if showProgress {
		go pw.Render()
	}

	entities, err := fetchData(logger, pw, promClient)
	if showProgress {
		pw.Stop()
		for pw.IsRenderInProgress() {
			time.Sleep(time.Millisecond * 100)
		}
	}
	if err != nil {
		return err
	}

	var groupBy []string
	for _, l := range strings.Split(*groupByFlag, ",") {
		if l = strings.TrimSpace(l); l != "" {
			groupBy = append(groupBy, l)
		}
	}

	return writeReport(*outputFile, report{
		GroupBy:        groupBy,
		Entities:       entities,
		RefreshSeconds: int(interval.Seconds()),
		Generated:      time.Now().Format(time.RFC1123),
	})
}

func setMode(m string) func(*kingpin.ParseContext) error {
	return func(*kingpin.ParseContext) error {
		mode = m
		return nil
	}
}

func nodeOf(l model.LabelSet) Node {
	return Node{Job: string(l["job"]), Instance: string(l["instance"])}
}

func labelsOf(sets ...model.LabelSet) map[string]string {
	m := make(map[string]string)
	for _, l := range sets {
		for k, v := range l {
			m[string(k)] = string(v)
		}
	}
	return m
}

func fetchData(logger log.Logger, pw progress.Writer, client api.Client) ([]Entity, error) {
	t1 := &progress.Tracker{Message: "Fetching instances"}
	t2 := &progress.Tracker{Message: "Fetching alerts"}
	pw.AppendTracker(t1)
	pw.AppendTracker(t2)

	v1api := v1.NewAPI(client)

	// Target labels per job/instance, so that any of them can be used for grouping.
	targets := make(map[Node]model.LabelSet)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tgs, err := v1api.Targets(ctx)
		cancel()
		if err != nil {
			t1.MarkAsErrored()
			return nil, fmt.Errorf("querying targets: %w", err)
		}
		for _, t := range tgs.Active {
			targets[nodeOf(t.Labels)] = t.Labels
		}
		t1.MarkAsDone()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	rules, err := v1api.Rules(ctx)
	cancel()
	if err != nil {
		t2.MarkAsErrored()
		return nil, fmt.Errorf("querying rules: %w", err)
	}
	t2.MarkAsDone()

	var todo []v1.AlertingRule
	for _, g := range rules.Groups {
		for _, rule := range g.Rules {
			if v, ok := rule.(v1.AlertingRule); ok {
				todo = append(todo, v)
			}
		}
	}
	t3 := &progress.Tracker{
		Message: "Analyzing alerts",
		Total:   int64(len(todo)),
	}
	pw.AppendTracker(t3)

	var out []Entity
	for _, r := range todo {
		// Nodes on which this rule is pending or firing.
		active := make(map[Node]bool)
		for _, a := range r.Alerts {
			n := nodeOf(a.Labels)
			active[n] = true
			l := labelsOf(targets[n], a.Labels)
			l["alertname"] = r.Name
			out = append(out, Entity{Name: n.Instance, Alert: r.Name, State: string(a.State), Labels: l})
		}

		selectors, err := analyzeMetricsFromQuery(r.Query)
		if err != nil {
			level.Warn(logger).Log("msg", "Can't analyze rule", "rule", r.Name, "err", err)
			t3.IncrementWithError(1)
			continue
		}
		if len(selectors) == 0 {
			t3.Increment(1)
			continue
		}

		parts := make([]string, 0, len(selectors))
		for _, v := range selectors {
			parts = append(parts, "group by (instance,job) ("+v+")")
		}
		query := strings.Join(parts, " and ")

		qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, _, err := v1api.Query(qctx, query, time.Now())
		qcancel()
		if err != nil {
			level.Warn(logger).Log("msg", "Query failed", "rule", r.Name, "err", err)
			t3.IncrementWithError(1)
			continue
		}
		vec, ok := res.(model.Vector)
		if !ok {
			level.Warn(logger).Log("msg", "Unexpected result type", "rule", r.Name, "type", res.Type().String())
			t3.IncrementWithError(1)
			continue
		}
		// Metrics exist on these nodes but the alert isn't active: report as OK.
		for _, m := range vec {
			l := model.LabelSet(m.Metric)
			n := nodeOf(l)
			if active[n] {
				continue
			}
			lbl := labelsOf(targets[n], l)
			lbl["alertname"] = r.Name
			out = append(out, Entity{Name: n.Instance, Alert: r.Name, State: "present", Labels: lbl})
		}
		t3.Increment(1)
	}

	if out == nil {
		out = []Entity{}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Alert != out[j].Alert {
			return out[i].Alert < out[j].Alert
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func writeReport(path string, data report) error {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return err
	}
	// Write to a temporary file and rename, so a concurrent reader never sees a partial file.
	f, err := os.CreateTemp(filepath.Dir(path), ".alerts-*.html")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := t.Execute(f, data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func analyzeMetricsFromQuery(query string) ([]string, error) {
	expr, err := parser.ParseExpr(query)
	if err != nil {
		return nil, err
	}
	return extractMetricFromExpr(expr)
}

func extractMetricFromExpr(exp parser.Expr) ([]string, error) {
	switch x := exp.(type) {
	case *parser.BinaryExpr:
		l, err := extractMetricFromExpr(x.LHS)
		if err != nil {
			return nil, err
		}
		r, err := extractMetricFromExpr(x.RHS)
		if err != nil {
			return nil, err
		}
		return append(l, r...), nil
	case *parser.AggregateExpr:
		return extractMetricFromExpr(x.Expr)
	case *parser.SubqueryExpr:
		return extractMetricFromExpr(x.Expr)
	case *parser.ParenExpr:
		return extractMetricFromExpr(x.Expr)
	case *parser.NumberLiteral, *parser.StringLiteral:
		return nil, nil
	case *parser.StepInvariantExpr:
		return extractMetricFromExpr(x.Expr)
	case *parser.UnaryExpr:
		return extractMetricFromExpr(x.Expr)
	case *parser.VectorSelector:
		return []string{x.String()}, nil
	case *parser.MatrixSelector:
		return []string{x.VectorSelector.String()}, nil
	case *parser.Call:
		m := make([]string, 0)
		for _, a := range x.Args {
			r, err := extractMetricFromExpr(a)
			if err != nil {
				return nil, err
			}
			m = append(m, r...)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("unknown expr: %v", x)
	}
}
