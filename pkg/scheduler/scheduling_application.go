/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package scheduler

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/apache/incubator-yunikorn-core/pkg/cache"
	"github.com/apache/incubator-yunikorn-core/pkg/common/resources"
	"github.com/apache/incubator-yunikorn-core/pkg/log"
	"github.com/apache/incubator-yunikorn-core/pkg/plugins"
	"github.com/apache/incubator-yunikorn-scheduler-interface/lib/go/si"
)

var reservationDelay = 2 * time.Second

type SchedulingApplication struct {
	ApplicationInfo *cache.ApplicationInfo

	// Private fields need protection
	queue          *SchedulingQueue                    // queue the application is running in
	allocating     *resources.Resource                 // allocating resource set by the scheduler
	pending        *resources.Resource                 // pending resources from asks for the app
	reservations   map[string]*reservation             // a map of reservations
	requests       map[string]*schedulingAllocationAsk // a map of asks
	sortedRequests []*schedulingAllocationAsk

	sync.RWMutex
}

func newSchedulingApplication(appInfo *cache.ApplicationInfo) *SchedulingApplication {
	return &SchedulingApplication{
		ApplicationInfo: appInfo,
		allocating:      resources.NewResource(),
		pending:         resources.NewResource(),
		requests:        make(map[string]*schedulingAllocationAsk),
		reservations:    make(map[string]*reservation),
	}
}

// override reservation delay for tests
func OverrideReservationDelay(delay time.Duration) {
	log.Logger().Debug("Test override reservation delay",
		zap.Duration("delay", delay))
	reservationDelay = delay
}

// Return an array of all reservation keys for the app.
// This will return an empty array if there are no reservations.
// Visible for tests
func (sa *SchedulingApplication) GetReservations() []string {
	sa.RLock()
	defer sa.RUnlock()
	keys := make([]string, 0)
	for key := range sa.reservations {
		keys = append(keys, key)
	}
	return keys
}

// Return the allocation ask for the key, nil if not found
func (sa *SchedulingApplication) GetSchedulingAllocationAsk(allocationKey string) *schedulingAllocationAsk {
	sa.RLock()
	defer sa.RUnlock()
	return sa.requests[allocationKey]
}

// Return the allocated resources for this application
func (sa *SchedulingApplication) GetAllocatedResource() *resources.Resource {
	return sa.ApplicationInfo.GetAllocatedResource()
}

// Return the pending resources for this application
func (sa *SchedulingApplication) GetPendingResource() *resources.Resource {
	sa.RLock()
	defer sa.RUnlock()
	return sa.pending
}

// Return the allocating and allocated resources for this application
func (sa *SchedulingApplication) getAssumeAllocated() *resources.Resource {
	sa.RLock()
	defer sa.RUnlock()
	return resources.Add(sa.allocating, sa.ApplicationInfo.GetAllocatedResource())
}

// Return the allocating resources for this application
func (sa *SchedulingApplication) getAllocatingResource() *resources.Resource {
	sa.RLock()
	defer sa.RUnlock()
	return sa.allocating
}

// Increment allocating resource for the app
func (sa *SchedulingApplication) incAllocatingResource(delta *resources.Resource) {
	sa.Lock()
	defer sa.Unlock()
	sa.allocating.AddTo(delta)
}

// Decrement allocating resource for the app
func (sa *SchedulingApplication) decAllocatingResource(delta *resources.Resource) {
	sa.Lock()
	defer sa.Unlock()
	var err error
	sa.allocating, err = resources.SubErrorNegative(sa.allocating, delta)
	if err != nil {
		log.Logger().Warn("Allocating resources went negative",
			zap.Error(err))
	}
}

// Remove one or more allocation asks from this application.
// This also removes any reservations that are linked to the ask.
// The return value is the number of reservations released
func (sa *SchedulingApplication) removeAllocationAsk(allocKey string) int {
	sa.Lock()
	defer sa.Unlock()
	// shortcut no need to do anything
	if len(sa.requests) == 0 {
		return 0
	}
	var deltaPendingResource *resources.Resource = nil
	// when allocation key not specified, cleanup all allocation ask
	var toRelease int
	if allocKey == "" {
		// cleanup all reservations
		for key, reserve := range sa.reservations {
			_, err := reserve.unReserve()
			if err != nil {
				log.Logger().Warn("Removal of reservation failed while removing all allocations",
					zap.String("appID", sa.ApplicationInfo.ApplicationID),
					zap.String("reservationKey", key),
					zap.Error(err))
			}
			// clean up the queue reservation (one at a time)
			sa.queue.unReserve(sa.ApplicationInfo.ApplicationID)
			toRelease++
		}
		// Cleanup total pending resource
		deltaPendingResource = sa.pending
		sa.pending = resources.NewResource()
		sa.requests = make(map[string]*schedulingAllocationAsk)
	} else {
		// cleanup the reservation for this allocation
		for _, key := range sa.isAskReserved(allocKey) {
			_, err := sa.reservations[key].unReserve()
			if err != nil {
				log.Logger().Warn("Removal of reservation failed while removing allocation",
					zap.String("appID", sa.ApplicationInfo.ApplicationID),
					zap.String("reservationKey", key),
					zap.Error(err))
			}
			// clean up the queue reservation
			sa.queue.unReserve(sa.ApplicationInfo.ApplicationID)
			toRelease++
		}
		if ask := sa.requests[allocKey]; ask != nil {
			deltaPendingResource = resources.MultiplyBy(ask.AllocatedResource, float64(ask.getPendingAskRepeat()))
			sa.pending.SubFrom(deltaPendingResource)
			delete(sa.requests, allocKey)
		}
	}
	// clean up the queue pending resources
	sa.queue.decPendingResource(deltaPendingResource)
	return toRelease
}

// Add an allocation ask to this application
// If the ask already exist update the existing info
func (sa *SchedulingApplication) addAllocationAsk(ask *schedulingAllocationAsk) (*resources.Resource, error) {
	sa.Lock()
	defer sa.Unlock()
	if ask == nil {
		return nil, fmt.Errorf("ask cannot be nil when added to app %s", sa.ApplicationInfo.ApplicationID)
	}
	if ask.getPendingAskRepeat() == 0 || resources.IsZero(ask.AllocatedResource) {
		return nil, fmt.Errorf("invalid ask added to app %s: %v", sa.ApplicationInfo.ApplicationID, ask)
	}
	ask.QueueName = sa.queue.Name
	delta := resources.Multiply(ask.AllocatedResource, int64(ask.getPendingAskRepeat()))

	var oldAskResource *resources.Resource = nil
	if oldAsk := sa.requests[ask.AskProto.AllocationKey]; oldAsk != nil {
		oldAskResource = resources.Multiply(oldAsk.AllocatedResource, int64(oldAsk.getPendingAskRepeat()))
	}

	delta.SubFrom(oldAskResource)
	sa.requests[ask.AskProto.AllocationKey] = ask

	// Update total pending resource
	sa.pending.AddTo(delta)
	sa.queue.incPendingResource(delta)

	return delta, nil
}

func (sa *SchedulingApplication) updateAskRepeat(allocKey string, delta int32) (*resources.Resource, error) {
	sa.Lock()
	defer sa.Unlock()
	if ask := sa.requests[allocKey]; ask != nil {
		return sa.updateAskRepeatInternal(ask, delta)
	}
	return nil, fmt.Errorf("failed to locate ask with key %s", allocKey)
}

func (sa *SchedulingApplication) updateAskRepeatInternal(ask *schedulingAllocationAsk, delta int32) (*resources.Resource, error) {
	// updating with delta does error checking internally
	if !ask.updatePendingAskRepeat(delta) {
		return nil, fmt.Errorf("ask repaeat not updated resulting repeat less than zero for ask %s on app %s", ask.AskProto.AllocationKey, sa.ApplicationInfo.ApplicationID)
	}

	deltaPendingResource := resources.Multiply(ask.AllocatedResource, int64(delta))
	sa.pending.AddTo(deltaPendingResource)
	// update the pending of the queue with the same delta
	sa.queue.incPendingResource(deltaPendingResource)

	return deltaPendingResource, nil
}

// Return if the application has any reservations.
func (sa *SchedulingApplication) hasReserved() bool {
	sa.RLock()
	defer sa.RUnlock()
	return len(sa.reservations) > 0
}

// Return if the application has the node reserved.
// An empty nodeID is never reserved.
func (sa *SchedulingApplication) isReservedOnNode(nodeID string) bool {
	if nodeID == "" {
		return false
	}
	sa.RLock()
	defer sa.RUnlock()
	for key := range sa.reservations {
		if strings.HasPrefix(key, nodeID) {
			return true
		}
	}
	return false
}

// Reserve the application for this node and ask combination.
// If the reservation fails the function returns false, if the reservation is made it returns true.
// If the node and ask combination was already reserved for the application this is a noop and returns true.
func (sa *SchedulingApplication) reserve(node *SchedulingNode, ask *schedulingAllocationAsk) error {
	sa.Lock()
	defer sa.Unlock()
	// create the reservation (includes nil checks)
	nodeReservation := newReservation(node, sa, ask, true)
	if nodeReservation == nil {
		log.Logger().Debug("reservation creation failed unexpectedly",
			zap.String("app", sa.ApplicationInfo.ApplicationID),
			zap.Any("node", node),
			zap.Any("ask", ask))
		return fmt.Errorf("reservation creation failed node or ask are nil on appID %s", sa.ApplicationInfo.ApplicationID)
	}
	allocKey := ask.AskProto.AllocationKey
	if sa.requests[allocKey] == nil {
		log.Logger().Debug("ask is not registered to this app",
			zap.String("app", sa.ApplicationInfo.ApplicationID),
			zap.String("allocKey", allocKey))
		return fmt.Errorf("reservation creation failed ask %s not found on appID %s", allocKey, sa.ApplicationInfo.ApplicationID)
	}
	if !sa.canAskReserve(ask) {
		return fmt.Errorf("reservation of ask exceeds pending repeat, pending ask repeat %d", ask.getPendingAskRepeat())
	}
	// check if we can reserve the node before reserving on the app
	if err := node.reserve(sa, ask); err != nil {
		return err
	}
	sa.reservations[nodeReservation.getKey()] = nodeReservation
	// reservation added successfully
	return nil
}

// unReserve the application for this node and ask combination.
// This first removes the reservation from the node.
// The error is set if the reservation key cannot be generated on the app or node.
// If the reservation does not exist it returns false, if the reservation is removed it returns true.
func (sa *SchedulingApplication) unReserve(node *SchedulingNode, ask *schedulingAllocationAsk) error {
	sa.Lock()
	defer sa.Unlock()
	return sa.unReserveInternal(node, ask)
}

// Unlocked version for unReserve that really does the work.
// Must only be called while holding the application lock.
func (sa *SchedulingApplication) unReserveInternal(node *SchedulingNode, ask *schedulingAllocationAsk) error {
	resKey := reservationKey(node, nil, ask)
	if resKey == "" {
		log.Logger().Debug("unreserve reservation key create failed unexpectedly",
			zap.String("appID", sa.ApplicationInfo.ApplicationID),
			zap.Any("node", node),
			zap.Any("ask", ask))
		return fmt.Errorf("reservation key failed node or ask are nil for appID %s", sa.ApplicationInfo.ApplicationID)
	}
	// find the reservation and then unReserve the node before removing from the app
	if _, found := sa.reservations[resKey]; found {
		if err := node.unReserve(sa, ask); err != nil {
			return err
		}
		delete(sa.reservations, resKey)
		return nil
	}
	// reservation was not found
	log.Logger().Debug("reservation not found while removing from app",
		zap.String("appID", sa.ApplicationInfo.ApplicationID),
		zap.String("nodeID", node.NodeID),
		zap.String("ask", ask.AskProto.AllocationKey))
	return nil
}

// Return the allocation reservations on any node.
// The returned array is 0 or more keys into the reservations map.
// No locking must be called while holding the lock
func (sa *SchedulingApplication) isAskReserved(allocKey string) []string {
	reservationKeys := make([]string, 0)
	if allocKey == "" {
		return reservationKeys
	}
	for key := range sa.reservations {
		if strings.HasSuffix(key, allocKey) {
			reservationKeys = append(reservationKeys, key)
		}
	}
	return reservationKeys
}

// Check if the allocation has already been reserved. An ask can reserve multiple nodes if the request has a repeat set
// larger than 1. It can never reserve more than the repeat number of nodes.
// No locking must be called while holding the lock
func (sa *SchedulingApplication) canAskReserve(ask *schedulingAllocationAsk) bool {
	allocKey := ask.AskProto.AllocationKey
	pending := int(ask.getPendingAskRepeat())
	resNumber := sa.isAskReserved(allocKey)
	if len(resNumber) >= pending {
		log.Logger().Debug("reservation exceeds repeats",
			zap.String("askKey", allocKey),
			zap.Int("askPending", pending),
			zap.Int("askReserved", len(resNumber)))
	}
	return pending > len(resNumber)
}

// Sort the request for the app in order based on the priority of the request.
// The sorted list only contains candidates that have an outstanding repeat.
// No locking must be called while holding the lock
func (sa *SchedulingApplication) sortRequests(ascending bool) {
	sa.sortedRequests = nil
	for _, request := range sa.requests {
		if request.getPendingAskRepeat() == 0 {
			continue
		}
		sa.sortedRequests = append(sa.sortedRequests, request)
	}
	// we might not have any requests
	if len(sa.sortedRequests) > 0 {
		sortAskByPriority(sa.sortedRequests, ascending)
	}
}

// Try a regular allocation of the pending requests
func (sa *SchedulingApplication) tryAllocate(headRoom *resources.Resource, ctx *partitionSchedulingContext) *schedulingAllocation {
	sa.Lock()
	defer sa.Unlock()
	// make sure the request are sorted
	sa.sortRequests(false)
	// get all the requests from the app sorted in order
	for _, request := range sa.sortedRequests {
		// resource must fit in headroom otherwise skip the request
		if !resources.FitIn(headRoom, request.AllocatedResource) {
			continue
		}
		if nodeIterator := ctx.getNodeIterator(); nodeIterator != nil {
			alloc := sa.tryNodes(request, nodeIterator)
			// have a candidate return it
			if alloc != nil {
				return alloc
			}
		}
	}
	// no requests fit, skip to next app
	return nil
}

// Try a reserved allocation of an outstanding reservation
func (sa *SchedulingApplication) tryReservedAllocate(headRoom *resources.Resource, ctx *partitionSchedulingContext) *schedulingAllocation {
	sa.Lock()
	defer sa.Unlock()
	// process all outstanding reservations and pick the first one that fits
	for _, reserve := range sa.reservations {
		ask := sa.requests[reserve.askKey]
		// sanity check and cleanup if needed
		if ask == nil || ask.getPendingAskRepeat() == 0 {
			var unreserveAsk *schedulingAllocationAsk
			// if the ask was not found we need to construct one to unreserve
			if ask == nil {
				unreserveAsk = &schedulingAllocationAsk{
					AskProto:      &si.AllocationAsk{AllocationKey: reserve.askKey},
					ApplicationID: sa.ApplicationInfo.ApplicationID,
					QueueName:     sa.queue.Name,
				}
			} else {
				unreserveAsk = ask
			}
			// remove the reservation as this should not be reserved
			alloc := newSchedulingAllocation(unreserveAsk, reserve.nodeID)
			alloc.result = unreserved
			return alloc
		}
		// check if this fits in the queue's head room
		if !resources.FitIn(headRoom, ask.AllocatedResource) {
			continue
		}
		// check allocation possibility
		alloc := sa.tryNode(reserve.node, ask)
		// allocation worked set the result and return
		if alloc != nil {
			alloc.result = allocatedReserved
			return alloc
		}
	}
	// lets try this on all other nodes
	for _, reserve := range sa.reservations {
		if nodeIterator := ctx.getNodeIterator(); nodeIterator != nil {
			alloc := sa.tryNodesNoReserve(reserve.ask, nodeIterator, reserve.nodeID)
			// have a candidate return it, including the node that was reserved
			if alloc != nil {
				return alloc
			}
		}
	}
	return nil
}

// Try all the nodes for a reserved request that have not been tried yet.
// This should never result in a reservation as the ask is already reserved
func (sa *SchedulingApplication) tryNodesNoReserve(ask *schedulingAllocationAsk, nodeIterator NodeIterator, reservedNode string) *schedulingAllocation {
	for nodeIterator.HasNext() {
		node := nodeIterator.Next()
		// skip over the node if the resource does not fit the node or this is the reserved node.
		if !node.nodeInfo.FitInNode(ask.AllocatedResource) || node.NodeID == reservedNode {
			continue
		}
		alloc := sa.tryNode(node, ask)
		// allocation worked so return
		if alloc != nil {
			alloc.reservedNodeID = reservedNode
			alloc.result = allocatedReserved
			return alloc
		}
	}
	// ask does not fit, skip to next ask
	return nil
}

// Try all the nodes for a request. The result is an allocation or reservation of a node.
// New allocations can only be reserved after a delay.
func (sa *SchedulingApplication) tryNodes(ask *schedulingAllocationAsk, nodeIterator NodeIterator) *schedulingAllocation {
	var nodeToReserve *SchedulingNode
	scoreReserved := math.Inf(1)
	// check if the ask is reserved or not
	allocKey := ask.AskProto.AllocationKey
	reservedAsks := sa.isAskReserved(allocKey)
	for nodeIterator.HasNext() {
		node := nodeIterator.Next()
		// skip over the node if the resource does not fit the node at all.
		if !node.nodeInfo.FitInNode(ask.AllocatedResource) {
			continue
		}
		alloc := sa.tryNode(node, ask)
		// allocation worked so return
		if alloc != nil {
			// check if the node was reserved for this ask: if it is set the result and return
			// NOTE: this is a safeguard as reserved nodes should never be part of the iterator
			// but we have no locking
			if _, ok := sa.reservations[reservationKey(node, nil, ask)]; ok {
				log.Logger().Debug("allocate found reserved ask during non reserved allocate",
					zap.String("appID", sa.ApplicationInfo.ApplicationID),
					zap.String("nodeID", node.NodeID),
					zap.String("allocationKey", allocKey))
				alloc.result = allocatedReserved
				return alloc
			}
			// we could also have a different node reserved for this ask if it has pick one of
			// the reserved nodes to unreserve (first one in the list)
			if len(reservedAsks) > 0 {
				nodeID := strings.TrimSuffix(reservedAsks[0], "|"+allocKey)
				log.Logger().Debug("allocate picking reserved ask during non reserved allocate",
					zap.String("appID", sa.ApplicationInfo.ApplicationID),
					zap.String("nodeID", nodeID),
					zap.String("allocationKey", allocKey))
				alloc.result = allocatedReserved
				alloc.reservedNodeID = nodeID
				return alloc
			}
			// nothing reserved just return this as a normal alloc
			alloc.result = allocated
			return alloc
		}
		// nothing allocated should we look at a reservation?
		// TODO make this smarter a hardcoded delay is not the right thing
		askAge := time.Since(ask.getCreateTime())
		if askAge > reservationDelay {
			log.Logger().Debug("app reservation check",
				zap.String("allocationKey", allocKey),
				zap.Time("createTime", ask.getCreateTime()),
				zap.Duration("askAge", askAge),
				zap.Duration("reservationDelay", reservationDelay))
			score := ask.AllocatedResource.FitInScore(node.getAvailableResource())
			// Record the so-far best node to reserve
			if score < scoreReserved {
				scoreReserved = score
				nodeToReserve = node
			}
		}
	}
	// we have not allocated yet, check if we should reserve
	// NOTE: the node should not be reserved as the iterator filters them but we do not lock the nodes
	if nodeToReserve != nil && !nodeToReserve.isReserved() {
		log.Logger().Debug("found candidate node for app reservation",
			zap.String("appID", sa.ApplicationInfo.ApplicationID),
			zap.String("nodeID", nodeToReserve.NodeID),
			zap.String("allocationKey", allocKey))
		// return allocation proposal and mark it as a reservation
		alloc := newSchedulingAllocation(ask, nodeToReserve.NodeID)
		alloc.result = reserved
		return alloc
	}
	// ask does not fit, skip to next ask
	return nil
}

// Try allocating on one specific node
func (sa *SchedulingApplication) tryNode(node *SchedulingNode, ask *schedulingAllocationAsk) *schedulingAllocation {
	allocKey := ask.AskProto.AllocationKey
	toAllocate := ask.AllocatedResource
	// create the key for the reservation
	if err := node.preAllocateCheck(toAllocate, reservationKey(nil, sa, ask), false); err != nil {
		// skip schedule onto node
		log.Logger().Debug("skipping node for allocation: basic condition not satisfied",
			zap.String("node", node.NodeID),
			zap.Any("allocationKey", allocKey),
			zap.Error(err))
		return nil
	}
	// skip the node if conditions can not be satisfied
	if !node.preAllocateConditions(allocKey) {
		return nil
	}
	// everything OK really allocate
	if node.allocateResource(toAllocate, false) {
		// before deciding on an allocation, call the reconcile plugin to sync scheduler cache
		// between core and shim if necessary. This is useful when running multiple allocations
		// in parallel and need to handle inter container affinity and anti-affinity.
		if rp := plugins.GetReconcilePlugin(); rp != nil {
			if err := rp.ReSyncSchedulerCache(&si.ReSyncSchedulerCacheArgs{
				AssumedAllocations: []*si.AssumedAllocation{
					{
						AllocationKey: allocKey,
						NodeID:        node.NodeID,
					},
				},
			}); err != nil {
				log.Logger().Error("failed to sync shim cache",
					zap.Error(err))
			}
		}
		// update the allocating resources
		sa.queue.incAllocatingResource(toAllocate)
		sa.allocating.AddTo(toAllocate)
		// mark this ask as allocating by lowering the repeat
		_, err := sa.updateAskRepeatInternal(ask, -1)
		if err != nil {
			log.Logger().Debug("ask repeat update failed unexpectedly",
				zap.Error(err))
		}

		// return allocation
		return newSchedulingAllocation(ask, node.NodeID)
	}
	return nil
}

// Recover the allocation for this app on the node provided.
// This is only called for recovering existing allocations on a node. We can not use the normal scheduling for this as
// the cache has already been updated and the allocation is confirmed. Checks for resource limits would fail. However
// the scheduler fakes a confirmation from the cache later and we thus need this to track correctly.
func (sa *SchedulingApplication) recoverOnNode(node *SchedulingNode, ask *schedulingAllocationAsk) {
	sa.Lock()
	defer sa.Unlock()
	toAllocate := ask.AllocatedResource
	// update the scheduling objects with the in progress resource
	node.incAllocatingResource(toAllocate)
	sa.queue.incAllocatingResource(toAllocate)
	sa.allocating.AddTo(toAllocate)
	// mark this ask as allocating by lowering the repeat
	if _, err := sa.updateAskRepeatInternal(ask, -1); err != nil {
		log.Logger().Error("application recovery update of existing allocation failed",
			zap.String("appID", sa.ApplicationInfo.ApplicationID),
			zap.String("allocKey", ask.AskProto.AllocationKey),
			zap.Error(err))
	}
}
