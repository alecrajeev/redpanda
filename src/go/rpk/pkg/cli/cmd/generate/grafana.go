// Copyright 2020 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/redpanda-data/redpanda/src/go/rpk/pkg/cli"
	"github.com/redpanda-data/redpanda/src/go/rpk/pkg/cli/cmd/generate/graf"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	datasource   string
	jobName      string
	hClient      = http.Client{Timeout: 10 * time.Second}
	metricGroups = []string{
		"errors",
		"storage",
		"reactor",
		"scheduler",
		"io_queue",
		"vectorized_internal_rpc",
		"kafka_rpc",
		"rpc_client",
		"memory",
		"raft",
	}
)

const panelHeight = 6

type RowSet struct {
	rowTitles   []string
	groupPanels map[string]*graf.RowPanel
}

func newRowSet() *RowSet {
	return &RowSet{
		rowTitles:   []string{},
		groupPanels: map[string]*graf.RowPanel{},
	}
}

func newGrafanaDashboardCmd() *cobra.Command {
	var metricsEndpoint string
	command := &cobra.Command{
		Use:   "grafana-dashboard",
		Short: "Generate a Grafana dashboard for redpanda metrics",
		RunE: func(ccmd *cobra.Command, args []string) error {
			if !(strings.HasPrefix(metricsEndpoint, "http://") ||
				strings.HasPrefix(metricsEndpoint, "https://")) {
				metricsEndpoint = fmt.Sprintf("http://%s", metricsEndpoint)
			}
			return executeGrafanaDashboard(metricsEndpoint)
		},
	}
	metricsEndpointFlag := "metrics-endpoint"
	deprecatedPrometheusURLFlag := "prometheus-url"

	for _, flag := range []string{metricsEndpointFlag, deprecatedPrometheusURLFlag} {
		command.Flags().StringVar(
			&metricsEndpoint,
			flag,
			"http://localhost:9644/metrics",
			"The redpanda metrics endpoint where to get the metrics metadata. i.e. redpanda_host:9644/metrics")
	}

	command.Flags().MarkDeprecated(deprecatedPrometheusURLFlag, fmt.Sprintf("Deprecated flag. Use --%v instead", metricsEndpointFlag))

	datasourceFlag := "datasource"
	command.Flags().StringVar(
		&datasource,
		datasourceFlag,
		"",
		"The name of the Prometheus datasource as configured in your grafana instance.")
	command.Flags().StringVar(
		&jobName,
		"job-name",
		"redpanda",
		"The prometheus job name by which to identify the redpanda nodes")
	command.MarkFlagRequired(datasourceFlag)
	return command
}

func executeGrafanaDashboard(metricsEndpoint string) error {
	metricFamilies, err := fetchMetrics(metricsEndpoint)
	if err != nil {
		return err
	}
	metricsURL, err := url.Parse(metricsEndpoint)
	if err != nil {
		return err
	}
	isPublicMetrics := metricsURL.EscapedPath() == "/public_metrics"
	dashboard := buildGrafanaDashboard(metricFamilies, isPublicMetrics)
	jsonSpec, err := json.MarshalIndent(dashboard, "", " ")
	if err != nil {
		return err
	}
	log.SetFormatter(cli.NewNoopFormatter())
	// The logger's default stream is stderr, which prevents piping to files
	// from working without redirecting them with '2>&1'.
	if log.StandardLogger().Out == os.Stderr {
		log.SetOutput(os.Stdout)
	}
	log.Info(string(jsonSpec))
	return nil
}

func buildGrafanaDashboard(
	metricFamilies map[string]*dto.MetricFamily,
	isPublicMetrics bool,
) graf.Dashboard {
	intervals := []string{"5s", "10s", "30s", "1m", "5m", "15m", "30m", "1h", "2h", "1d"}
	timeOptions := []string{"5m", "15m", "1h", "6h", "12h", "24h", "2d", "7d", "30d"}
	var summaryPanels []graf.Panel
	if isPublicMetrics {
		summaryPanels = buildPublicMetricsSummary(metricFamilies)
	} else {
		summaryPanels = buildSummary(metricFamilies)
	}
	lastY := summaryPanels[len(summaryPanels)-1].GetGridPos().Y + panelHeight
	rowSet := newRowSet()
	rowSet.processRows(metricFamilies, isPublicMetrics)
	rowSet.addCachePerformancePanels(metricFamilies)
	rows := rowSet.finalize(lastY)
	return graf.Dashboard{
		Title:      "Redpanda",
		Templating: buildTemplating(),
		Panels: append(
			summaryPanels,
			rows...,
		),
		Editable: true,
		Refresh:  "10s",
		Time:     graf.Time{From: "now-1h", To: "now"},
		TimePicker: graf.TimePicker{
			RefreshIntervals: intervals,
			TimeOptions:      timeOptions,
		},
		Timezone:      "utc",
		SchemaVersion: 12,
	}
}

func (rowSet *RowSet) finalize(fromY int) []graf.Panel {
	panelWidth := 8

	sort.Strings(rowSet.rowTitles)
	rows := []graf.Panel{}

	y := fromY
	for _, title := range rowSet.rowTitles {
		row := rowSet.groupPanels[title]
		row.GetGridPos().Y = y
		for i, panel := range row.Panels {
			panel.GetGridPos().Y = y
			panel.GetGridPos().X = (i * panelWidth) % 24
		}
		rows = append(rows, row)
		y++
	}

	return rows
}

func (rowSet *RowSet) processRows(metricFamilies map[string]*dto.MetricFamily, isPublicMetrics bool) {
	names := []string{}
	for k := range metricFamilies {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		family := metricFamilies[name]
		var panel graf.Panel
		// hack around redpanda_storage_* metrics: these should be gauge
		// panels but the metrics type come as COUNTER
		if family.GetType() == dto.MetricType_COUNTER && !strings.HasPrefix(name, "redpanda_storage") {
			panel = newCounterPanel(family, isPublicMetrics)
		} else if subtype(family) == "histogram" {
			panel = newPercentilePanel(family, 0.95, isPublicMetrics)
		} else {
			panel = newGaugePanel(family, isPublicMetrics)
		}

		if panel == nil {
			continue
		}

		group := metricGroup(name)
		row, ok := rowSet.groupPanels[group]
		if ok {
			row.Panels = append(row.Panels, panel)
			rowSet.groupPanels[group] = row
		} else {
			rowSet.rowTitles = append(rowSet.rowTitles, group)
			rowSet.groupPanels[group] = graf.NewRowPanel(group, panel)
		}
	}
}

func (rowSet *RowSet) addRatioPanel(
	metricFamilies map[string]*dto.MetricFamily, m0, m1, group, help string,
) {
	a := metricFamilies[m0]
	b := metricFamilies[m1]
	row := rowSet.groupPanels[group]
	panel := makeRatioPanel(a, b, help)
	row.Panels = append(row.Panels, panel)
	rowSet.groupPanels[group] = row
}

func (rowSet *RowSet) addCachePerformancePanels(
	metricFamilies map[string]*dto.MetricFamily,
) {
	// are we generating for a broker that has these stats?
	if _, ok := metricFamilies["vectorized_storage_log_cached_batches_read"]; !ok {
		return
	}

	rowSet.addRatioPanel(
		metricFamilies,
		"vectorized_storage_log_cached_batches_read",
		"vectorized_storage_log_batches_read",
		"storage",
		"Batch cache hit ratio - batches")

	rowSet.addRatioPanel(
		metricFamilies,
		"vectorized_storage_log_cached_read_bytes",
		"vectorized_storage_log_read_bytes",
		"storage",
		"Batch cache hit ratio - bytes")
}

func buildTemplating() graf.Templating {
	node := newDefaultTemplateVar("node", "Node", true)
	node.IncludeAll = true
	node.AllValue = ".*"
	node.Type = "query"
	node.Query = "label_values(instance)"
	shard := newDefaultTemplateVar("node_shard", "Shard", true)
	shard.IncludeAll = true
	shard.AllValue = ".*"
	shard.Type = "query"
	shard.Query = "label_values(shard)"
	clusterOpt := graf.Option{
		Text:     "Cluster",
		Value:    "",
		Selected: false,
	}
	aggregateOpts := []graf.Option{
		clusterOpt,
		{
			Text:     "Instance",
			Value:    "instance,",
			Selected: false,
		},
		{
			Text:     "Instance, Shard",
			Value:    "instance,shard,",
			Selected: false,
		},
	}
	aggregate := newDefaultTemplateVar(
		"aggr_criteria",
		"Aggregate by",
		false,
		aggregateOpts...,
	)
	aggregate.Type = "custom"
	aggregate.Query = "Cluster : cluster,Instance : instance,Instance\\,Shard : instance\\,shard"
	aggregate.Current = graf.Current{
		Text:  clusterOpt.Text,
		Value: clusterOpt.Value,
	}
	return graf.Templating{
		List: []graf.TemplateVar{node, shard, aggregate},
	}
}

// buildSummary builds the Summary section of the Redpanda generated grafana
// dashboard that uses the /metric endpoint.
func buildSummary(metricFamilies map[string]*dto.MetricFamily) []graf.Panel {
	maxWidth := 24
	singleStatW := 2
	percentiles := []float32{0.95, 0.99}
	percentilesNo := len(percentiles)
	panels := []graf.Panel{}
	y := 0

	summaryText := htmlHeader("Redpanda Summary")
	summaryTitle := graf.NewTextPanel(summaryText, "html")
	summaryTitle.GridPos = graf.GridPos{H: 2, W: maxWidth, X: 0, Y: y}
	summaryTitle.Transparent = true
	panels = append(panels, summaryTitle)
	y += summaryTitle.GridPos.H

	nodesUp := graf.NewSingleStatPanel("Nodes Up")
	nodesUp.Datasource = datasource
	nodesUp.GridPos = graf.GridPos{H: 6, W: singleStatW, X: 0, Y: y}
	nodesUp.Targets = []graf.Target{{
		Expr:           `count by (app) (vectorized_application_uptime)`,
		Step:           40,
		IntervalFactor: 1,
		LegendFormat:   "Nodes Up",
	}}
	nodesUp.Transparent = true
	panels = append(panels, nodesUp)
	y += nodesUp.GridPos.H

	partitionCount := graf.NewSingleStatPanel("Partitions")
	partitionCount.Datasource = datasource
	partitionCount.GridPos = graf.GridPos{
		H: 6,
		W: singleStatW,
		X: nodesUp.GridPos.W,
		Y: y,
	}
	partitionCount.Targets = []graf.Target{{
		Expr:         `count(count by (topic,partition) (vectorized_storage_log_partition_size{namespace="kafka"}))`,
		LegendFormat: "Partition count",
	}}
	partitionCount.Transparent = true
	panels = append(panels, partitionCount)
	y += partitionCount.GridPos.H

	kafkaFamily, kafkaExists := metricFamilies["vectorized_kafka_rpc_dispatch_handler_latency"]
	if kafkaExists {
		width := (maxWidth - (singleStatW * 2)) / percentilesNo
		for i, p := range percentiles {
			panel := newPercentilePanel(kafkaFamily, p, false)
			panel.GridPos = graf.GridPos{
				H: panelHeight,
				W: width,
				X: i*width + (singleStatW * 2),
				Y: y,
			}
			panels = append(panels, panel)
		}
		y += panelHeight
	}
	width := maxWidth / 4
	rpcLatencyText := htmlHeader("Internal RPC Latency")
	rpcLatencyTitle := graf.NewTextPanel(rpcLatencyText, "html")
	rpcLatencyTitle.GridPos = graf.GridPos{H: 2, W: maxWidth / 2, X: 0, Y: y}
	rpcLatencyTitle.Transparent = true
	rpcFamily, rpcExists := metricFamilies["vectorized_internal_rpc_dispatch_handler_latency"]
	if rpcExists {
		y += rpcLatencyTitle.GridPos.H
		panels = append(panels, rpcLatencyTitle)
		for i, p := range percentiles {
			panel := newPercentilePanel(rpcFamily, p, false)
			panel.GridPos = graf.GridPos{
				H: panelHeight,
				W: width,
				X: i * width,
				Y: y,
			}
			panels = append(panels, panel)
		}
	}

	throughputText := htmlHeader("Throughput")
	throughputTitle := graf.NewTextPanel(throughputText, "html")
	throughputTitle.GridPos = graf.GridPos{
		H: 2,
		W: maxWidth / 2,
		X: rpcLatencyTitle.GridPos.W,
		Y: rpcLatencyTitle.GridPos.Y,
	}
	throughputTitle.Transparent = true
	panels = append(panels, throughputTitle)

	readBytesFamily, readBytesExist := metricFamilies["vectorized_storage_log_read_bytes"]
	writtenBytesFamily, writtenBytesExist := metricFamilies["vectorized_storage_log_written_bytes"]
	if readBytesExist && writtenBytesExist {
		readPanel := newCounterPanel(readBytesFamily, false)
		readPanel.GridPos = graf.GridPos{
			H: panelHeight,
			W: width,
			X: maxWidth / 2,
			Y: y,
		}
		panels = append(panels, readPanel)

		writtenPanel := newCounterPanel(writtenBytesFamily, false)
		writtenPanel.GridPos = graf.GridPos{
			H: panelHeight,
			W: width,
			X: readPanel.GridPos.X + readPanel.GridPos.W,
			Y: y,
		}
		panels = append(panels, writtenPanel)
	}

	return panels
}

// buildPublicMetricsSummary builds the Summary section of the Redpanda
// generated grafana dashboard that uses the /public_metrics endpoint.
func buildPublicMetricsSummary(metricFamilies map[string]*dto.MetricFamily) []graf.Panel {
	maxWidth := 24
	singleStatW := 2
	percentiles := []float32{0.95, 0.99}
	percentilesNo := len(percentiles)
	panels := []graf.Panel{}
	y := 0

	summaryText := htmlHeader("Redpanda Summary")
	summaryTitle := graf.NewTextPanel(summaryText, "html")
	summaryTitle.GridPos = graf.GridPos{H: 2, W: maxWidth, X: 0, Y: y}
	summaryTitle.Transparent = true
	panels = append(panels, summaryTitle)
	y += summaryTitle.GridPos.H

	// Nodes Up Panel
	nodesUp := graf.NewSingleStatPanel("Nodes Up")
	nodesUp.Datasource = datasource
	nodesUp.GridPos = graf.GridPos{H: 6, W: singleStatW, X: 0, Y: y}
	nodesUp.Targets = []graf.Target{{
		Expr:           `redpanda_cluster_brokers`,
		Step:           40,
		IntervalFactor: 1,
		LegendFormat:   "Nodes Up",
		Instant:        true,
	}}
	nodesUp.Transparent = true
	panels = append(panels, nodesUp)
	y += nodesUp.GridPos.H

	// Partitions Panel
	partitionCount := graf.NewSingleStatPanel("Partitions")
	partitionCount.Datasource = datasource
	partitionCount.GridPos = graf.GridPos{
		H: 6,
		W: singleStatW,
		X: 0,
		Y: nodesUp.GridPos.H,
	}
	partitionCount.Targets = []graf.Target{{
		Expr:         "redpanda_cluster_partitions",
		LegendFormat: "Partition count",
		Instant:      true,
	}}
	partitionCount.Transparent = true
	panels = append(panels, partitionCount)
	y += partitionCount.GridPos.H

	// Latency of Kafka consume/produce requests (p95 - p99)
	_, kafkaExists := metricFamilies[`redpanda_kafka_request_latency_seconds`]
	if kafkaExists {
		width := (maxWidth - singleStatW) / percentilesNo
		for i, p := range percentiles {
			pTarget := graf.Target{
				Expr:           fmt.Sprintf(`histogram_quantile(%.2f, sum(rate(redpanda_kafka_request_latency_seconds_bucket{instance=~"$node", redpanda_request="produce"}[$__rate_interval])) by (le, provider, region, instance, namespace, pod))`, p),
				LegendFormat:   "node: {{instance}}",
				Format:         "time_series",
				Step:           10,
				IntervalFactor: 2,
				RefID:          "A",
			}
			pTitle := fmt.Sprintf("Latency of Kafka produce requests (p%.0f) per broker", p*100)
			producePanel := newGraphPanel(pTitle, pTarget, "s")
			producePanel.Interval = "1m"
			producePanel.Lines = true
			producePanel.SteppedLine = true
			producePanel.NullPointMode = "null as zero"
			producePanel.Tooltip.ValueType = "individual"
			producePanel.Tooltip.Sort = 0
			producePanel.GridPos = graf.GridPos{
				H: panelHeight,
				W: width,
				X: i*width + singleStatW,
				Y: y,
			}
			cTarget := graf.Target{
				Expr:           fmt.Sprintf(`histogram_quantile(%.2f, sum(rate(redpanda_kafka_request_latency_seconds_bucket{instance=~"$node", redpanda_request="consume"}[$__rate_interval])) by (le, provider, region, instance, namespace, pod))`, p),
				LegendFormat:   "node: {{instance}}",
				Format:         "time_series",
				Step:           10,
				IntervalFactor: 2,
				RefID:          "A",
			}
			cTitle := fmt.Sprintf("Latency of Kafka consume requests (p%.0f) per broker", p*100)
			consumePanel := newGraphPanel(cTitle, cTarget, "s")
			consumePanel.Interval = "1m"
			consumePanel.Lines = true
			consumePanel.SteppedLine = true
			consumePanel.NullPointMode = "null as zero"
			consumePanel.Tooltip.ValueType = "individual"
			consumePanel.Tooltip.Sort = 0
			consumePanel.GridPos = graf.GridPos{
				H: panelHeight,
				W: width,
				X: i*width + singleStatW,
				Y: producePanel.GridPos.H,
			}
			panels = append(panels, consumePanel, producePanel)
		}
		y += panelHeight
	}
	width := maxWidth / 4

	// Internal RPC Latency Section
	rpcLatencyText := htmlHeader("Internal RPC Latency")
	rpcLatencyTitle := graf.NewTextPanel(rpcLatencyText, "html")
	rpcLatencyTitle.GridPos = graf.GridPos{H: 2, W: maxWidth / 2, X: 0, Y: y}
	rpcLatencyTitle.Transparent = true
	rpcFamily, rpcExists := metricFamilies[`redpanda_rpc_request_latency_seconds`]
	if rpcExists {
		y += rpcLatencyTitle.GridPos.H
		panels = append(panels, rpcLatencyTitle)
		for i, p := range percentiles {
			template := `histogram_quantile(%.2f, sum(rate(%s_bucket{instance=~"$node",redpanda_server="internal"}[$__rate_interval])) by (le, $aggr_criteria))`
			expr := fmt.Sprintf(template, p, rpcFamily.GetName())
			target := graf.Target{
				Expr:           expr,
				LegendFormat:   "node: {{instance}}",
				Format:         "time_series",
				Step:           10,
				IntervalFactor: 2,
				RefID:          "A",
			}
			title := fmt.Sprintf("%s (p%.0f)", rpcFamily.GetHelp(), p*100)
			panel := newGraphPanel(title, target, "s")
			panel.Interval = "1m"
			panel.Lines = true
			panel.SteppedLine = true
			panel.NullPointMode = "null as zero"
			panel.Tooltip.ValueType = "individual"
			panel.Tooltip.Sort = 0
			panel.GridPos = graf.GridPos{
				H: panelHeight,
				W: width,
				X: i * width,
				Y: y,
			}
			panels = append(panels, panel)
		}
	}

	// Throughput section
	throughputText := htmlHeader("Throughput")
	throughputTitle := graf.NewTextPanel(throughputText, "html")
	throughputTitle.GridPos = graf.GridPos{
		H: 2,
		W: maxWidth / 2,
		X: rpcLatencyTitle.GridPos.W,
		Y: rpcLatencyTitle.GridPos.Y,
	}
	throughputTitle.Transparent = true
	panels = append(panels, throughputTitle)

	reqBytesFamily, reqBytesExists := metricFamilies["redpanda_kafka_request_bytes_total"]
	if reqBytesExists {
		target := graf.Target{
			Expr:           `sum(rate(redpanda_kafka_request_bytes_total[$__rate_interval])) by (redpanda_request)`,
			LegendFormat:   "redpanda_request: {{redpanda_request}}",
			Format:         "time_series",
			Step:           10,
			IntervalFactor: 2,
		}
		panel := newGraphPanel("Rate - "+reqBytesFamily.GetHelp(), target, "Bps")
		panel.Interval = "1m"
		panel.Lines = true
		panel.GridPos = graf.GridPos{
			H: panelHeight,
			W: width * 2,
			X: maxWidth / 2,
			Y: y,
		}
		panel.Title = "Throughput of Kafka produce/consume requests for the cluster"
		panels = append(panels, panel)
	}

	return panels
}

func metricGroup(metric string) string {
	for _, group := range metricGroups {
		if strings.Contains(metric, group) {
			return group
		}
	}
	return "others"
}

func fetchMetrics(
	metricsEndpoint string,
) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		metricsEndpoint,
		nil,
	)
	if err != nil {
		return nil, err
	}
	res, err := hClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, fmt.Errorf(
			"the request to %s failed. Status: %d",
			metricsEndpoint,
			res.StatusCode,
		)
	}
	bs, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	parser := &expfmt.TextParser{}
	return parser.TextToMetricFamilies(bytes.NewBuffer(bs))
}

func newPercentilePanel(
	m *dto.MetricFamily, percentile float32, isPublicMetrics bool,
) *graf.GraphPanel {
	template := `histogram_quantile(%.2f, sum(rate(%s_bucket{instance=~"$node",shard=~"$node_shard"}[2m])) by (le, $aggr_criteria))`
	if isPublicMetrics {
		template = `histogram_quantile(%.2f, sum(rate(%s_bucket{instance=~"$node"}[$__rate_interval])) by (le, $aggr_criteria))`
	}
	target := graf.Target{
		Expr:           fmt.Sprintf(template, percentile, m.GetName()),
		LegendFormat:   legendFormat(m),
		Format:         "time_series",
		Step:           10,
		IntervalFactor: 2,
		RefID:          "A",
	}
	title := fmt.Sprintf("%s (p%.0f)", m.GetHelp(), percentile*100)
	panel := newGraphPanel(title, target, "µs")
	panel.Lines = true
	panel.SteppedLine = true
	panel.NullPointMode = "null as zero"
	panel.Tooltip.ValueType = "individual"
	panel.Tooltip.Sort = 0
	panel.Interval = "1m"
	return panel
}

func newCounterPanel(m *dto.MetricFamily, isPublicMetrics bool) *graf.GraphPanel {
	template := `sum(irate(%s{instance=~"$node",shard=~"$node_shard"}[2m])) by ($aggr_criteria)`
	if isPublicMetrics {
		template = `sum(rate(%s{instance=~"$node"}[$__rate_interval])) by ($aggr_criteria)`
	}
	target := graf.Target{
		Expr:           fmt.Sprintf(template, m.GetName()),
		LegendFormat:   legendFormat(m),
		Format:         "time_series",
		Step:           10,
		IntervalFactor: 2,
	}
	format := "ops"
	if strings.Contains(m.GetName(), "bytes") {
		format = "Bps"
	} else if strings.Contains(m.GetName(), "redpanda_scheduler") {
		format = "percentunit"
	}
	panel := newGraphPanel("Rate - "+m.GetHelp(), target, format)
	panel.Lines = true
	panel.Interval = "1m"
	return panel
}

func newGaugePanel(m *dto.MetricFamily, isPublicMetrics bool) *graf.GraphPanel {
	template := `sum(%s{instance=~"$node",shard=~"$node_shard"}) by ($aggr_criteria)`
	if isPublicMetrics {
		template = `sum(%s{instance=~"$node"}) by ($aggr_criteria)`
	}
	target := graf.Target{
		Expr:           fmt.Sprintf(template, m.GetName()),
		LegendFormat:   legendFormat(m),
		Format:         "time_series",
		Step:           10,
		IntervalFactor: 2,
	}
	format := "short"
	if strings.Contains(subtype(m), "bytes") {
		format = "bytes"
	}
	panel := newGraphPanel(m.GetHelp(), target, format)
	panel.Lines = true
	panel.SteppedLine = true
	return panel
}

func makeRatioPanel(m0, m1 *dto.MetricFamily, help string) *graf.GraphPanel {
	expr := fmt.Sprintf(
		`sum(irate(%s{instance=~"$node",shard=~"$node_shard"}[2m])) by ($aggr_criteria) / sum(irate(%s{instance=~"$node",shard=~"$node_shard"}[2m])) by ($aggr_criteria)`,
		m0.GetName(), m1.GetName())
	target := graf.Target{
		Expr:           expr,
		LegendFormat:   legendFormat(m0),
		Format:         "time_series",
		Step:           10,
		IntervalFactor: 2,
	}
	format := "short"
	if strings.Contains(subtype(m0), "bytes") {
		format = "bytes"
	}
	panel := newGraphPanel(help, target, format)
	panel.Lines = true
	panel.SteppedLine = true
	return panel
}

func newGraphPanel(
	title string, target graf.Target, yAxisFormat string,
) *graf.GraphPanel {
	// yAxisMin := 0.0
	p := graf.NewGraphPanel(title, yAxisFormat)
	p.Datasource = datasource
	p.Targets = []graf.Target{target}
	p.Tooltip = graf.Tooltip{
		MsResolution: true,
		Shared:       true,
		ValueType:    "cumulative",
	}
	return p
}

func newDefaultTemplateVar(
	name, label string, multi bool, opts ...graf.Option,
) graf.TemplateVar {
	if opts == nil {
		opts = []graf.Option{}
	}
	return graf.TemplateVar{
		Name:       name,
		Datasource: datasource,
		Label:      label,
		Multi:      multi,
		Refresh:    1,
		Sort:       1,
		Options:    opts,
	}
}

func legendFormat(m *dto.MetricFamily) string {
	duplicate := func(s string, ls []string) bool {
		for _, l := range ls {
			if s == l {
				return true
			}
		}
		return false
	}
	labels := []string{}
	legend := "node: {{instance}}"
	for _, metric := range m.GetMetric() {
		for _, label := range metric.GetLabel() {
			name := label.GetName()
			if name != "type" && !duplicate(name, labels) {
				legend += fmt.Sprintf(
					", %s: {{%s}}",
					name,
					name,
				)
				labels = append(labels, name)
			}
		}
	}
	return legend
}

func subtype(m *dto.MetricFamily) string {
	for _, metric := range m.GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "type" {
				return label.GetValue()
			}
		}
	}
	return "none"
}

func htmlHeader(str string) string {
	return fmt.Sprintf(
		"<h1 style=\"color:#87CEEB; border-bottom: 3px solid #87CEEB;\">%s</h1>",
		str,
	)
}
