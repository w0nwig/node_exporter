// Copyright 2018 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"bytes"
	"fmt"
	cadvisor "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/metrics"
	"github.com/json-iterator/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"io/ioutil"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	"net/http"
	"regexp"
)

type cadvisorRequest struct {
	NumStats      int    `json:"num_stats,omitempty"`
	Subcontainers bool   `json:"subcontainers,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
}

func init() {
	registerCollector("rate", defaultEnabled, NewContainerRateStatsCollector)
}

type containerRateStats struct {
	c *http.Client
}

// NewContainerRateStatsCollector returns a new Collector exposing container rate stats.
func NewContainerRateStatsCollector() (Collector, error) {
	return &containerRateStats{
		c: http.DefaultClient,
	}, nil
}

func (crs *containerRateStats) Update(ch chan<- prometheus.Metric) error {
	// Request data from all subcontainers.
	request := cadvisorRequest{
		ContainerName: "/",
		NumStats:      2,
		Subcontainers: true,
	}
	body, err := jsoniter.ConfigFastest.Marshal(request)
	if err != nil {
		log.Errorf("failed to marshal containers stats: %v", err)
		return err
	}
	req, err := http.NewRequest("POST", "http://127.0.0.1:10255/stats/container", bytes.NewBuffer(body))
	if err != nil {
		log.Errorf("failed to request to stats containers: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	var containers map[string]cadvisor.ContainerInfo
	err = crs.postRequestAndGetValue(crs.c, req, &containers)
	if err != nil {
		return fmt.Errorf("failed to get all container stats from Kubelet: %v", err)
	}

	log.Infof("containers length: %d", len(containers))
	result := make([]cadvisor.ContainerInfo, 0, len(containers))
	for _, containerInfo := range containers {
		cont := crs.parseStat(&containerInfo)
		if cont != nil {
			result = append(result, *cont)
		}
	}

	rawLabels := map[string]struct{}{}
	for _, container := range containers {
		rl := containerPrometheusLabelsFunc(&container)
		if rl == nil {
			continue
		}
		for l := range rl {
			rawLabels[l] = struct{}{}
		}
	}

	for _, container := range result {

		log.Infof("stats length %d", len(container.Stats))

		// Now for the actual metrics
		if len(container.Stats) < 2 {
			continue
		}

		stats := container.Stats

		values := make([]string, 0, len(rawLabels))
		labels := make([]string, 0, len(rawLabels))
		containerLabels := containerPrometheusLabelsFunc(&container)
		if containerLabels == nil {
			log.Info("not found labels and continue.")
			continue
		}
		for l := range rawLabels {
			labels = append(labels, sanitizeLabelName(l))
			values = append(values, containerLabels[l])
		}

		time_delta := stats[1].Timestamp.UnixNano() - stats[0].Timestamp.UnixNano()

		if container.Spec.HasCpu {
			desc := prometheus.NewDesc("container_cpu_user_usage_rate", "container_cpu_user_usage_rate", labels, nil)
			cpu_user_usage := stats[1].Cpu.Usage.User - stats[0].Cpu.Usage.User
			if cpu_user_usage > 0 {
				user_usage_rate := float64(cpu_user_usage) / float64(time_delta)
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(user_usage_rate), values...)
			}

			desc = prometheus.NewDesc("container_cpu_system_usage_rate", "container_cpu_system_usage_rate", labels, nil)
			cpu_system_usage := stats[1].Cpu.Usage.System - stats[0].Cpu.Usage.System
			if cpu_system_usage > 0 {
				system_usage_rate := float64(cpu_system_usage) / float64(time_delta)
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(system_usage_rate), values...)
			}

			desc = prometheus.NewDesc("container_cpu_usage_rate", "container_cpu_usage_rate", labels, nil)
			cpu_usage := stats[1].Cpu.Usage.Total - stats[0].Cpu.Usage.Total
			if cpu_usage > 0 {
				cpu_usage_rate := float64(cpu_usage) / float64(time_delta)
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(cpu_usage_rate), values...)
			}

			desc = prometheus.NewDesc("container_cpu_cfs_rate", "container_cpu_cfs_rate", labels, nil)
			cfs_usage := stats[1].Cpu.CFS.ThrottledTime - stats[0].Cpu.CFS.ThrottledTime
			if cfs_usage > 0 {
				cfs_usage_rate := float64(cfs_usage) / float64(time_delta)
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(cfs_usage_rate), values...)
			}
		}

		if container.Spec.HasNetwork {
			desc := prometheus.NewDesc("container_network_receive_bytes_rate", "container_network_receive_bytes_total", labels, nil)
			rx_total := stats[1].Network.RxBytes - stats[0].Network.RxBytes
			if rx_total > 0 {
				rx_rate := float64(rx_total) / float64(time_delta)
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(rx_rate), values...)
			}

			desc = prometheus.NewDesc("container_network_transmit_bytes_rate", "container_network_transmit_bytes_total", labels, nil)
			tx_total := stats[1].Network.TxBytes - stats[0].Network.TxBytes
			if tx_total > 0 {
				tx_rate := float64(tx_total) / float64(time_delta)
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(tx_rate), values...)
			}
		}

	}

	return nil
}

func (crs *containerRateStats) postRequestAndGetValue(client *http.Client, req *http.Request, value interface{}) error {
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body - %v", err)
	}
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("404 not found")
	} else if response.StatusCode != http.StatusOK {
		return fmt.Errorf("request failed - %q, response: %q", response.Status, string(body))
	}

	kubeletAddr := "[unknown]"
	if req.URL != nil {
		kubeletAddr = req.URL.Host
	}
	log.Infof("Raw response from Kubelet at %s: %s", kubeletAddr, string(body))

	err = jsoniter.ConfigFastest.Unmarshal(body, value)
	if err != nil {
		return fmt.Errorf("failed to parse output. Response: %q. Error: %v", string(body), err)
	}
	return nil
}

func (crs *containerRateStats) parseStat(containerInfo *cadvisor.ContainerInfo) *cadvisor.ContainerInfo {
	containerInfo.Stats = sampleContainerStats(containerInfo.Stats)
	if len(containerInfo.Aliases) > 0 {
		containerInfo.Name = containerInfo.Aliases[0]
	}
	return containerInfo
}

func sampleContainerStats(stats []*cadvisor.ContainerStats) []*cadvisor.ContainerStats {
	if len(stats) == 0 {
		return []*cadvisor.ContainerStats{}
	}
	return stats
}

func containerPrometheusLabelsFunc(c *cadvisor.ContainerInfo) map[string]string {
	// containerPrometheusLabels maps cAdvisor labels to prometheus labels.
	// Prometheus requires that all metrics in the same family have the same labels,
	// so we arrange to supply blank strings for missing labels
	var name, image, podName, namespace, containerName string
	if len(c.Aliases) > 0 {
		name = c.Aliases[0]
	}
	image = c.Spec.Image
	if v, ok := c.Spec.Labels[kubelettypes.KubernetesPodNameLabel]; ok {
		podName = v
	}
	if v, ok := c.Spec.Labels[kubelettypes.KubernetesPodNamespaceLabel]; ok {
		namespace = v
	}
	if v, ok := c.Spec.Labels[kubelettypes.KubernetesContainerNameLabel]; ok {
		containerName = v
	}
	// Associate pod cgroup with pod so we have an accurate accounting of sandbox
	if podName == "" && namespace == "" {
		return nil
	}
	set := map[string]string{
		metrics.LabelID:    c.Name,
		metrics.LabelName:  name,
		metrics.LabelImage: image,
		"pod_name":         podName,
		"namespace":        namespace,
		"container_name":   containerName,
	}
	return set

}

var invalidLabelCharRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// sanitizeLabelName replaces anything that doesn't match
// client_label.LabelNameRE with an underscore.
func sanitizeLabelName(name string) string {
	return invalidLabelCharRE.ReplaceAllString(name, "_")
}
