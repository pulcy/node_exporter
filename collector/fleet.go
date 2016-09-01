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
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/fleet/client"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	fleetNamespace = "fleet"
)

type fleetCollector struct {
	metric                      []prometheus.Gauge
	unitFailedStateDesc         *prometheus.Desc
	unitShouldBeActiveStateDesc *prometheus.Desc
	api                         client.API
}

func init() {
	Factories["fleet"] = NewFleetCollector
}

// Take a prometheus registry and return a new Collector exposing fleet unit counts
func NewFleetCollector() (Collector, error) {
	fleetURL := "unix:///var/run/fleet.sock"
	endpoint, err := url.Parse(fleetURL)
	if err != nil {
		return nil, maskAny(err)
	}
	api, err := createFleetClient(*endpoint)
	if err != nil {
		return nil, maskAny(err)
	}

	return &fleetCollector{
		api: api,
		unitFailedStateDesc: prometheus.NewDesc(
			prometheus.BuildFQName(fleetNamespace, "", "unit_failed_state"),
			"Fleet failed unit state", []string{"name"}, nil,
		),
		unitShouldBeActiveStateDesc: prometheus.NewDesc(
			prometheus.BuildFQName(fleetNamespace, "", "unit_should_be_active"),
			"Fleet inactive unit that should be active", []string{"name"}, nil,
		),
		metric: []prometheus.Gauge{
			prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: fleetNamespace,
				Name:      "units_running",
				Help:      "# running units",
			}),
			prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: fleetNamespace,
				Name:      "units_down",
				Help:      "# down units",
			}),
			prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: fleetNamespace,
				Name:      "units_down_should_be_active",
				Help:      "# down units that should be active",
			}),
			prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: fleetNamespace,
				Name:      "units_failed",
				Help:      "# failed units",
			}),
			prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: fleetNamespace,
				Name:      "units_starting",
				Help:      "# starting units",
			}),
			prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: fleetNamespace,
				Name:      "units_stopping",
				Help:      "# stopping units",
			}),
			prometheus.NewGauge(prometheus.GaugeOpts{
				Namespace: fleetNamespace,
				Name:      "machines",
				Help:      "# machines",
			}),
		},
	}, nil
}

func (c *fleetCollector) Update(ch chan<- prometheus.Metric) (err error) {
	states, err := c.api.UnitStates()
	if err != nil {
		return maskAny(err)
	}

	running := 0
	down := 0
	downShouldBeActive := 0
	failed := 0
	starting := 0
	stopping := 0

	for _, s := range states {
		stateValue := 0.0
		shouldBeActiveStateValue := 0.0
		switch s.SystemdActiveState {
		case "inactive":
			down++
			rc, err := shouldUnitBeActive(s.Name, c.api)
			if err == nil && rc {
				downShouldBeActive++
				shouldBeActiveStateValue = 1.0
			}
		case "failed":
			failed++
			stateValue = 1.0
		case "activating":
			starting++
		case "deactivating":
			stopping++
		case "active", "reloading":
			switch s.SystemdSubState {
			case "stop-sigterm", "stop-post", "stop":
				stopping++
			case "auto-restart", "launched", "start-pre", "start-post", "start", "dead":
				starting++

			case "exited", "running":
				running++
			}
		}

		ch <- prometheus.MustNewConstMetric(
			c.unitFailedStateDesc, prometheus.GaugeValue, stateValue,
			s.Name)
		ch <- prometheus.MustNewConstMetric(
			c.unitShouldBeActiveStateDesc, prometheus.GaugeValue, shouldBeActiveStateValue,
			s.Name)
	}

	machines, err := c.api.Machines()
	if err != nil {
		return maskAny(err)
	}

	c.metric[0].Set(float64(running))
	c.metric[1].Set(float64(down))
	c.metric[2].Set(float64(downShouldBeActive))
	c.metric[3].Set(float64(failed))
	c.metric[4].Set(float64(starting))
	c.metric[5].Set(float64(stopping))
	c.metric[6].Set(float64(len(machines)))

	for _, m := range c.metric {
		m.Collect(ch)
	}

	return nil
}

func shouldUnitBeActive(name string, api client.API) (bool, error) {
	if !strings.HasSuffix(name, ".service") {
		return false, nil
	}
	unit, err := api.Unit(name)
	if err != nil {
		return false, maskAny(err)
	}
	serviceType := ""
	stopWhenUnneeded := ""
	for _, option := range unit.Options {
		switch strings.ToLower(option.Section) {
		case "service":
			if strings.ToLower(option.Name) == "type" {
				serviceType = strings.ToLower(option.Value)
			}
		case "unit":
			if strings.ToLower(option.Name) == "stopwhenunneeded" {
				stopWhenUnneeded = strings.ToLower(option.Value)
			}
		}
	}
	if serviceType == "oneshot" {
		return false, nil
	}
	if stopWhenUnneeded == "yes" {
		return false, nil
	}
	return true, nil
}

func createFleetClient(endpoint url.URL) (client.API, error) {
	var trans http.RoundTripper

	switch endpoint.Scheme {
	case "unix", "file":
		if len(endpoint.Host) > 0 {
			return nil, fmt.Errorf("unable to connect to host %q with scheme %q", endpoint.Host, endpoint.Scheme)
		}

		// The Path field is only used for dialing and should not be used when
		// building any further HTTP requests.
		sockPath := endpoint.Path
		endpoint.Path = ""

		// http.Client doesn't support the schemes "unix" or "file", but it
		// is safe to use "http" as dialFunc ignores it anyway.
		endpoint.Scheme = "http"

		// The Host field is not used for dialing, but will be exposed in debug logs.
		endpoint.Host = "domain-sock"

		trans = &http.Transport{
			Dial: func(s, t string) (net.Conn, error) {
				// http.Client does not natively support dialing a unix domain socket, so the
				// dial function must be overridden.
				return net.Dial("unix", sockPath)
			},
		}
	case "http", "https":
		trans = http.DefaultTransport
	default:
		return nil, fmt.Errorf("Unknown scheme in fleet endpoint: %s", endpoint.Scheme)
	}

	c := &http.Client{
		Transport: trans,
	}
	fAPI, err := client.NewHTTPClient(c, endpoint)
	if err != nil {
		return nil, maskAny(err)
	}
	return fAPI, nil
}
