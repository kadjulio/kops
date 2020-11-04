/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instancegroups

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/kops/pkg/client/simple"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	api "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/cloudinstances"
	"k8s.io/kops/pkg/validation"
	"k8s.io/kops/upup/pkg/fi"
)

// RollingUpdateCluster is a struct containing cluster information for a rolling update.
type RollingUpdateCluster struct {
	Clientset simple.Clientset
	Ctx       context.Context
	Cluster   *api.Cluster
	Cloud     fi.Cloud

	// MasterInterval is the amount of time to wait after stopping a master instance
	MasterInterval time.Duration
	// NodeInterval is the amount of time to wait after stopping a non-master instance
	NodeInterval time.Duration
	// BastionInterval is the amount of time to wait after stopping a bastion instance
	BastionInterval time.Duration
	// Interactive prompts user to continue after each instance is updated
	Interactive bool

	Force bool

	// K8sClient is the kubernetes client, used for draining etc
	K8sClient kubernetes.Interface

	// ClusterValidator is used for validating the cluster. Unused if CloudOnly
	ClusterValidator validation.ClusterValidator

	FailOnDrainError bool
	FailOnValidate   bool
	CloudOnly        bool
	ClusterName      string

	// PostDrainDelay is the duration we wait after draining each node
	PostDrainDelay time.Duration

	// ValidationTimeout is the maximum time to wait for the cluster to validate, once we start validation
	ValidationTimeout time.Duration

	// ValidateTickDuration is the amount of time to wait between cluster validation attempts
	ValidateTickDuration time.Duration

	// ValidateSuccessDuration is the amount of time a cluster must continue to validate successfully
	// before updating the next node
	ValidateSuccessDuration time.Duration

	// ValidateCount is the amount of time that a cluster needs to be validated after single node update
	ValidateCount int
}

// AdjustNeedUpdate adjusts the set of instances that need updating, using factors outside those known by the cloud implementation
func (*RollingUpdateCluster) AdjustNeedUpdate(groups map[string]*cloudinstances.CloudInstanceGroup) error {
	for _, group := range groups {
		group.AdjustNeedUpdate()
	}
	return nil
}

// RollingUpdate performs a rolling update on a K8s Cluster.
func (c *RollingUpdateCluster) RollingUpdate(groups map[string]*cloudinstances.CloudInstanceGroup, instanceGroups *api.InstanceGroupList) error {
	if len(groups) == 0 {
		klog.Info("Cloud Instance Group length is zero. Not doing a rolling-update.")
		return nil
	}

	var resultsMutex sync.Mutex
	results := make(map[string]error)

	masterGroups := make(map[string]*cloudinstances.CloudInstanceGroup)
	nodeGroups := make(map[string]*cloudinstances.CloudInstanceGroup)
	bastionGroups := make(map[string]*cloudinstances.CloudInstanceGroup)
	for k, group := range groups {
		switch group.InstanceGroup.Spec.Role {
		case api.InstanceGroupRoleNode:
			nodeGroups[k] = group
		case api.InstanceGroupRoleMaster:
			masterGroups[k] = group
		case api.InstanceGroupRoleBastion:
			bastionGroups[k] = group
		default:
			return fmt.Errorf("unknown group type for group %q", group.InstanceGroup.ObjectMeta.Name)
		}
	}

	// Upgrade bastions first; if these go down we can't see anything
	{
		var wg sync.WaitGroup

		for _, k := range sortGroups(bastionGroups) {
			wg.Add(1)
			go func(k string) {
				resultsMutex.Lock()
				results[k] = fmt.Errorf("function panic bastions")
				resultsMutex.Unlock()

				defer wg.Done()

				err := c.rollingUpdateInstanceGroup(bastionGroups[k], c.BastionInterval)

				resultsMutex.Lock()
				results[k] = err
				resultsMutex.Unlock()
			}(k)
		}

		wg.Wait()
	}

	// Do not continue update if bastion(s) failed
	for _, err := range results {
		if err != nil {
			return fmt.Errorf("bastion not healthy after update, stopping rolling-update: %q", err)
		}
	}

	// Upgrade masters next
	{
		// We run master nodes in series, even if they are in separate instance groups
		// typically they will be in separate instance groups, so we can force the zones,
		// and we don't want to roll all the masters at the same time.  See issue #284

		for _, k := range sortGroups(masterGroups) {
			err := c.rollingUpdateInstanceGroup(masterGroups[k], c.MasterInterval)

			// Do not continue update if master(s) failed, cluster is potentially in an unhealthy state
			if err != nil {
				return fmt.Errorf("master not healthy after update, stopping rolling-update: %q", err)
			}
		}
	}

	// Upgrade nodes
	{
		// We run nodes in series, even if they are in separate instance groups
		// typically they will not being separate instance groups. If you roll the nodes in parallel
		// you can get into a scenario where you can evict multiple statefulset pods from the same
		// statefulset at the same time. Further improvements needs to be made to protect from this as
		// well.

		for k := range nodeGroups {
			results[k] = fmt.Errorf("function panic nodes")
		}

		for _, k := range sortGroups(nodeGroups) {
			err := c.rollingUpdateInstanceGroup(nodeGroups[k], c.NodeInterval)

			results[k] = err

			// TODO: Bail on error?
		}
	}

	for _, err := range results {
		if err != nil {
			return err
		}
	}

	klog.Infof("Rolling update completed for cluster %q!", c.ClusterName)
	return nil
}

func sortGroups(groupMap map[string]*cloudinstances.CloudInstanceGroup) []string {
	groups := make([]string, 0, len(groupMap))
	for group := range groupMap {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}
