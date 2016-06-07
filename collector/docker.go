// Copyright (c) 2016 Pulcy.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"golang.org/x/net/context"
)

const (
	dockerNamespace = "docker"
)

type dockerCollector struct {
	containerCpuUsageDesc    *prometheus.Desc
	containerMemoryUsageDesc *prometheus.Desc
	api                      *client.Client
}

func init() {
	Factories["docker"] = NewDockerCollector
}

// Take a prometheus registry and return a new Collector exposing docker container statistics
func NewDockerCollector() (Collector, error) {
	api, err := client.NewEnvClient()
	if err != nil {
		return nil, maskAny(err)
	}

	return &dockerCollector{
		api: api,
		containerCpuUsageDesc: prometheus.NewDesc(
			prometheus.BuildFQName(dockerNamespace, "", "cpu_usage"),
			"Docker container CPU usage", []string{"name"}, nil,
		),
		containerMemoryUsageDesc: prometheus.NewDesc(
			prometheus.BuildFQName(dockerNamespace, "", "mem_usage"),
			"Docker container memory usage", []string{"name"}, nil,
		),
	}, nil
}

func (c *dockerCollector) Update(ch chan<- prometheus.Metric) (err error) {
	containers, err := c.api.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		return maskAny(err)
	}

	wg := sync.WaitGroup{}
	for _, cnt := range containers {
		wg.Add(1)
		go func(cnt types.Container) {
			defer wg.Done()
			if err := c.collectStats(cnt, ch); err != nil {
				log.Errorf("failed to load statistics for %s: %#v", cnt.ID, err)
			}
		}(cnt)
	}
	wg.Wait()

	return nil
}

func (c *dockerCollector) collectStats(cnt types.Container, ch chan<- prometheus.Metric) (err error) {
	rd, err := c.api.ContainerStats(context.Background(), cnt.ID, false)
	if err != nil {
		return maskAny(err)
	}
	defer rd.Close()
	var stats types.Stats
	decoder := json.NewDecoder(rd)
	if err := decoder.Decode(&stats); err != nil {
		return maskAny(err)
	}

	name := cnt.ID
	if len(cnt.Names) > 0 {
		name = cnt.Names[0]
	}
	if strings.HasPrefix(name, "/") {
		name = name[1:]
	}

	previousCPU := stats.PreCPUStats.CPUUsage.TotalUsage
	previousSystem := stats.PreCPUStats.SystemUsage
	cpuPercent := calculateCPUPercent(previousCPU, previousSystem, &stats)

	ch <- prometheus.MustNewConstMetric(
		c.containerCpuUsageDesc, prometheus.GaugeValue, cpuPercent,
		name)
	ch <- prometheus.MustNewConstMetric(
		c.containerMemoryUsageDesc, prometheus.GaugeValue, float64(stats.MemoryStats.Usage),
		name)

	return nil
}

func calculateCPUPercent(previousCPU, previousSystem uint64, v *types.Stats) float64 {
	var (
		cpuPercent = 0.0
		// calculate the change for the cpu usage of the container in between readings
		cpuDelta = float64(v.CPUStats.CPUUsage.TotalUsage) - float64(previousCPU)
		// calculate the change for the entire system between readings
		systemDelta = float64(v.CPUStats.SystemUsage) - float64(previousSystem)
	)

	if systemDelta > 0.0 && cpuDelta > 0.0 {
		cpuPercent = (cpuDelta / systemDelta) * float64(len(v.CPUStats.CPUUsage.PercpuUsage)) * 100.0
	}
	return cpuPercent
}
