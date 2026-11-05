package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// External series the shipped monitoring may reference without the controller
// exporting them: Prometheus' own scrape health and the blackbox exporter.
var externalMetrics = map[string]bool{"up": true, "probe_success": true}

var promqlWords = map[string]bool{
	"and": true, "or": true, "unless": true, "by": true, "without": true,
	"on": true, "ignoring": true, "group_left": true, "group_right": true,
	"bool": true, "offset": true, "le": true,
}

var (
	reQuoted    = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	reMatchers  = regexp.MustCompile(`\{[^}]*\}`)
	reRange     = regexp.MustCompile(`\[[^\]]*\]`)
	reGrouping  = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	reIdent     = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*(\s*\()?`)
	reExprLine  = regexp.MustCompile(`(?m)^\s*expr:\s*(.+)$`)
	reDescName  = regexp.MustCompile(`fqName: "([^"]+)"`)
	reDurations = regexp.MustCompile(`\b\d+(ms|s|m|h|d|w|y)\b`)
)

// metricNames returns the metric names a PromQL expression selects.
func metricNames(expr string) []string {
	e := reQuoted.ReplaceAllString(expr, `""`)
	e = reMatchers.ReplaceAllString(e, " ")
	e = reRange.ReplaceAllString(e, " ")
	e = reGrouping.ReplaceAllString(e, " ")
	e = reDurations.ReplaceAllString(e, " ")
	var out []string
	for _, m := range reIdent.FindAllString(e, -1) {
		if strings.HasSuffix(strings.TrimSpace(m), "(") { // function call
			continue
		}
		if promqlWords[m] {
			continue
		}
		out = append(out, m)
	}
	return out
}

func exportedNames(t *testing.T) map[string]bool {
	t.Helper()
	ch := make(chan *prometheus.Desc, 256)
	go func() {
		for _, c := range appCollectors {
			c.Describe(ch)
		}
		close(ch)
	}()
	names := map[string]bool{}
	for d := range ch {
		m := reDescName.FindStringSubmatch(d.String())
		if m == nil {
			t.Fatalf("can't read metric name from %s", d.String())
		}
		names[m[1]] = true
	}
	return names
}

func exists(name string, exported map[string]bool) bool {
	if exported[name] || externalMetrics[name] {
		return true
	}
	for _, suf := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suf); ok && exported[base] {
			return true
		}
	}
	return false
}

func dashboardExprs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var exprs []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok && k == "expr" {
					exprs = append(exprs, s)
					continue
				}
				walk(val)
			}
		case []any:
			for _, val := range x {
				walk(val)
			}
		}
	}
	walk(doc)
	return exprs
}

// Every metric the shipped alert rules and Grafana dashboards query must be one
// the controller exports. The PENDING-10 live test found two alerts and twelve
// dashboard queries pointing at names that didn't exist.
func TestMonitoringReferencesExportedMetrics(t *testing.T) {
	exported := exportedNames(t)
	root := filepath.Join("..", "..", "..", "deploy", "monitoring")

	exprs := map[string][]string{}
	alerts, err := os.ReadFile(filepath.Join(root, "alerts.yaml"))
	if err != nil {
		t.Fatalf("read alerts: %v", err)
	}
	for _, m := range reExprLine.FindAllStringSubmatch(string(alerts), -1) {
		exprs["alerts.yaml"] = append(exprs["alerts.yaml"], m[1])
	}
	if len(exprs["alerts.yaml"]) == 0 {
		t.Fatal("no expr lines found in alerts.yaml")
	}
	dashboards, _ := filepath.Glob(filepath.Join(root, "dashboards", "*.json"))
	if len(dashboards) == 0 {
		t.Fatal("no dashboards found")
	}
	for _, d := range dashboards {
		exprs[filepath.Base(d)] = dashboardExprs(t, d)
	}

	var missing []string
	for file, list := range exprs {
		for _, e := range list {
			names := metricNames(e)
			if len(names) == 0 {
				missing = append(missing, file+": no metric found in "+e)
			}
			for _, n := range names {
				if !exists(n, exported) {
					missing = append(missing, file+": "+n+" (in "+e+")")
				}
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("monitoring references metrics the controller doesn't export:\n  %s", strings.Join(missing, "\n  "))
	}
}

func TestMetricNamesParser(t *testing.T) {
	cases := map[string][]string{
		`db_pool_connections{state="acquired"} >= ignoring(state) db_pool_connections{state="max"}`:   {"db_pool_connections", "db_pool_connections"},
		`histogram_quantile(0.95, sum(rate(acl_compile_duration_seconds_bucket[5m])) by (le)) > 0.25`: {"acl_compile_duration_seconds_bucket"},
		`outbox_events_pending > 500 or outbox_oldest_pending_age_seconds > 300`:                      {"outbox_events_pending", "outbox_oldest_pending_age_seconds"},
		`sum(increase(pki_rotations_total[1h])) by (entity, status)`:                                  {"pki_rotations_total"},
		`cert_runway_seconds / cert_lifetime_seconds < 0.25`:                                          {"cert_runway_seconds", "cert_lifetime_seconds"},
	}
	for expr, want := range cases {
		got := metricNames(expr)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("metricNames(%q) = %v, want %v", expr, got, want)
		}
	}
}

var (
	reDescLabels  = regexp.MustCompile(`variableLabels: \{([^}]*)\}`)
	reAlertLabel  = regexp.MustCompile(`\$labels\.([a-zA-Z_][a-zA-Z0-9_]*)`)
	reLegendLabel = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
)

// Labels used in alert annotations and dashboard legends must exist on some
// exported metric. The live test found annotations reading $labels.cert_type
// (the label is "entity"), which rendered as "Certificate ”".
func TestMonitoringReferencesExportedLabels(t *testing.T) {
	labels := map[string]bool{"job": true, "instance": true, "le": true}
	ch := make(chan *prometheus.Desc, 256)
	go func() {
		for _, c := range appCollectors {
			c.Describe(ch)
		}
		close(ch)
	}()
	for d := range ch {
		if m := reDescLabels.FindStringSubmatch(d.String()); m != nil {
			for _, l := range strings.Split(m[1], ",") {
				if l = strings.TrimSpace(l); l != "" {
					labels[l] = true
				}
			}
		}
	}
	if !labels["entity"] {
		t.Fatalf("label parser found no 'entity' label; Desc format changed? %v", labels)
	}

	root := filepath.Join("..", "..", "..", "deploy", "monitoring")
	var bad []string
	alerts, _ := os.ReadFile(filepath.Join(root, "alerts.yaml"))
	for _, m := range reAlertLabel.FindAllStringSubmatch(string(alerts), -1) {
		if !labels[m[1]] {
			bad = append(bad, "alerts.yaml: $labels."+m[1])
		}
	}
	dashboards, _ := filepath.Glob(filepath.Join(root, "dashboards", "*.json"))
	for _, d := range dashboards {
		raw, _ := os.ReadFile(d)
		for _, m := range reLegendLabel.FindAllStringSubmatch(string(raw), -1) {
			if !labels[m[1]] {
				bad = append(bad, filepath.Base(d)+": {{"+m[1]+"}}")
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("monitoring uses labels no exported metric has:\n  %s", strings.Join(bad, "\n  "))
	}
}
