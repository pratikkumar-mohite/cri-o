package statsserver

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/cri-o/cri-o/internal/config/cgmgr"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	types "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// updateSandbox updates the StatsServer's entry for this sandbox, as well as each child container.
// It first populates the stats from the CgroupParent, then calculates network usage, updates
// each of its children container stats by calling into the runtime, and finally calculates the CPUNanoCores.
func (ss *StatsServer) updateSandbox(sb *sandbox.Sandbox) *types.PodSandboxStats {
	if sb == nil {
		return nil
	}

	// Sandbox metrics are to fullfil the CRI metrics endpoint
	sandboxMetrics, exists := ss.sboxMetrics[sb.ID()]
	if !exists {
		sandboxMetrics = NewSandboxMetrics(sb)
	}
	sandboxMetrics.ResetMetricsForSandbox()
	sandboxMetrics.ResetContainerMetricsForSandbox()

	// Sandbox stats are to fullfil the  Kubelet's /stats/summary endpoint
	sandboxStats := &types.PodSandboxStats{
		Attributes: &types.PodSandboxAttributes{
			Id:          sb.ID(),
			Labels:      sb.Labels(),
			Metadata:    sb.Metadata(),
			Annotations: sb.Annotations(),
		},
		Linux: &types.LinuxPodSandboxStats{},
	}

	// network metrics are collected at pod level only
	if slices.Contains(ss.Config().IncludedPodMetrics, "network") {
		podMetrics := ss.GenerateNetworkMetrics(sb, sandboxMetrics)
		sandboxMetrics.AddMetricToSandboxMetrics("", podMetrics)
	}

	if cgstats, err := ss.Config().CgroupManager().SandboxCgroupStats(sb.CgroupParent(), sb.ID()); err != nil {
		logrus.Errorf("Error getting sandbox stats %s: %v", sb.ID(), err)
	} else {
		sandboxStats.Linux.Cpu = criCPUStats(cgstats.CPU, cgstats.SystemNano)
		sandboxStats.Linux.Memory = criMemStats(cgstats.Memory, cgstats.SystemNano)
		sandboxStats.Linux.Process = criProcessStats(cgstats.Pid, cgstats.SystemNano)
	}

	if err := ss.populateNetworkUsage(sandboxStats, sb); err != nil {
		logrus.Errorf("Error adding network stats for sandbox %s: %v", sb.ID(), err)
	}

	containersList := sb.Containers().List()
	containerStats := make([]*types.ContainerStats, 0, len(containersList))

	for _, c := range containersList {
		if c.StateNoLock().Status == oci.ContainerStateStopped {
			continue
		}
		cgstats, err := ss.Runtime().ContainerStats(context.TODO(), c, sb.CgroupParent())
		if err != nil {
			logrus.Errorf("Error getting container stats %s: %v", c.ID(), err)
			continue
		}
		// convert cgroups stats to CRI stats
		cStats := containerCRIStats(cgstats, c, cgstats.SystemNano)
		ss.populateWritableLayer(cStats, c)
		if oldcStats, ok := ss.ctrStats[c.ID()]; ok {
			updateUsageNanoCores(oldcStats.Cpu, cStats.Cpu)
		}
		containerStats = append(containerStats, cStats)
		// convert cgroups stats to CRI metrics
		containerMetrics := ss.containerMetricsFromCgStats(sb, c, cgstats, sandboxMetrics)
		sandboxMetrics.AddMetricToSandboxMetrics(c.ID(), containerMetrics)
	}

	sandboxStats.Linux.Containers = containerStats
	if old, ok := ss.sboxStats[sb.ID()]; ok {
		updateUsageNanoCores(old.Linux.Cpu, sandboxStats.Linux.Cpu)
	}

	ss.sboxStats[sb.ID()] = sandboxStats
	ss.sboxMetrics[sb.ID()] = sandboxMetrics

	return sandboxStats
}

// updateContainerStats calls into the runtime handler to update the container stats,
// as well as populates the writable layer by calling into the container storage.
// If this container already existed in the stats server, the CPU nano cores are calculated as well.
func (ss *StatsServer) updateContainerStats(c *oci.Container, sb *sandbox.Sandbox) *types.ContainerStats {
	if c == nil || sb == nil {
		return nil
	}
	if c.StateNoLock().Status == oci.ContainerStateStopped {
		return nil
	}
	cgstats, err := ss.Runtime().ContainerStats(context.TODO(), c, sb.CgroupParent())
	if err != nil {
		logrus.Errorf("Error getting container stats %s: %v", c.ID(), err)
		return nil
	}
	cStats := containerCRIStats(cgstats, c, cgstats.SystemNano)
	ss.populateWritableLayer(cStats, c)
	if oldcStats, ok := ss.ctrStats[c.ID()]; ok {
		updateUsageNanoCores(oldcStats.Cpu, cStats.Cpu)
	}
	ss.ctrStats[c.ID()] = cStats
	return cStats
}

// populateNetworkUsage gathers information about the network from within the sandbox's network namespace.
func (ss *StatsServer) populateNetworkUsage(stats *types.PodSandboxStats, sb *sandbox.Sandbox) error {
	return ns.WithNetNSPath(sb.NetNsPath(), func(_ ns.NetNS) error {
		links, err := netlink.LinkList()
		if err != nil {
			logrus.Errorf("Unable to retrieve network namespace links: %v", err)
			return err
		}
		stats.Linux.Network = &types.NetworkUsage{
			Interfaces: make([]*types.NetworkInterfaceUsage, 0, len(links)-1),
		}
		for i := range links {
			iface, err := linkToInterface(links[i])
			if err != nil {
				logrus.Errorf("Failed to %v for pod %s", err, sb.ID())
				continue
			}
			// TODO FIXME or DefaultInterfaceName?
			if i == 0 {
				stats.Linux.Network.DefaultInterface = iface
			} else {
				stats.Linux.Network.Interfaces = append(stats.Linux.Network.Interfaces, iface)
			}
		}
		return nil
	})
}

// metricsForPodSandbox is an internal, non-locking version of MetricsForPodSandbox
// that returns (and occasionally gathers) the metrics for the given sandbox.
// Note: caller must hold the lock on the StatsServer
func (ss *StatsServer) metricsForPodSandbox(sb *sandbox.Sandbox) *SandboxMetrics {
	if ss.collectionPeriod == 0 {
		return ss.updatePodSandboxMetrics(sb)
	}
	sboxMetrics, ok := ss.sboxMetrics[sb.ID()]
	if ok {
		return sboxMetrics
	}
	// Cache miss, try again
	return ss.updatePodSandboxMetrics(sb)
}

// updatePodSandboxMetrics updates the sandbox metrics for the given sandbox.
// If the sandbox is not found, it creates a new entry in the map.
// Note: caller must hold the lock on the StatsServer.
func (ss *StatsServer) updatePodSandboxMetrics(sb *sandbox.Sandbox) *SandboxMetrics {
	if sb == nil {
		return nil
	}
	sm, exists := ss.sboxMetrics[sb.ID()]
	if !exists {
		sm = NewSandboxMetrics(sb)
	}
	// Reset metrics for the next iteration
	sm.ResetMetricsForSandbox()
	sm.ResetContainerMetricsForSandbox() // Reset container metrics as well

	// network metrics are collected at pod level
	if slices.Contains(ss.Config().IncludedPodMetrics, "network") {
		podMetrics := ss.GenerateNetworkMetrics(sb, sm)
		sm.AddMetricToSandboxMetrics("", podMetrics)
	}

	for _, c := range sb.Containers().List() {
		// Skip if the container is stopped
		if c.StateNoLock().Status == oci.ContainerStateStopped {
			continue
		}
		containerMetrics := ss.GenerateSandboxContainerMetrics(sb, c, sm)
		sm.AddMetricToSandboxMetrics(c.ID(), containerMetrics)
	}

	ss.sboxMetrics[sb.ID()] = sm // Update the entry in the map
	return sm
}

// GenerateSandboxContainerMetrics generates a list of metrics for the specified sandbox
// containers by collecting metrics from the cgroup based on the includedMetrics
// except for network metrics which is collected at pod level.
func (ss *StatsServer) GenerateSandboxContainerMetrics(sb *sandbox.Sandbox, c *oci.Container, sm *SandboxMetrics) []*types.Metric {
	cgstats, err := ss.Runtime().ContainerStats(context.TODO(), c, sb.CgroupParent())
	if err != nil || cgstats == nil {
		logrus.Errorf("Error getting sandbox stats %s: %v", sb.ID(), err)
		return nil
	}
	return ss.containerMetricsFromCgStats(sb, c, cgstats, sm)
}

func (ss *StatsServer) containerMetricsFromCgStats(sb *sandbox.Sandbox, c *oci.Container, cgstats *cgmgr.CgroupStats, sandboxMetrics *SandboxMetrics) []*types.Metric {
	var metrics []*types.Metric
	for _, m := range ss.Config().IncludedPodMetrics {
		switch m {
		case "cpu":
			cpuMetrics := GenerateSandboxCPUMetrics(sb, c, cgstats.CPU, sandboxMetrics)
			metrics = append(metrics, cpuMetrics...)
		case "memory":
			memoryMetrics := GenerateSandboxMemoryMetrics(sb, c, cgstats.Memory, sandboxMetrics)
			metrics = append(metrics, memoryMetrics...)
		case "oom":
			cm, err := ss.Config().CgroupManager().ContainerCgroupManager(sb.CgroupParent(), c.ID())
			if err != nil {
				logrus.Errorf("Unable to fetch cgroup manager %s: %v", sb.ID(), err)
				continue
			}
			oomCount, err := cm.OOMKillCount()
			if err != nil {
				logrus.Errorf("Unable to fetch oom kill count %s: %v", sb.ID(), err)
				continue
			}
			oomMetrics := GenerateSandboxOOMMetrics(sb, c, oomCount, sandboxMetrics)
			metrics = append(metrics, oomMetrics...)
		case "network":
			continue // network metrics are collected at pod level only
		default:
			logrus.Warnf("Unknown or misspelled metric: %s", m)
		}
	}
	return metrics
}

// linkToInterface translates information found from the netlink package
// into CRI the NetworkInterfaceUsage structure.
func linkToInterface(link netlink.Link) (*types.NetworkInterfaceUsage, error) {
	attrs := link.Attrs()
	if attrs == nil {
		return nil, errors.New("get stats for iface")
	}
	if attrs.Statistics == nil {
		return nil, fmt.Errorf("get stats for iface %s", attrs.Name)
	}
	return &types.NetworkInterfaceUsage{
		Name:     attrs.Name,
		RxBytes:  &types.UInt64Value{Value: attrs.Statistics.RxBytes},
		RxErrors: &types.UInt64Value{Value: attrs.Statistics.RxErrors},
		TxBytes:  &types.UInt64Value{Value: attrs.Statistics.TxBytes},
		TxErrors: &types.UInt64Value{Value: attrs.Statistics.TxErrors},
	}, nil
}

func containerCRIStats(stats *cgmgr.CgroupStats, ctr *oci.Container, systemNano int64) *types.ContainerStats {
	criStats := &types.ContainerStats{
		Attributes: ctr.CRIAttributes(),
	}
	criStats.Cpu = criCPUStats(stats.CPU, systemNano)
	criStats.Memory = criMemStats(stats.Memory, systemNano)
	criStats.Swap = criSwapStats(stats.Memory, systemNano)
	return criStats
}

func criCPUStats(cpuStats *cgmgr.CPUStats, systemNano int64) *types.CpuUsage {
	return &types.CpuUsage{
		Timestamp:            systemNano,
		UsageCoreNanoSeconds: &types.UInt64Value{Value: cpuStats.TotalUsageNano},
	}
}

func criMemStats(memStats *cgmgr.MemoryStats, systemNano int64) *types.MemoryUsage {
	return &types.MemoryUsage{
		Timestamp:       systemNano,
		WorkingSetBytes: &types.UInt64Value{Value: memStats.WorkingSetBytes},
		RssBytes:        &types.UInt64Value{Value: memStats.RssBytes},
		PageFaults:      &types.UInt64Value{Value: memStats.PageFaults},
		MajorPageFaults: &types.UInt64Value{Value: memStats.MajorPageFaults},
		UsageBytes:      &types.UInt64Value{Value: memStats.Usage},
		AvailableBytes:  &types.UInt64Value{Value: memStats.AvailableBytes},
	}
}

func criSwapStats(memStats *cgmgr.MemoryStats, systemNano int64) *types.SwapUsage {
	return &types.SwapUsage{
		Timestamp:          systemNano,
		SwapUsageBytes:     &types.UInt64Value{Value: memStats.SwapUsage},
		SwapAvailableBytes: &types.UInt64Value{Value: memStats.SwapLimit - memStats.SwapUsage},
	}
}

func criProcessStats(pStats *cgmgr.PidsStats, systemNano int64) *types.ProcessUsage {
	return &types.ProcessUsage{
		Timestamp:    systemNano,
		ProcessCount: &types.UInt64Value{Value: pStats.Current},
	}
}
