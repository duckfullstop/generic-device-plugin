// Copyright 2020 the generic-device-plugin authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deviceplugin

import (
	"context"
	"crypto/sha1"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	deviceCheckInterval = 5 * time.Second
)

// DeviceSpec defines a device type and the paths at which
// it can be found. Paths can be globs.
type DeviceSpec struct {
	Resource string
	Groups   [][]string
	Count    uint
}

// device wraps the v1.beta1.Device type to add context about
// the device needed by the GenericPlugin.
type device struct {
	v1beta1.Device
	paths []string
}

// GenericPlugin is a plugin for generic devices that can:
// * be found using a file path; and
// * mounted and used without special logic.
type GenericPlugin struct {
	ds      *DeviceSpec
	devices map[string]device
	logger  log.Logger
	mu      sync.Mutex

	// metrics
	deviceGauge        prometheus.Gauge
	allocationsCounter prometheus.Counter
}

// NewGenericPlugin creates a new plugin for a generic device.
func NewGenericPlugin(ds *DeviceSpec, pluginDir string, logger log.Logger, reg prometheus.Registerer) Plugin {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	gp := &GenericPlugin{
		ds:      ds,
		devices: make(map[string]device),
		logger:  logger,
		deviceGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "generic_device_plugin_devices",
			Help: "The number of devices managed by this device plugin.",
		}),
		allocationsCounter: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "generic_device_plugin_allocations_total",
			Help: "The total number of device allocations made by this device plugin.",
		}),
	}

	if reg != nil {
		reg.MustRegister(gp.deviceGauge, gp.allocationsCounter)
	}

	return NewPlugin(ds.Resource, pluginDir, gp, logger, prometheus.WrapRegistererWithPrefix("generic_", reg))
}

func (gp *GenericPlugin) discover() ([]device, error) {
	var devices []device
	for _, group := range gp.ds.Groups {
		paths := make([][]string, len(group))
		var length int
		// Discover all of the devices matching each pattern in the group.
		for i, path := range group {
			matches, err := filepath.Glob(path)
			if err != nil {
				return nil, err
			}
			sort.Strings(matches)
			paths[i] = matches
			// Keep track of the shortest length in the group.
			if length == 0 || len(matches) < length {
				length = len(matches)
			}
		}
		for i := 0; i < length; i++ {
			for j := uint(0); j < gp.ds.Count; j++ {
				h := sha1.New()
				h.Write([]byte(strconv.FormatUint(uint64(j), 10)))
				d := device{
					Device: v1beta1.Device{
						Health: v1beta1.Healthy,
					},
					paths: make([]string, len(group)),
				}
				for k := range group {
					h.Write([]byte(paths[k][i]))
					d.paths[k] = paths[k][i]
				}
				d.ID = fmt.Sprintf("%x", h.Sum(nil))
				devices = append(devices, d)
			}
		}
	}
	return devices, nil
}

// refreshDevices updates the devices available to the
// generic device plugin and returns a boolean indicating
// if everything is OK, i.e. if the devices are the same ones as before.
func (gp *GenericPlugin) refreshDevices() (bool, error) {
	devices, err := gp.discover()
	if err != nil {
		return false, fmt.Errorf("failed to discover devices: %v", err)
	}

	gp.deviceGauge.Set(float64(len(devices)))

	gp.mu.Lock()
	defer gp.mu.Unlock()

	old := gp.devices
	gp.devices = make(map[string]device)

	var equal bool
	// Add the new devices to the map and check
	// if they were in the old map.
	for _, d := range devices {
		gp.devices[d.ID] = d
		if _, ok := old[d.ID]; !ok {
			equal = false
		}
	}
	if !equal {
		return false, nil
	}

	// Check if devices were removed.
	for k := range old {
		if _, ok := gp.devices[k]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// GetDeviceState always returns healthy.
func (gp *GenericPlugin) GetDeviceState(_ string) string {
	return v1beta1.Healthy
}

// Allocate assigns generic devices to a Pod.
func (gp *GenericPlugin) Allocate(_ context.Context, req *v1beta1.AllocateRequest) (*v1beta1.AllocateResponse, error) {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	res := &v1beta1.AllocateResponse{
		ContainerResponses: make([]*v1beta1.ContainerAllocateResponse, 0, len(req.ContainerRequests)),
	}
	for _, r := range req.ContainerRequests {
		resp := new(v1beta1.ContainerAllocateResponse)
		// Add all requested devices to to response.
		for _, id := range r.DevicesIDs {
			dev, ok := gp.devices[id]
			if !ok {
				return nil, fmt.Errorf("requested device does not exist %q", id)
			}
			if dev.Health != v1beta1.Healthy {
				return nil, fmt.Errorf("requested device is not healthy %q", id)
			}
			for _, path := range dev.paths {
				resp.Devices = append(resp.Devices, &v1beta1.DeviceSpec{
					HostPath:      path,
					ContainerPath: path,
					Permissions:   "mrw",
				})
			}
		}
		res.ContainerResponses = append(res.ContainerResponses, resp)
	}
	gp.allocationsCounter.Add(float64(len(res.ContainerResponses)))
	return res, nil
}

// GetDevicePluginOptions always returns an empty response.
func (gp *GenericPlugin) GetDevicePluginOptions(_ context.Context, _ *v1beta1.Empty) (*v1beta1.DevicePluginOptions, error) {
	return &v1beta1.DevicePluginOptions{}, nil
}

// ListAndWatch lists all devices and then refreshes every deviceCheckInterval.
func (gp *GenericPlugin) ListAndWatch(_ *v1beta1.Empty, stream v1beta1.DevicePlugin_ListAndWatchServer) error {
	level.Info(gp.logger).Log("msg", "starting listwatch")
	if _, err := gp.refreshDevices(); err != nil {
		return err
	}
	ok := false
	var err error
	for {
		if !ok {
			res := new(v1beta1.ListAndWatchResponse)
			for _, dev := range gp.devices {
				res.Devices = append(res.Devices, &v1beta1.Device{ID: dev.ID, Health: dev.Health})
			}
			if err := stream.Send(res); err != nil {
				return err
			}
		}
		<-time.After(deviceCheckInterval)
		ok, err = gp.refreshDevices()
		if err != nil {
			return err
		}
	}
}

// PreStartContainer always returns an empty response.
func (gp *GenericPlugin) PreStartContainer(_ context.Context, _ *v1beta1.PreStartContainerRequest) (*v1beta1.PreStartContainerResponse, error) {
	return &v1beta1.PreStartContainerResponse{}, nil
}

// GetPreferredAllocation always returns an empty response.
func (gp *GenericPlugin) GetPreferredAllocation(context.Context, *v1beta1.PreferredAllocationRequest) (*v1beta1.PreferredAllocationResponse, error) {
	return &v1beta1.PreferredAllocationResponse{}, nil
}
