package costmodel

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/kubecost/cost-model/pkg/cloud"
	"github.com/kubecost/cost-model/pkg/util"
	prometheus "github.com/prometheus/client_golang/api"
	"k8s.io/klog"
)

const (
	queryClusterCores = `sum(
		avg(avg_over_time(kube_node_status_capacity_cpu_cores[%s] %s)) by (node, cluster_id) * avg(avg_over_time(node_cpu_hourly_cost[%s] %s)) by (node, cluster_id) * 730 +
		avg(avg_over_time(node_gpu_hourly_cost[%s] %s)) by (node, cluster_id) * 730
	  ) by (cluster_id)`

	queryClusterRAM = `sum(
		avg(avg_over_time(kube_node_status_capacity_memory_bytes[%s] %s)) by (node, cluster_id) / 1024 / 1024 / 1024 * avg(avg_over_time(node_ram_hourly_cost[%s] %s)) by (node, cluster_id) * 730
	  ) by (cluster_id)`

	queryStorage = `sum(
		avg(avg_over_time(pv_hourly_cost[%s] %s)) by (persistentvolume, cluster_id) * 730 
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes[%s] %s)) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024
	  ) by (cluster_id) %s`

	queryTotal = `sum(avg(node_total_hourly_cost) by (node, cluster_id)) * 730 +
	  sum(
		avg(avg_over_time(pv_hourly_cost[1h])) by (persistentvolume, cluster_id) * 730 
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes[1h])) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024
	  ) by (cluster_id) %s`
)

// TODO move this to a package-accessible helper
type PromQueryContext struct {
	Client         prometheus.Client
	ErrorCollector *util.ErrorCollector
	WaitGroup      *sync.WaitGroup
}

// TODO move this to a package-accessible helper function once dependencies are able to
// be extricated from costmodel package (PromQueryResult -> util.Vector). Otherwise, circular deps.
func AsyncPromQuery(query string, resultCh chan []*PromQueryResult, ctx PromQueryContext) {
	if ctx.WaitGroup != nil {
		defer ctx.WaitGroup.Done()
	}

	raw, promErr := Query(ctx.Client, query)
	ctx.ErrorCollector.Report(promErr)

	results, parseErr := NewQueryResults(raw)
	ctx.ErrorCollector.Report(parseErr)

	resultCh <- results
}

// Costs represents cumulative and monthly cluster costs over a given duration. Costs
// are broken down by cores, memory, and storage.
type ClusterCosts struct {
	Start             *time.Time             `json:"startTime"`
	End               *time.Time             `json:"endTime"`
	CPUCumulative     float64                `json:"cpuCumulativeCost"`
	CPUMonthly        float64                `json:"cpuMonthlyCost"`
	CPUBreakdown      *ClusterCostsBreakdown `json:"cpuBreakdown"`
	GPUCumulative     float64                `json:"gpuCumulativeCost"`
	GPUMonthly        float64                `json:"gpuMonthlyCost"`
	RAMCumulative     float64                `json:"ramCumulativeCost"`
	RAMMonthly        float64                `json:"ramMonthlyCost"`
	RAMBreakdown      *ClusterCostsBreakdown `json:"ramBreakdown"`
	StorageCumulative float64                `json:"storageCumulativeCost"`
	StorageMonthly    float64                `json:"storageMonthlyCost"`
	StorageBreakdown  *ClusterCostsBreakdown `json:"storageBreakdown"`
	TotalCumulative   float64                `json:"totalCumulativeCost"`
	TotalMonthly      float64                `json:"totalMonthlyCost"`
}

// ClusterCostsBreakdown provides percentage-based breakdown of a resource by
// categories: user for user-space (i.e. non-system) usage, system, and idle.
type ClusterCostsBreakdown struct {
	Idle   float64 `json:"idle"`
	Other  float64 `json:"other"`
	System float64 `json:"system"`
	User   float64 `json:"user"`
}

// NewClusterCostsFromCumulative takes cumulative cost data over a given time range, computes
// the associated monthly rate data, and returns the Costs.
func NewClusterCostsFromCumulative(cpu, gpu, ram, storage float64, window, offset string, dataHours float64) (*ClusterCosts, error) {
	start, end, err := util.ParseTimeRange(window, offset)
	if err != nil {
		return nil, err
	}

	klog.Infof("[Debug] ComputeClusterCosts: dataHours=%f; range dataHours=%f", dataHours, end.Sub(*start).Hours())

	// If the number of hours is not given (i.e. is zero) compute one from the window and offset
	if dataHours == 0 {
		dataHours = end.Sub(*start).Hours()
	}

	// Do not allow zero-length windows to prevent divide-by-zero issues
	if dataHours == 0 {
		return nil, fmt.Errorf("illegal time range: window %s, offset %s", window, offset)
	}

	cc := &ClusterCosts{
		Start:             start,
		End:               end,
		CPUCumulative:     cpu,
		GPUCumulative:     gpu,
		RAMCumulative:     ram,
		StorageCumulative: storage,
		TotalCumulative:   cpu + gpu + ram + storage,
		CPUMonthly:        cpu / dataHours * (util.HoursPerMonth),
		GPUMonthly:        gpu / dataHours * (util.HoursPerMonth),
		RAMMonthly:        ram / dataHours * (util.HoursPerMonth),
		StorageMonthly:    storage / dataHours * (util.HoursPerMonth),
	}
	cc.TotalMonthly = cc.CPUMonthly + cc.GPUMonthly + cc.RAMMonthly + cc.StorageMonthly

	return cc, nil
}

// ComputeClusterCosts gives the cumulative and monthly-rate cluster costs over a window of time for all clusters.
func ComputeClusterCosts(client prometheus.Client, provider cloud.Provider, window, offset string) (map[string]*ClusterCosts, error) {
	// Compute number of minutes in the full interval, for use interpolating missed scrapes or scaling missing data
	start, end, err := util.ParseTimeRange(window, offset)
	if err != nil {
		return nil, err
	}
	mins := end.Sub(*start).Minutes()

	const fmtQueryDataCount = `max(sum(count_over_time(kube_node_status_capacity_cpu_cores[%s:1m]%s)) by (node, cluster_id))`

	const fmtQueryTotalGPU = `sum(
		sum_over_time(node_gpu_hourly_cost[%s:1m]%s) / 60
	) by (cluster_id)`

	const fmtQueryTotalCPU = `sum(
		sum(sum_over_time(kube_node_status_capacity_cpu_cores[%s:1m]%s)) by (node, cluster_id) *
		avg(avg_over_time(node_cpu_hourly_cost[%s:1m]%s)) by (node, cluster_id) / 60
	) by (cluster_id)`

	const fmtQueryTotalRAM = `sum(
		sum(sum_over_time(kube_node_status_capacity_memory_bytes[%s:1m]%s) / 1024 / 1024 / 1024) by (node, cluster_id) *
		avg(avg_over_time(node_ram_hourly_cost[%s:1m]%s)) by (node, cluster_id) / 60
	) by (cluster_id)`

	const fmtQueryTotalStorage = `sum(
		sum(sum_over_time(kube_persistentvolume_capacity_bytes[%s:1m]%s)) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024 *
		avg(avg_over_time(pv_hourly_cost[%s:1m]%s)) by (persistentvolume, cluster_id) / 60
	) by (cluster_id) %s`

	// TODO niko/clustercost account for cluster_id in the queries below

	const fmtQueryCPUModePct = `sum(rate(node_cpu_seconds_total[%s])) by (mode) / scalar(sum(rate(node_cpu_seconds_total[%s])))`

	const fmtQueryRAMSystemPct = `sum(avg_over_time(container_memory_usage_bytes{container_name!="",namespace="kube-system"}[%s]))
	/ sum(avg(kube_node_status_capacity_memory_bytes) by (node))`

	// TODO niko/clustercost how does this represent "other"?
	const fmtQueryRAMOtherPct = `avg_over_time(kubecost_cluster_memory_working_set_bytes[%s])
	/ sum(kube_node_status_capacity_memory_bytes)`

	queryTotalLocalStorage := provider.GetLocalStorageQuery(window, offset, false)
	if queryTotalLocalStorage != "" {
		queryTotalLocalStorage = fmt.Sprintf(" + %s", queryTotalLocalStorage)
	}

	fmtOffset := ""
	if offset != "" {
		fmtOffset = fmt.Sprintf("offset %s", offset)
	}

	queryDataCount := fmt.Sprintf(fmtQueryDataCount, window, fmtOffset)
	queryTotalGPU := fmt.Sprintf(fmtQueryTotalGPU, window, fmtOffset)
	queryTotalCPU := fmt.Sprintf(fmtQueryTotalCPU, window, fmtOffset, window, fmtOffset)
	queryTotalRAM := fmt.Sprintf(fmtQueryTotalRAM, window, fmtOffset, window, fmtOffset)
	queryTotalStorage := fmt.Sprintf(fmtQueryTotalStorage, window, fmtOffset, window, fmtOffset, queryTotalLocalStorage)
	queryCPUModePct := fmt.Sprintf(fmtQueryCPUModePct, window, window)
	queryRAMSystemPct := fmt.Sprintf(fmtQueryRAMSystemPct, window)
	queryRAMOtherPct := fmt.Sprintf(fmtQueryRAMOtherPct, window)

	numQueries := 8

	klog.V(4).Infof("[Debug] queryDataCount: %s", queryDataCount)
	klog.V(4).Infof("[Debug] queryTotalGPU: %s", queryTotalGPU)
	klog.V(4).Infof("[Debug] queryTotalCPU: %s", queryTotalCPU)
	klog.V(4).Infof("[Debug] queryTotalRAM: %s", queryTotalRAM)
	klog.V(4).Infof("[Debug] queryTotalStorage: %s", queryTotalStorage)
	klog.V(4).Infof("[Debug] queryCPUModePct: %s", queryCPUModePct)
	klog.V(4).Infof("[Debug] queryRAMSystemPct: %s", queryRAMSystemPct)
	klog.V(4).Infof("[Debug] queryRAMOtherPct: %s", queryRAMOtherPct)

	// Submit queries to Prometheus asynchronously
	var ec util.ErrorCollector
	var wg sync.WaitGroup
	ctx := PromQueryContext{client, &ec, &wg}
	ctx.WaitGroup.Add(numQueries)

	chDataCount := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryDataCount, chDataCount, ctx)

	chTotalGPU := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryTotalGPU, chTotalGPU, ctx)

	chTotalCPU := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryTotalCPU, chTotalCPU, ctx)

	chTotalRAM := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryTotalRAM, chTotalRAM, ctx)

	chTotalStorage := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryTotalStorage, chTotalStorage, ctx)

	chCPUModePct := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryCPUModePct, chCPUModePct, ctx)

	chRAMSystemPct := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryRAMSystemPct, chRAMSystemPct, ctx)

	chRAMOtherPct := make(chan []*PromQueryResult, 1)
	go AsyncPromQuery(queryRAMOtherPct, chRAMOtherPct, ctx)

	// After queries complete, retrieve results
	wg.Wait()

	resultsDataCount := <-chDataCount
	close(chDataCount)

	resultsTotalGPU := <-chTotalGPU
	close(chTotalGPU)

	resultsTotalCPU := <-chTotalCPU
	close(chTotalCPU)

	resultsTotalRAM := <-chTotalRAM
	close(chTotalRAM)

	resultsTotalStorage := <-chTotalStorage
	close(chTotalStorage)

	resultsCPUModePct := <-chCPUModePct
	close(chCPUModePct)

	resultsRAMSystemPct := <-chRAMSystemPct
	close(chRAMSystemPct)

	// TODO niko/clustercosts
	// resultsRAMOtherPct := <-chRAMOtherPct
	// close(chRAMOtherPct)

	dataMins := mins
	if len(resultsDataCount) > 0 && len(resultsDataCount[0].Values) > 0 {
		dataMins = resultsDataCount[0].Values[0].Value
	} else {
		klog.V(3).Infof("[Warning] cluster cost data count returned no results")
	}

	// Determine combined discount
	discount, customDiscount := 0.0, 0.0
	c, err := A.Cloud.GetConfig()
	if err == nil {
		discount, err = ParsePercentString(c.Discount)
		if err != nil {
			discount = 0.0
		}
		customDiscount, err = ParsePercentString(c.NegotiatedDiscount)
		if err != nil {
			customDiscount = 0.0
		}
	}

	// Intermediate structure storing mapping of [clusterID][type ∈ {cpu, ram, storage, total}]=cost
	costData := make(map[string]map[string]float64)
	defaultClusterID := os.Getenv(clusterIDKey)

	// Helper function to iterate over Prom query results, parsing the raw values into
	// the intermediate costData structure.
	setCostsFromResults := func(costData map[string]map[string]float64, results []*PromQueryResult, name string, discount float64, customDiscount float64) {
		for _, result := range results {
			clusterID, _ := result.GetString("cluster_id")
			if clusterID == "" {
				clusterID = defaultClusterID
			}
			if _, ok := costData[clusterID]; !ok {
				costData[clusterID] = map[string]float64{}
			}
			if len(result.Values) > 0 {
				costData[clusterID][name] += result.Values[0].Value * (1.0 - discount) * (1.0 - customDiscount)
				costData[clusterID]["total"] += result.Values[0].Value * (1.0 - discount) * (1.0 - customDiscount)
			}
		}
	}
	setCostsFromResults(costData, resultsTotalGPU, "gpu", 0.0, customDiscount)
	setCostsFromResults(costData, resultsTotalCPU, "cpu", discount, customDiscount)
	setCostsFromResults(costData, resultsTotalRAM, "ram", discount, customDiscount)
	setCostsFromResults(costData, resultsTotalStorage, "storage", 0.0, customDiscount)

	cpuBreakdownMap := map[string]*ClusterCostsBreakdown{}
	for _, result := range resultsCPUModePct {
		// TODO niko/clustercost account for multi-cluster here
		clusterID, _ := result.GetString("cluster_id")
		if clusterID == "" {
			clusterID = defaultClusterID
		}
		if _, ok := cpuBreakdownMap[clusterID]; !ok {
			cpuBreakdownMap[clusterID] = &ClusterCostsBreakdown{}
		}
		cpuBD := cpuBreakdownMap[clusterID]

		mode, err := result.GetString("mode")
		if err != nil {
			klog.V(3).Infof("[Warning] ComputeClusterCosts: unable to read CPU mode: %s", err)
			mode = "other"
		}

		switch mode {
		case "idle":
			cpuBD.Idle += result.Values[0].Value
		case "system":
			cpuBD.System += result.Values[0].Value
		case "user":
			cpuBD.User += result.Values[0].Value
		default:
			cpuBD.Other += result.Values[0].Value
		}

		klog.Infof("[Debug] cpuBD: %+v", cpuBD)
	}

	ramBreakdownMap := map[string]*ClusterCostsBreakdown{}
	for _, result := range resultsRAMSystemPct {
		// TODO niko/clustercost account for multi-cluster here
		clusterID, _ := result.GetString("cluster_id")
		if clusterID == "" {
			clusterID = defaultClusterID
		}
		if _, ok := ramBreakdownMap[clusterID]; !ok {
			ramBreakdownMap[clusterID] = &ClusterCostsBreakdown{}
		}
		ramBD := ramBreakdownMap[clusterID]

		ramBD.System += result.Values[0].Value

		klog.Infof("[Debug] ramBD: %+v", ramBD)
	}

	// Convert intermediate structure to Costs instances
	costsByCluster := map[string]*ClusterCosts{}
	for id, cd := range costData {
		costs, err := NewClusterCostsFromCumulative(cd["cpu"], cd["gpu"], cd["ram"], cd["storage"], window, offset, dataMins/util.MinsPerHour)
		if err != nil {
			klog.V(3).Infof("[Warning] Failed to parse cluster costs on %s (%s) from cumulative data: %+v", window, offset, cd)
			return nil, err
		}

		klog.Infof("[Debug] clusterID: %s", id)

		if cpuBD, ok := cpuBreakdownMap[id]; ok {
			klog.Infof("[Debug] OK cpuBD: %+v", cpuBD)

			costs.CPUBreakdown = cpuBD
		}
		if ramBD, ok := ramBreakdownMap[id]; ok {
			klog.Infof("[Debug] OK ramBD: %+v", ramBD)

			costs.RAMBreakdown = ramBD
		}

		costsByCluster[id] = costs
	}

	return costsByCluster, nil
}

type Totals struct {
	TotalCost   [][]string `json:"totalcost"`
	CPUCost     [][]string `json:"cpucost"`
	MemCost     [][]string `json:"memcost"`
	StorageCost [][]string `json:"storageCost"`
}

func resultToTotals(qr interface{}) ([][]string, error) {
	results, err := NewQueryResults(qr)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("Not enough data available in the selected time range")
	}

	result := results[0]
	totals := [][]string{}
	for _, value := range result.Values {
		d0 := fmt.Sprintf("%f", value.Timestamp)
		d1 := fmt.Sprintf("%f", value.Value)
		toAppend := []string{
			d0,
			d1,
		}
		totals = append(totals, toAppend)
	}
	return totals, nil
}

func resultToTotal(qr interface{}) (map[string][][]string, error) {
	defaultClusterID := os.Getenv(clusterIDKey)

	results, err := NewQueryResults(qr)
	if err != nil {
		return nil, err
	}

	toReturn := make(map[string][][]string)
	for _, result := range results {
		clusterID, _ := result.GetString("cluster_id")
		if clusterID == "" {
			clusterID = defaultClusterID
		}

		// Expect a single value only
		if len(result.Values) == 0 {
			klog.V(1).Infof("[Warning] Metric values did not contain any valid data.")
			continue
		}

		value := result.Values[0]
		d0 := fmt.Sprintf("%f", value.Timestamp)
		d1 := fmt.Sprintf("%f", value.Value)
		toAppend := []string{
			d0,
			d1,
		}
		if t, ok := toReturn[clusterID]; ok {
			t = append(t, toAppend)
		} else {
			toReturn[clusterID] = [][]string{toAppend}
		}
	}

	return toReturn, nil
}

// ClusterCostsForAllClusters gives the cluster costs averaged over a window of time for all clusters.
func ClusterCostsForAllClusters(cli prometheus.Client, provider cloud.Provider, window, offset string) (map[string]*Totals, error) {
	localStorageQuery := provider.GetLocalStorageQuery(window, offset, true)
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	fmtOffset := ""
	if offset != "" {
		fmtOffset = fmt.Sprintf("offset %s", offset)
	}

	qCores := fmt.Sprintf(queryClusterCores, window, fmtOffset, window, fmtOffset, window, fmtOffset)
	qRAM := fmt.Sprintf(queryClusterRAM, window, fmtOffset, window, fmtOffset)
	qStorage := fmt.Sprintf(queryStorage, window, fmtOffset, window, fmtOffset, localStorageQuery)

	klog.V(4).Infof("Running query %s", qCores)
	resultClusterCores, err := Query(cli, qCores)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qCores, err.Error())
	}

	klog.V(4).Infof("Running query %s", qRAM)
	resultClusterRAM, err := Query(cli, qRAM)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qRAM, err.Error())
	}

	klog.V(4).Infof("Running query %s", qRAM)
	resultStorage, err := Query(cli, qStorage)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qStorage, err.Error())
	}

	toReturn := make(map[string]*Totals)

	coreTotal, err := resultToTotal(resultClusterCores)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qCores, err.Error())
	}
	for clusterID, total := range coreTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].CPUCost = total
	}

	ramTotal, err := resultToTotal(resultClusterRAM)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qRAM, err.Error())
	}
	for clusterID, total := range ramTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].MemCost = total
	}

	storageTotal, err := resultToTotal(resultStorage)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qStorage, err.Error())
	}
	for clusterID, total := range storageTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].StorageCost = total
	}

	return toReturn, nil
}

// AverageClusterTotals gives the current full cluster costs averaged over a window of time.
// Used to be ClutserCosts, but has been deprecated for that use.
func AverageClusterTotals(cli prometheus.Client, provider cloud.Provider, windowString, offset string) (*Totals, error) {
	// turn offsets of the format "[0-9+]h" into the format "offset [0-9+]h" for use in query templatess
	fmtOffset := ""
	if offset != "" {
		fmtOffset = fmt.Sprintf("offset %s", offset)
	}

	localStorageQuery := provider.GetLocalStorageQuery(windowString, offset, true)
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	qCores := fmt.Sprintf(queryClusterCores, windowString, fmtOffset, windowString, fmtOffset, windowString, fmtOffset)
	qRAM := fmt.Sprintf(queryClusterRAM, windowString, fmtOffset, windowString, fmtOffset)
	qStorage := fmt.Sprintf(queryStorage, windowString, fmtOffset, windowString, fmtOffset, localStorageQuery)
	qTotal := fmt.Sprintf(queryTotal, localStorageQuery)

	resultClusterCores, err := Query(cli, qCores)
	if err != nil {
		return nil, err
	}
	resultClusterRAM, err := Query(cli, qRAM)
	if err != nil {
		return nil, err
	}

	resultStorage, err := Query(cli, qStorage)
	if err != nil {
		return nil, err
	}

	resultTotal, err := Query(cli, qTotal)
	if err != nil {
		return nil, err
	}

	coreTotal, err := resultToTotal(resultClusterCores)
	if err != nil {
		return nil, err
	}

	ramTotal, err := resultToTotal(resultClusterRAM)
	if err != nil {
		return nil, err
	}

	storageTotal, err := resultToTotal(resultStorage)
	if err != nil {
		return nil, err
	}

	clusterTotal, err := resultToTotal(resultTotal)
	if err != nil {
		return nil, err
	}

	defaultClusterID := os.Getenv(clusterIDKey)

	return &Totals{
		TotalCost:   clusterTotal[defaultClusterID],
		CPUCost:     coreTotal[defaultClusterID],
		MemCost:     ramTotal[defaultClusterID],
		StorageCost: storageTotal[defaultClusterID],
	}, nil
}

// ClusterCostsOverTime gives the full cluster costs over time
func ClusterCostsOverTime(cli prometheus.Client, provider cloud.Provider, startString, endString, windowString, offset string) (*Totals, error) {
	localStorageQuery := provider.GetLocalStorageQuery(windowString, offset, true)
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	layout := "2006-01-02T15:04:05.000Z"

	start, err := time.Parse(layout, startString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + startString + ". Error: " + err.Error())
		return nil, err
	}
	end, err := time.Parse(layout, endString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + endString + ". Error: " + err.Error())
		return nil, err
	}
	window, err := time.ParseDuration(windowString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + windowString + ". Error: " + err.Error())
		return nil, err
	}

	// turn offsets of the format "[0-9+]h" into the format "offset [0-9+]h" for use in query templatess
	if offset != "" {
		offset = fmt.Sprintf("offset %s", offset)
	}

	qCores := fmt.Sprintf(queryClusterCores, windowString, offset, windowString, offset, windowString, offset)
	qRAM := fmt.Sprintf(queryClusterRAM, windowString, offset, windowString, offset)
	qStorage := fmt.Sprintf(queryStorage, windowString, offset, windowString, offset, localStorageQuery)
	qTotal := fmt.Sprintf(queryTotal, localStorageQuery)

	resultClusterCores, err := QueryRange(cli, qCores, start, end, window)
	if err != nil {
		return nil, err
	}
	resultClusterRAM, err := QueryRange(cli, qRAM, start, end, window)
	if err != nil {
		return nil, err
	}

	resultStorage, err := QueryRange(cli, qStorage, start, end, window)
	if err != nil {
		return nil, err
	}

	resultTotal, err := QueryRange(cli, qTotal, start, end, window)
	if err != nil {
		return nil, err
	}

	coreTotal, err := resultToTotals(resultClusterCores)
	if err != nil {
		return nil, err
	}

	ramTotal, err := resultToTotals(resultClusterRAM)
	if err != nil {
		return nil, err
	}

	storageTotal, err := resultToTotals(resultStorage)
	if err != nil {
		return nil, err
	}

	clusterTotal, err := resultToTotals(resultTotal)
	if err != nil {
		return nil, err
	}

	return &Totals{
		TotalCost:   clusterTotal,
		CPUCost:     coreTotal,
		MemCost:     ramTotal,
		StorageCost: storageTotal,
	}, nil

}
