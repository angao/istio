// Copyright 2018 Istio Authors
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

package monitor

import (
	"os"
	"reflect"
	"strings"
	"syscall"

	"github.com/gogo/protobuf/proto"

	"istio.io/pkg/appsignals"

	"istio.io/pkg/log"

	"istio.io/istio/pilot/pkg/model"
)

// Monitor will poll a config function in order to update a ConfigStore as
// changes are found.
type Monitor struct {
	name            string
	root            string
	store           model.ConfigStore
	configs         []*model.Config
	getSnapshotFunc func() ([]*model.Config, error)
}

// NewMonitor creates a Monitor and will delegate to a passed in controller.
// The controller holds a reference to the actual store.
// Any func that returns a []*model.Config can be used with the Monitor
func NewMonitor(name string, delegateStore model.ConfigStore, getSnapshotFunc func() ([]*model.Config, error), root string) *Monitor {
	monitor := &Monitor{
		name:            name,
		root:            root,
		store:           delegateStore,
		getSnapshotFunc: getSnapshotFunc,
	}
	return monitor
}

// Start starts a new Monitor. Immediately checks the Monitor getSnapshotFunc
// and updates the controller. It then kicks off an asynchronous event loop that
// periodically polls the getSnapshotFunc for changes until a close event is sent.
func (m *Monitor) Start(stop <-chan struct{}) {
	m.checkAndUpdate()

	c := make(chan appsignals.Signal, 1)
	appsignals.Watch(c)
	shut := make(chan os.Signal, 1)
	if err := appsignals.FileTrigger(m.root, syscall.SIGUSR1, shut); err != nil {
		log.Errorf("Unable to setup FileTrigger for %s: %v", m.root, err)
	}
	// Run the close loop asynchronously.
	go func() {
		for {
			select {
			case trigger := <-c:
				if trigger.Signal == syscall.SIGUSR1 {
					log.Infof("Triggering reload in response to: %v", trigger.Source)
					m.checkAndUpdate()
				}
			case <-stop:
				shut <- syscall.SIGTERM
				return
			}
		}
	}()
}

func (m *Monitor) checkAndUpdate() {
	newConfigs, err := m.getSnapshotFunc()
	//If an error exists then log it and return to running the check and update
	//Do not edit the local []*model.config until the connection has been reestablished
	//The error will only come from a directory read error or a gRPC connection error
	if err != nil {
		log.Warnf("checkAndUpdate Error Caught %s: %v\n", m.name, err)
		return
	}

	// make a deep copy of newConfigs to prevent data race
	copyConfigs := make([]*model.Config, 0)
	for _, config := range newConfigs {
		cpy := *config
		cpy.Spec = proto.Clone(config.Spec)
		copyConfigs = append(copyConfigs, &cpy)
	}

	// Compare the new list to the previous one and detect changes.
	oldLen := len(m.configs)
	newLen := len(newConfigs)
	oldIndex, newIndex := 0, 0
	for oldIndex < oldLen && newIndex < newLen {
		oldConfig := m.configs[oldIndex]
		newConfig := newConfigs[newIndex]
		if v := compareIds(oldConfig, newConfig); v < 0 {
			m.deleteConfig(oldConfig)
			oldIndex++
		} else if v > 0 {
			m.createConfig(newConfig)
			newIndex++
		} else {
			// version may change without content changing
			oldConfig.ConfigMeta.ResourceVersion = newConfig.ConfigMeta.ResourceVersion
			if !reflect.DeepEqual(oldConfig, newConfig) {
				m.updateConfig(newConfig)
			}
			oldIndex++
			newIndex++
		}
	}

	// Detect remaining deletions
	for ; oldIndex < oldLen; oldIndex++ {
		m.deleteConfig(m.configs[oldIndex])
	}

	// Detect remaining additions
	for ; newIndex < newLen; newIndex++ {
		m.createConfig(newConfigs[newIndex])
	}

	// Save the updated list.
	m.configs = copyConfigs
}

func (m *Monitor) createConfig(c *model.Config) {
	if _, err := m.store.Create(*c); err != nil {
		log.Warnf("Failed to create config %s %s/%s: %v (%+v)", c.Type, c.Namespace, c.Name, err, *c)
	}
}

func (m *Monitor) updateConfig(c *model.Config) {
	// Set the resource version based on the existing config.
	if prev := m.store.Get(c.GroupVersionKind(), c.Name, c.Namespace); prev != nil {
		c.ResourceVersion = prev.ResourceVersion
	}

	if _, err := m.store.Update(*c); err != nil {
		log.Warnf("Failed to update config (%+v): %v ", *c, err)
	}
}

func (m *Monitor) deleteConfig(c *model.Config) {
	if err := m.store.Delete(c.GroupVersionKind(), c.Name, c.Namespace); err != nil {
		log.Warnf("Failed to delete config (%+v): %v ", *c, err)
	}
}

// compareIds compares the IDs (i.e. Namespace, Type, and Name) of the two configs and returns
// 0 if a == b, -1 if a < b, and 1 if a > b. Used for sorting config arrays.
func compareIds(a, b *model.Config) int {
	return strings.Compare(a.Key(), b.Key())
}
