package metrics

import (
	"strconv"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	autoscalingv1 "agones.dev/agones/pkg/apis/autoscaling/v1"
	"github.com/prometheus/client_golang/prometheus"

	litev1 "github.com/ponyo877/agones-lite/pkg/apis/lite/v1"
	"github.com/ponyo877/agones-lite/pkg/store"
)

// The gauges, with upstream's names, help texts and label names.
var (
	gameServersCount = prometheus.NewDesc("agones_gameservers_count",
		"The number of gameservers per fleet and status", []string{"type", "fleet_name", "namespace"}, nil)
	fleetReplicas = prometheus.NewDesc("agones_fleets_replicas_count",
		"The number of replicas per fleet", []string{"name", "type", "namespace"}, nil)
	fleetCounters = prometheus.NewDesc("agones_fleet_counters",
		"Aggregated Counters counts and capacity across GameServers in the Fleet", []string{"name", "namespace", "type", "counter"}, nil)
	fleetLists = prometheus.NewDesc("agones_fleet_lists",
		"Aggregated Lists counts and capacity across GameServers in the Fleet", []string{"name", "namespace", "type", "list"}, nil)
	scalerCurrent = prometheus.NewDesc("agones_fleet_autoscalers_current_replicas_count",
		"The current replicas count as seen by autoscalers", []string{"name", "fleet_name", "namespace"}, nil)
	scalerDesired = prometheus.NewDesc("agones_fleet_autoscalers_desired_replicas_count",
		"The desired replicas count as seen by autoscalers", []string{"name", "fleet_name", "namespace"}, nil)
	scalerAble = prometheus.NewDesc("agones_fleet_autoscalers_able_to_scale",
		"The fleet autoscaler can access the fleet to scale", []string{"name", "fleet_name", "namespace"}, nil)
	scalerLimited = prometheus.NewDesc("agones_fleet_autoscalers_limited",
		"The fleet autoscaler is capped", []string{"name", "fleet_name", "namespace"}, nil)
	nodesCount = prometheus.NewDesc("agones_nodes_count",
		"The count of nodes in the cluster", []string{"empty"}, nil)
)

// collector reads the gauges from the store whenever Prometheus scrapes.
// Nothing is kept in between, so a gauge can never drift from the truth.
type collector struct {
	servers store.Typed[*agonesv1.GameServer]
	fleets  store.Typed[*agonesv1.Fleet]
	scalers store.Typed[*autoscalingv1.FleetAutoscaler]
	nodes   store.Typed[*litev1.Node]
}

func newCollector(s *store.Store) *collector {
	return &collector{
		servers: store.GameServersOf(s),
		fleets:  store.FleetsOf(s),
		scalers: store.FleetAutoscalersOf(s),
		nodes:   store.NodesOf(s),
	}
}

// Describe implements prometheus.Collector.
func (c *collector) Describe(out chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		gameServersCount, fleetReplicas, fleetCounters, fleetLists,
		scalerCurrent, scalerDesired, scalerAble, scalerLimited, nodesCount,
	} {
		out <- desc
	}
}

// Collect implements prometheus.Collector.
func (c *collector) Collect(out chan<- prometheus.Metric) {
	servers := c.servers.List("")
	c.gameServers(out, servers)
	c.nodeCounts(out, servers)
	for _, fleet := range c.fleets.List("") {
		c.fleet(out, fleet)
	}
	for _, fas := range c.scalers.List("") {
		c.scaler(out, fas)
	}
}

func gauge(out chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	out <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}

// gameServers reports how many GameServers are in each state, per Fleet.
func (c *collector) gameServers(out chan<- prometheus.Metric, servers []*agonesv1.GameServer) {
	type group struct{ state, fleet, namespace string }
	counts := map[group]float64{}
	for _, gs := range servers {
		counts[group{string(gs.Status.State), fleetOf(gs), gs.Namespace}]++
	}
	for g, count := range counts {
		gauge(out, gameServersCount, count, g.state, g.fleet, g.namespace)
	}
}

// nodeCounts reports how many nodes hold game servers and how many are empty.
func (c *collector) nodeCounts(out chan<- prometheus.Metric, servers []*agonesv1.GameServer) {
	busy := map[string]bool{}
	for _, gs := range servers {
		busy[gs.Status.NodeName] = true
	}
	counts := map[bool]float64{true: 0, false: 0}
	for _, node := range c.nodes.List("") {
		counts[!busy[node.Name]]++
	}
	for empty, count := range counts {
		gauge(out, nodesCount, count, strconv.FormatBool(empty))
	}
}

func (c *collector) fleet(out chan<- prometheus.Metric, fleet *agonesv1.Fleet) {
	replicas := map[string]int32{
		"total":     fleet.Status.Replicas,
		"allocated": fleet.Status.AllocatedReplicas,
		"ready":     fleet.Status.ReadyReplicas,
		"reserved":  fleet.Status.ReservedReplicas,
		"desired":   fleet.Spec.Replicas,
	}
	for kind, count := range replicas {
		gauge(out, fleetReplicas, float64(count), fleet.Name, kind, fleet.Namespace)
	}
	for name, counter := range fleet.Status.Counters {
		aggregate(out, fleetCounters, fleet, name, counter.AllocatedCount, counter.AllocatedCapacity, counter.Count, counter.Capacity)
	}
	for name, list := range fleet.Status.Lists {
		aggregate(out, fleetLists, fleet, name, list.AllocatedCount, list.AllocatedCapacity, list.Count, list.Capacity)
	}
}

// aggregate reports the four numbers upstream gives for a Fleet's Counter or List.
func aggregate(out chan<- prometheus.Metric, desc *prometheus.Desc, fleet *agonesv1.Fleet, name string, allocatedCount, allocatedCapacity, count, capacity int64) {
	values := map[string]int64{
		"allocated_count":    allocatedCount,
		"allocated_capacity": allocatedCapacity,
		"total_count":        count,
		"total_capacity":     capacity,
	}
	for kind, value := range values {
		gauge(out, desc, float64(value), fleet.Name, fleet.Namespace, kind, name)
	}
}

func (c *collector) scaler(out chan<- prometheus.Metric, fas *autoscalingv1.FleetAutoscaler) {
	labels := []string{fas.Name, fas.Spec.FleetName, fas.Namespace}
	gauge(out, scalerCurrent, float64(fas.Status.CurrentReplicas), labels...)
	gauge(out, scalerDesired, float64(fas.Status.DesiredReplicas), labels...)
	gauge(out, scalerAble, truth(fas.Status.AbleToScale), labels...)
	gauge(out, scalerLimited, truth(fas.Status.ScalingLimited), labels...)
}

func truth(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
