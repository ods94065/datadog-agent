// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

// +build clusterchecks

package clusterchecks

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/DataDog/datadog-agent/pkg/autodiscovery/integration"
	"github.com/DataDog/datadog-agent/pkg/clusteragent/clusterchecks/types"
)

func generateIntegration(name string) integration.Config {
	return integration.Config{
		Name:         name,
		ClusterCheck: true,
	}
}

func extractCheckNames(configs []integration.Config) []string {
	var names []string
	for _, c := range configs {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

func TestScheduleUnschedule(t *testing.T) {
	dispatcher := newDispatcher()
	stored, err := dispatcher.getAllConfigs()
	assert.NoError(t, err)
	assert.Len(t, stored, 0)

	config1 := integration.Config{
		Name:         "non-cluster-check",
		ClusterCheck: false,
	}
	config2 := integration.Config{
		Name:         "cluster-check",
		ClusterCheck: true,
	}

	dispatcher.Schedule([]integration.Config{config1, config2})
	stored, err = dispatcher.getAllConfigs()
	assert.NoError(t, err)
	assert.Len(t, stored, 1)
	assert.Contains(t, stored, config2)

	node, found := dispatcher.store.digestToNode[config2.Digest()]
	assert.True(t, found)
	assert.Equal(t, "", node)

	dispatcher.Unschedule([]integration.Config{config1, config2})
	stored, err = dispatcher.getAllConfigs()
	assert.NoError(t, err)
	assert.Len(t, stored, 0)
	requireNotLocked(t, dispatcher.store)
}

func TestScheduleReschedule(t *testing.T) {
	dispatcher := newDispatcher()
	config := generateIntegration("cluster-check")

	// Register to node1
	dispatcher.addConfig(config, "node1")
	configs1, _, err := dispatcher.getNodeConfigs("node1")
	assert.NoError(t, err)
	assert.Len(t, configs1, 1)
	assert.Contains(t, configs1, config)

	// Move to node2
	dispatcher.addConfig(config, "node2")
	configs2, _, err := dispatcher.getNodeConfigs("node2")
	assert.NoError(t, err)
	assert.Len(t, configs2, 1)
	assert.Contains(t, configs2, config)

	// De-registered from previous node
	configs1, _, err = dispatcher.getNodeConfigs("node1")
	assert.NoError(t, err)
	assert.Len(t, configs1, 0)

	// Only registered once in global list
	stored, err := dispatcher.getAllConfigs()
	assert.NoError(t, err)
	assert.Len(t, stored, 1)
	assert.Contains(t, stored, config)

	requireNotLocked(t, dispatcher.store)
}

func TestProcessNodeStatus(t *testing.T) {
	dispatcher := newDispatcher()

	status1 := types.NodeStatus{LastChange: 0}

	// Initial node register
	upToDate, err := dispatcher.processNodeStatus("node1", status1)
	assert.NoError(t, err)
	assert.True(t, upToDate)
	node1, found := dispatcher.store.getNodeStore("node1")
	assert.True(t, found)
	assert.Equal(t, status1, node1.lastStatus)
	assert.True(t, timestampNow() >= node1.lastPing)
	assert.True(t, timestampNow() <= node1.lastPing+1)

	// Give changes
	node1.lastConfigChange = timestampNow()
	node1.lastPing = node1.lastPing - 50
	status2 := types.NodeStatus{LastChange: node1.lastConfigChange - 2}
	upToDate, err = dispatcher.processNodeStatus("node1", status2)
	assert.NoError(t, err)
	assert.False(t, upToDate)
	assert.True(t, timestampNow() >= node1.lastPing)
	assert.True(t, timestampNow() <= node1.lastPing+1)

	// No change
	status3 := types.NodeStatus{LastChange: node1.lastConfigChange}
	upToDate, err = dispatcher.processNodeStatus("node1", status3)
	assert.NoError(t, err)
	assert.True(t, upToDate)

	requireNotLocked(t, dispatcher.store)
}

func TestGetLeastBusyNode(t *testing.T) {
	dispatcher := newDispatcher()

	// No node registered -> empty string
	assert.Equal(t, "", dispatcher.getLeastBusyNode())

	// 1 config on node1, 2 on node2
	dispatcher.addConfig(generateIntegration("A"), "node1")
	dispatcher.addConfig(generateIntegration("B"), "node2")
	dispatcher.addConfig(generateIntegration("C"), "node2")
	assert.Equal(t, "node1", dispatcher.getLeastBusyNode())

	// 3 configs on node1, 2 on node2
	dispatcher.addConfig(generateIntegration("D"), "node1")
	dispatcher.addConfig(generateIntegration("E"), "node1")
	assert.Equal(t, "node2", dispatcher.getLeastBusyNode())

	// Add an empty node3
	dispatcher.processNodeStatus("node3", types.NodeStatus{})
	assert.Equal(t, "node3", dispatcher.getLeastBusyNode())

	requireNotLocked(t, dispatcher.store)
}

func TestExpireNodes(t *testing.T) {
	dispatcher := newDispatcher()

	// Empty node list
	assert.Equal(t, 0, len(dispatcher.store.nodes))
	configs := dispatcher.expireNodes()
	assert.Nil(t, configs)

	// Node with no status (bug ?), handled by expiration
	dispatcher.addConfig(generateIntegration("one"), "node1")
	assert.Equal(t, 1, len(dispatcher.store.nodes))
	configs = dispatcher.expireNodes()
	assert.Equal(t, 1, len(configs))
	assert.Equal(t, 0, len(dispatcher.store.nodes))

	// Nodes with valid statuses
	dispatcher.addConfig(generateIntegration("A"), "nodeA")
	dispatcher.addConfig(generateIntegration("B1"), "nodeB")
	dispatcher.addConfig(generateIntegration("B2"), "nodeB")
	dispatcher.processNodeStatus("nodeA", types.NodeStatus{})
	dispatcher.processNodeStatus("nodeB", types.NodeStatus{})
	assert.Equal(t, 2, len(dispatcher.store.nodes))

	// Fake the status report timestamps, nodeB should expire
	dispatcher.store.nodes["nodeA"].lastPing = timestampNow() - 25
	dispatcher.store.nodes["nodeB"].lastPing = timestampNow() - 35

	configs = dispatcher.expireNodes()
	assert.Equal(t, 2, len(configs))
	assert.Equal(t, 1, len(dispatcher.store.nodes))

	// Make sure the expired configs are nodeB's
	assert.Equal(t, []string{"B1", "B2"}, extractCheckNames(configs))

	requireNotLocked(t, dispatcher.store)
}

func TestDispatchFourConfigsTwoNodes(t *testing.T) {
	dispatcher := newDispatcher()

	// Register two nodes
	dispatcher.processNodeStatus("nodeA", types.NodeStatus{})
	dispatcher.processNodeStatus("nodeB", types.NodeStatus{})
	assert.Equal(t, 2, len(dispatcher.store.nodes))

	dispatcher.Schedule([]integration.Config{
		generateIntegration("A"),
		generateIntegration("B"),
		generateIntegration("C"),
		generateIntegration("D"),
	})

	allConfigs, err := dispatcher.getAllConfigs()
	assert.NoError(t, err)
	assert.Equal(t, 4, len(allConfigs))
	assert.Equal(t, []string{"A", "B", "C", "D"}, extractCheckNames(allConfigs))

	configsA, _, err := dispatcher.getNodeConfigs("nodeA")
	assert.NoError(t, err)
	assert.Equal(t, 2, len(configsA))

	configsB, _, err := dispatcher.getNodeConfigs("nodeB")
	assert.NoError(t, err)
	assert.Equal(t, 2, len(configsB))

	// Make sure all checks are on a node
	names := extractCheckNames(configsA)
	names = append(names, extractCheckNames(configsB)...)
	sort.Strings(names)
	assert.Equal(t, []string{"A", "B", "C", "D"}, names)

	requireNotLocked(t, dispatcher.store)
}

func TestDispatchToDummyNode(t *testing.T) {
	dispatcher := newDispatcher()
	config := integration.Config{
		Name:         "cluster-check",
		ClusterCheck: true,
	}

	// No node is available, config will be dispatched to the dummy "" node
	dispatcher.Schedule([]integration.Config{config})
	node, found := dispatcher.store.digestToNode[config.Digest()]
	assert.True(t, found)
	assert.Equal(t, "", node)

	// When expiring that dummy node, the config will be listed for re-dispatching
	expired := dispatcher.expireNodes()
	assert.Equal(t, []integration.Config{config}, expired)
}
