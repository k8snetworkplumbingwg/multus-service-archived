/*
Copyright 2021 The Kubernetes Authors.

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

package controllers

import (
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	discinformers "k8s.io/client-go/informers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	utilproxy "k8s.io/kubernetes/pkg/proxy/util"
	utilnet "k8s.io/utils/net"
)

// EndpointSliceHandler is an abstract interface of objects which receive
// notifications about endpointSlice object changes.
type EndpointSliceHandler interface {
	// OnEndpointSliceAdd is called whenever creation of new object
	// is observed.
	OnEndpointSliceAdd(endpointSlice *discovery.EndpointSlice)
	// OnEndpointSliceUpdate is called whenever modification of an existing
	// object is observed.
	OnEndpointSliceUpdate(oldEndpointSlice, endpointSlice *discovery.EndpointSlice)
	// OnEndpointSliceDelete is called whenever deletion of an existing
	// object is observed.
	OnEndpointSliceDelete(endpointSlice *discovery.EndpointSlice)
	// OnEndpointSliceSynced is called once all the initial event handlers were
	// called and the state is fully propagated to local cache.
	OnEndpointSliceSynced()
}

// EndpointSliceConfig ...
type EndpointSliceConfig struct {
	listerSynced  cache.InformerSynced
	eventHandlers []EndpointSliceHandler
}

// NewEndpointSliceConfig ...
func NewEndpointSliceConfig(endpointSliceInformer discinformers.EndpointSliceInformer, resyncPeriod time.Duration) *EndpointSliceConfig {
	result := &EndpointSliceConfig{
		listerSynced: endpointSliceInformer.Informer().HasSynced,
	}

	endpointSliceInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    result.handleAddEndpointSlice,
			UpdateFunc: result.handleUpdateEndpointSlice,
			DeleteFunc: result.handleDeleteEndpointSlice,
		}, resyncPeriod,
	)

	return result
}

// RegisterEventHandler registers a handler which is called on every endpointSlice change.
func (c *EndpointSliceConfig) RegisterEventHandler(handler EndpointSliceHandler) {
	c.eventHandlers = append(c.eventHandlers, handler)
}

// Run ...
func (c *EndpointSliceConfig) Run(stopCh <-chan struct{}) {
	klog.Info("Starting EndpointSlice config controller")

	if !cache.WaitForNamedCacheSync("EndpointSlice config", stopCh, c.listerSynced) {
		return
	}

	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnEndpointSliceSynced()")
		c.eventHandlers[i].OnEndpointSliceSynced()
	}
}

func (c *EndpointSliceConfig) handleAddEndpointSlice(obj interface{}) {
	endpointSlice, ok := obj.(*discovery.EndpointSlice)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", obj))
		return
	}

	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnEndpointSliceAdd")
		c.eventHandlers[i].OnEndpointSliceAdd(endpointSlice)
	}
}

func (c *EndpointSliceConfig) handleUpdateEndpointSlice(oldObj, newObj interface{}) {
	oldEndpointSlice, ok := oldObj.(*discovery.EndpointSlice)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", oldObj))
		return
	}
	endpointSlice, ok := newObj.(*discovery.EndpointSlice)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", newObj))
		return
	}
	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnEndpointSliceUpdate")
		c.eventHandlers[i].OnEndpointSliceUpdate(oldEndpointSlice, endpointSlice)
	}
}

func (c *EndpointSliceConfig) handleDeleteEndpointSlice(obj interface{}) {
	endpointSlice, ok := obj.(*discovery.EndpointSlice)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", obj))
		}
		if endpointSlice, ok = tombstone.Obj.(*discovery.EndpointSlice); !ok {
			utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", obj))
			return
		}
	}
	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnEndpointSliceDelete")
		c.eventHandlers[i].OnEndpointSliceDelete(endpointSlice)
	}
}

// ServicePortName carries a namespace + name + portname.  This is the unique
// identifier for a load-balanced service.
type ServicePortName struct {
	types.NamespacedName
	Port     string
	Protocol v1.Protocol
}

func (spn ServicePortName) String() string {
	return fmt.Sprintf("%s%s", spn.NamespacedName.String(), fmtPortName(spn.Port))
}

// Endpoint in an interface which abstracts information about an endpoint.
type Endpoint interface {
	// String returns endpoint string.  An example format can be: `IP:Port`.
	// We take the returned value as ServiceEndpoint.Endpoint.
	String() string
	// GetIsLocal returns true if the endpoint is running in same host as kube-proxy, otherwise returns false.
	GetIsLocal() bool
	// IsReady returns true if an endpoint is ready and not terminating.
	// This is only set when watching EndpointSlices. If using Endpoints, this is always
	// true since only ready endpoints are read from Endpoints.
	IsReady() bool
	// IsServing returns true if an endpoint is ready. It does not account
	// for terminating state.
	// This is only set when watching EndpointSlices. If using Endpoints, this is always
	// true since only ready endpoints are read from Endpoints.
	IsServing() bool
	// IsTerminating retruns true if an endpoint is terminating. For pods,
	// that is any pod with a deletion timestamp.
	// This is only set when watching EndpointSlices. If using Endpoints, this is always
	// false since terminating endpoints are always excluded from Endpoints.
	IsTerminating() bool
	// GetZoneHint returns the zone hint for the endpoint. This is based on
	// endpoint.hints.forZones[0].name in the EndpointSlice API.
	GetZoneHints() sets.String
	// IP returns IP part of the endpoint.
	IP() string
	// Port returns the Port part of the endpoint.
	Port() (int, error)
	// Equal checks if two endpoints are equal.
	Equal(Endpoint) bool
}

// BaseEndpointInfo contains base information that defines an endpoint.
// This could be used directly by proxier while processing endpoints,
// or can be used for constructing a more specific EndpointInfo struct
// defined by the proxier if needed.
type BaseEndpointInfo struct {
	Endpoint string // TODO: should be an endpointString type
	// IsLocal indicates whether the endpoint is running in same host as kube-proxy.
	IsLocal bool

	// ZoneHints represent the zone hints for the endpoint. This is based on
	// endpoint.hints.forZones[*].name in the EndpointSlice API.
	ZoneHints sets.String
	// Ready indicates whether this endpoint is ready and NOT terminating.
	// For pods, this is true if a pod has a ready status and a nil deletion timestamp.
	// This is only set when watching EndpointSlices. If using Endpoints, this is always
	// true since only ready endpoints are read from Endpoints.
	// TODO: Ready can be inferred from Serving and Terminating below when enabled by default.
	Ready bool
	// Serving indiciates whether this endpoint is ready regardless of its terminating state.
	// For pods this is true if it has a ready status regardless of its deletion timestamp.
	// This is only set when watching EndpointSlices. If using Endpoints, this is always
	// true since only ready endpoints are read from Endpoints.
	Serving bool
	// Terminating indicates whether this endpoint is terminating.
	// For pods this is true if it has a non-nil deletion timestamp.
	// This is only set when watching EndpointSlices. If using Endpoints, this is always
	// false since terminating endpoints are always excluded from Endpoints.
	Terminating bool
}

var _ Endpoint = &BaseEndpointInfo{}

// String is part of proxy.Endpoint interface.
func (info *BaseEndpointInfo) String() string {
	return info.Endpoint
}

// GetIsLocal is part of proxy.Endpoint interface.
func (info *BaseEndpointInfo) GetIsLocal() bool {
	return info.IsLocal
}

// IsReady returns true if an endpoint is ready and not terminating.
func (info *BaseEndpointInfo) IsReady() bool {
	return info.Ready
}

// IsServing returns true if an endpoint is ready, regardless of if the
// endpoint is terminating.
func (info *BaseEndpointInfo) IsServing() bool {
	return info.Serving
}

// IsTerminating retruns true if an endpoint is terminating. For pods,
// that is any pod with a deletion timestamp.
func (info *BaseEndpointInfo) IsTerminating() bool {
	return info.Terminating
}

// GetZoneHints returns the zone hint for the endpoint.
func (info *BaseEndpointInfo) GetZoneHints() sets.String {
	return info.ZoneHints
}

// IP returns just the IP part of the endpoint, it's a part of proxy.Endpoint interface.
func (info *BaseEndpointInfo) IP() string {
	return utilproxy.IPPart(info.Endpoint)
}

// Port returns just the Port part of the endpoint.
func (info *BaseEndpointInfo) Port() (int, error) {
	return utilproxy.PortPart(info.Endpoint)
}

// Equal is part of proxy.Endpoint interface.
func (info *BaseEndpointInfo) Equal(other Endpoint) bool {
	return info.String() == other.String() && info.GetIsLocal() == other.GetIsLocal()
}

func newBaseEndpointInfo(IP string, port int, isLocal bool, topology map[string]string,
	ready, serving, terminating bool, zoneHints sets.String) *BaseEndpointInfo {
	return &BaseEndpointInfo{
		Endpoint:    net.JoinHostPort(IP, strconv.Itoa(port)),
		IsLocal:     isLocal,
		Ready:       ready,
		Serving:     serving,
		Terminating: terminating,
		ZoneHints:   zoneHints,
	}
}

// EndpointSliceCache is used as a cache of EndpointSlice information.
type EndpointSliceCache struct {
	// lock protects trackerByServiceMap.
	lock sync.Mutex

	// trackerByServiceMap is the basis of this cache. It contains endpoint
	// slice trackers grouped by service name and endpoint slice name. The first
	// key represents a namespaced service name while the second key represents
	// an endpoint slice name. Since endpoints can move between slices, we
	// require slice specific caching to prevent endpoints being removed from
	// the cache when they may have just moved to a different slice.
	trackerByServiceMap map[types.NamespacedName]*endpointSliceTracker

	hostname string
	ipFamily v1.IPFamily
	recorder record.EventRecorder
}

// endpointSliceTracker keeps track of EndpointSlices as they have been applied
// by a proxier along with any pending EndpointSlices that have been updated
// in this cache but not yet applied by a proxier.
type endpointSliceTracker struct {
	applied endpointSliceInfoByName
	pending endpointSliceInfoByName
}

// endpointSliceInfoByName groups endpointSliceInfo by the names of the
// corresponding EndpointSlices.
type endpointSliceInfoByName map[string]*endpointSliceInfo

// endpointSliceInfo contains just the attributes kube-proxy cares about.
// Used for caching. Intentionally small to limit memory util.
type endpointSliceInfo struct {
	Ports     []discovery.EndpointPort
	Endpoints []*endpointInfo
	Remove    bool
}

// endpointInfo contains just the attributes kube-proxy cares about.
// Used for caching. Intentionally small to limit memory util.
// Addresses and Topology are copied from EndpointSlice Endpoints.
type endpointInfo struct {
	Addresses []string
	NodeName  *string
	Topology  map[string]string
	ZoneHints sets.String

	Ready       bool
	Serving     bool
	Terminating bool
}

// spToEndpointMap stores groups Endpoint objects by ServicePortName and
// endpoint string (returned by Endpoint.String()).
type spToEndpointMap map[ServicePortName]map[string]Endpoint

// NewEndpointSliceCache initializes an EndpointSliceCache.
func NewEndpointSliceCache(hostname string, ipFamily v1.IPFamily, recorder record.EventRecorder) *EndpointSliceCache {
	return &EndpointSliceCache{
		trackerByServiceMap: map[types.NamespacedName]*endpointSliceTracker{},
		hostname:            hostname,
		ipFamily:            ipFamily,
		recorder:            recorder,
	}
}

// newEndpointSliceTracker initializes an endpointSliceTracker.
func newEndpointSliceTracker() *endpointSliceTracker {
	return &endpointSliceTracker{
		applied: endpointSliceInfoByName{},
		pending: endpointSliceInfoByName{},
	}
}

// newEndpointSliceInfo generates endpointSliceInfo from an EndpointSlice.
func newEndpointSliceInfo(endpointSlice *discovery.EndpointSlice, remove bool) *endpointSliceInfo {
	esInfo := &endpointSliceInfo{
		Ports:     make([]discovery.EndpointPort, len(endpointSlice.Ports)),
		Endpoints: []*endpointInfo{},
		Remove:    remove,
	}

	// copy here to avoid mutating shared EndpointSlice object.
	copy(esInfo.Ports, endpointSlice.Ports)
	sort.Sort(byPort(esInfo.Ports))

	if !remove {
		for _, endpoint := range endpointSlice.Endpoints {
			epInfo := &endpointInfo{
				Addresses: endpoint.Addresses,

				// conditions
				Ready:       endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready,
				Serving:     endpoint.Conditions.Serving == nil || *endpoint.Conditions.Serving,
				Terminating: endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating,
			}

			epInfo.NodeName = endpoint.NodeName

			esInfo.Endpoints = append(esInfo.Endpoints, epInfo)
		}

		sort.Sort(byAddress(esInfo.Endpoints))
	}

	return esInfo
}

// standardEndpointInfo is the default makeEndpointFunc.
func standardEndpointInfo(ep *BaseEndpointInfo) Endpoint {
	return ep
}

// EndpointSliceUpdate updates a pending slice in the cache.
func (cache *EndpointSliceCache) EndpointSliceUpdate(endpointSlice *discovery.EndpointSlice, remove bool) bool {
	serviceKey, sliceKey, err := endpointSliceCacheKeys(endpointSlice)
	if err != nil {
		klog.Warningf("Error getting endpoint slice cache keys: %v", err)
		return false
	}

	esInfo := newEndpointSliceInfo(endpointSlice, remove)

	cache.lock.Lock()
	defer cache.lock.Unlock()

	if _, ok := cache.trackerByServiceMap[serviceKey]; !ok {
		cache.trackerByServiceMap[serviceKey] = newEndpointSliceTracker()
	}

	changed := cache.esInfoChanged(serviceKey, sliceKey, esInfo)

	if changed {
		cache.trackerByServiceMap[serviceKey].pending[sliceKey] = esInfo
	}

	return changed
}

// checkoutChanges returns a list of all endpointsChanges that are
// pending and then marks them as applied.
func (cache *EndpointSliceCache) checkoutChanges() []*endpointsChange {
	changes := []*endpointsChange{}

	cache.lock.Lock()
	defer cache.lock.Unlock()

	for serviceNN, esTracker := range cache.trackerByServiceMap {
		if len(esTracker.pending) == 0 {
			continue
		}

		change := &endpointsChange{}

		change.previous = cache.getEndpointsMap(serviceNN, esTracker.applied)

		for name, sliceInfo := range esTracker.pending {
			if sliceInfo.Remove {
				delete(esTracker.applied, name)
			} else {
				esTracker.applied[name] = sliceInfo
			}

			delete(esTracker.pending, name)
		}

		change.current = cache.getEndpointsMap(serviceNN, esTracker.applied)
		changes = append(changes, change)
	}

	return changes
}

// getEndpointsMap computes an EndpointsMap for a given set of EndpointSlices.
func (cache *EndpointSliceCache) getEndpointsMap(serviceNN types.NamespacedName, sliceInfoByName endpointSliceInfoByName) EndpointsMap {
	endpointInfoBySP := cache.endpointInfoByServicePort(serviceNN, sliceInfoByName)
	return endpointsMapFromEndpointInfo(endpointInfoBySP)
}

// endpointInfoByServicePort groups endpoint info by service port name and address.
func (cache *EndpointSliceCache) endpointInfoByServicePort(serviceNN types.NamespacedName, sliceInfoByName endpointSliceInfoByName) spToEndpointMap {
	endpointInfoBySP := spToEndpointMap{}

	for _, sliceInfo := range sliceInfoByName {
		for _, port := range sliceInfo.Ports {
			if port.Name == nil {
				klog.Warningf("ignoring port with nil name %v", port)
				continue
			}
			// TODO: handle nil ports to mean "all"
			if port.Port == nil || *port.Port == int32(0) {
				klog.Warningf("ignoring invalid endpoint port %s", *port.Name)
				continue
			}

			svcPortName := ServicePortName{
				NamespacedName: serviceNN,
				Port:           *port.Name,
				Protocol:       *port.Protocol,
			}

			endpointInfoBySP[svcPortName] = cache.addEndpoints(serviceNN, int(*port.Port), endpointInfoBySP[svcPortName], sliceInfo.Endpoints)
		}
	}

	return endpointInfoBySP
}

// addEndpoints adds endpointInfo for each unique endpoint.
func (cache *EndpointSliceCache) addEndpoints(serviceNN types.NamespacedName, portNum int, endpointSet map[string]Endpoint, endpoints []*endpointInfo) map[string]Endpoint {
	if endpointSet == nil {
		endpointSet = map[string]Endpoint{}
	}

	// iterate through endpoints to add them to endpointSet.
	for _, endpoint := range endpoints {
		if len(endpoint.Addresses) == 0 {
			klog.Warningf("ignoring invalid endpoint port %s with empty addresses", endpoint)
			continue
		}

		// Filter out the incorrect IP version case. Any endpoint port that
		// contains incorrect IP version will be ignored.
		if (cache.ipFamily == v1.IPv6Protocol) != utilnet.IsIPv6String(endpoint.Addresses[0]) {
			// Emit event on the corresponding service which had a different IP
			// version than the endpoint.
			//utilproxy.LogAndEmitIncorrectIPVersionEvent(cache.recorder, "endpointslice", endpoint.Addresses[0], serviceNN.Namespace, serviceNN.Name, "")
			continue
		}

		isLocal := false
		if endpoint.NodeName != nil {
			isLocal = cache.isLocal(*endpoint.NodeName)
		} else {
			isLocal = cache.isLocal(endpoint.Topology[v1.LabelHostname])
		}

		endpointInfo := newBaseEndpointInfo(endpoint.Addresses[0], portNum, isLocal, endpoint.Topology,
			endpoint.Ready, endpoint.Serving, endpoint.Terminating, endpoint.ZoneHints)

		// This logic ensures we're deduplicating potential overlapping endpoints
		// isLocal should not vary between matching endpoints, but if it does, we
		// favor a true value here if it exists.
		if _, exists := endpointSet[endpointInfo.String()]; !exists || isLocal {
			endpointSet[endpointInfo.String()] = endpointInfo
		}
	}

	return endpointSet
}

func (cache *EndpointSliceCache) isLocal(hostname string) bool {
	return len(cache.hostname) > 0 && hostname == cache.hostname
}

// esInfoChanged returns true if the esInfo parameter should be set as a new
// pending value in the cache.
func (cache *EndpointSliceCache) esInfoChanged(serviceKey types.NamespacedName, sliceKey string, esInfo *endpointSliceInfo) bool {
	if _, ok := cache.trackerByServiceMap[serviceKey]; ok {
		appliedInfo, appliedOk := cache.trackerByServiceMap[serviceKey].applied[sliceKey]
		pendingInfo, pendingOk := cache.trackerByServiceMap[serviceKey].pending[sliceKey]

		// If there's already a pending value, return whether or not this would
		// change that.
		if pendingOk {
			return !reflect.DeepEqual(esInfo, pendingInfo)
		}

		// If there's already an applied value, return whether or not this would
		// change that.
		if appliedOk {
			return !reflect.DeepEqual(esInfo, appliedInfo)
		}
	}

	// If this is marked for removal and does not exist in the cache, no changes
	// are necessary.
	if esInfo.Remove {
		return false
	}

	// If not in the cache, and not marked for removal, it should be added.
	return true
}

// endpointsMapFromEndpointInfo computes an endpointsMap from endpointInfo that
// has been grouped by service port and IP.
func endpointsMapFromEndpointInfo(endpointInfoBySP map[ServicePortName]map[string]Endpoint) EndpointsMap {
	endpointsMap := EndpointsMap{}

	// transform endpointInfoByServicePort into an endpointsMap with sorted IPs.
	for svcPortName, endpointSet := range endpointInfoBySP {
		if len(endpointSet) > 0 {
			endpointsMap[svcPortName] = []Endpoint{}
			for _, endpointInfo := range endpointSet {
				endpointsMap[svcPortName] = append(endpointsMap[svcPortName], endpointInfo)

			}
			// Ensure endpoints are always returned in the same order to simplify diffing.
			sort.Sort(byEndpoint(endpointsMap[svcPortName]))

			klog.V(4).Infof("Setting endpoints for %q to %+v", svcPortName, formatEndpointsList(endpointsMap[svcPortName]))
		}
	}

	return endpointsMap
}

// formatEndpointsList returns a string list converted from an endpoints list.
func formatEndpointsList(endpoints []Endpoint) []string {
	var formattedList []string
	for _, ep := range endpoints {
		formattedList = append(formattedList, ep.String())
	}
	return formattedList
}

// endpointSliceCacheKeys returns cache keys used for a given EndpointSlice.
func endpointSliceCacheKeys(endpointSlice *discovery.EndpointSlice) (types.NamespacedName, string, error) {
	var err error
	serviceName, ok := endpointSlice.Labels[discovery.LabelServiceName]
	if !ok || serviceName == "" {
		err = fmt.Errorf("No %s label set on endpoint slice: %s", discovery.LabelServiceName, endpointSlice.Name)
	} else if endpointSlice.Namespace == "" || endpointSlice.Name == "" {
		err = fmt.Errorf("Expected EndpointSlice name and namespace to be set: %v", endpointSlice)
	}
	return types.NamespacedName{Namespace: endpointSlice.Namespace, Name: serviceName}, endpointSlice.Name, err
}

// byAddress helps sort endpointInfo
type byAddress []*endpointInfo

func (e byAddress) Len() int {
	return len(e)
}
func (e byAddress) Swap(i, j int) {
	e[i], e[j] = e[j], e[i]
}
func (e byAddress) Less(i, j int) bool {
	return strings.Join(e[i].Addresses, ",") < strings.Join(e[j].Addresses, ",")
}

// byEndpoint helps sort endpoints by endpoint string.
type byEndpoint []Endpoint

func (e byEndpoint) Len() int {
	return len(e)
}
func (e byEndpoint) Swap(i, j int) {
	e[i], e[j] = e[j], e[i]
}
func (e byEndpoint) Less(i, j int) bool {
	return e[i].String() < e[j].String()
}

// byPort helps sort EndpointSlice ports by port number
type byPort []discovery.EndpointPort

func (p byPort) Len() int {
	return len(p)
}
func (p byPort) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}
func (p byPort) Less(i, j int) bool {
	return *p[i].Port < *p[j].Port
}

// endpointsChange contains all changes to endpoints that happened since proxy
// rules were synced.  For a single object, changes are accumulated, i.e.
// previous is state from before applying the changes, current is state after
// applying the changes.
type endpointsChange struct {
	previous EndpointsMap
	current  EndpointsMap
}

// ServiceEndpoint is used to identify a service and one of its endpoint pair.
type ServiceEndpoint struct {
	Endpoint        string
	ServicePortName ServicePortName
}

// UpdateEndpointMapResult is the updated results after applying endpoints changes.
type UpdateEndpointMapResult struct {
	// HCEndpointsLocalIPSize maps an endpoints name to the length of its local IPs.
	HCEndpointsLocalIPSize map[types.NamespacedName]int
	// StaleEndpoints identifies if an endpoints service pair is stale.
	StaleEndpoints []ServiceEndpoint
	// StaleServiceNames identifies if a service is stale.
	StaleServiceNames []ServicePortName
	// List of the trigger times for all endpoints objects that changed. It's used to export the
	// network programming latency.
	// NOTE(oxddr): this can be simplified to []time.Time if memory consumption becomes an issue.
	LastChangeTriggerTimes map[types.NamespacedName][]time.Time
}

// Update updates endpointsMap base on the given changes.
func (em EndpointsMap) Update(changes *EndpointSliceCache) (result UpdateEndpointMapResult) {
	result.StaleEndpoints = make([]ServiceEndpoint, 0)
	result.StaleServiceNames = make([]ServicePortName, 0)
	result.LastChangeTriggerTimes = make(map[types.NamespacedName][]time.Time)

	em.apply(
		changes, &result.StaleEndpoints, &result.StaleServiceNames, &result.LastChangeTriggerTimes)

	// TODO: If this will appear to be computationally expensive, consider
	// computing this incrementally similarly to endpointsMap.
	result.HCEndpointsLocalIPSize = make(map[types.NamespacedName]int)
	localIPs := em.getLocalReadyEndpointIPs()
	for nsn, ips := range localIPs {
		result.HCEndpointsLocalIPSize[nsn] = len(ips)
	}

	return result
}

// EndpointsMap maps a service name to a list of all its Endpoints.
type EndpointsMap map[ServicePortName][]Endpoint

// apply the changes to EndpointsMap and updates stale endpoints and service-endpoints pair. The `staleEndpoints` argument
// is passed in to store the stale udp endpoints and `staleServiceNames` argument is passed in to store the stale udp service.
// The changes map is cleared after applying them.
// In addition it returns (via argument) and resets the lastChangeTriggerTimes for all endpoints
// that were changed and will result in syncing the proxy rules.
// apply triggers processEndpointsMapChange on every change.
func (em EndpointsMap) apply(ect *EndpointSliceCache, staleEndpoints *[]ServiceEndpoint,
	staleServiceNames *[]ServicePortName, lastChangeTriggerTimes *map[types.NamespacedName][]time.Time) {
	if ect == nil {
		return
	}

	changes := ect.checkoutChanges()
	for _, change := range changes {
		em.unmerge(change.previous)
		em.merge(change.current)
		detectStaleConnections(change.previous, change.current, staleEndpoints, staleServiceNames)
	}
}

// Merge ensures that the current EndpointsMap contains all <service, endpoints> pairs from the EndpointsMap passed in.
func (em EndpointsMap) merge(other EndpointsMap) {
	for svcPortName := range other {
		em[svcPortName] = other[svcPortName]
	}
}

// Unmerge removes the <service, endpoints> pairs from the current EndpointsMap which are contained in the EndpointsMap passed in.
func (em EndpointsMap) unmerge(other EndpointsMap) {
	for svcPortName := range other {
		delete(em, svcPortName)
	}
}

// GetLocalEndpointIPs returns endpoints IPs if given endpoint is local - local means the endpoint is running in same host as kube-proxy.
func (em EndpointsMap) getLocalReadyEndpointIPs() map[types.NamespacedName]sets.String {
	localIPs := make(map[types.NamespacedName]sets.String)
	for svcPortName, epList := range em {
		for _, ep := range epList {
			// Only add ready endpoints for health checking. Terminating endpoints may still serve traffic
			// but the health check signal should fail if there are only terminating endpoints on a node.
			if !ep.IsReady() {
				continue
			}

			if ep.GetIsLocal() {
				nsn := svcPortName.NamespacedName
				if localIPs[nsn] == nil {
					localIPs[nsn] = sets.NewString()
				}
				localIPs[nsn].Insert(ep.IP())
			}
		}
	}
	return localIPs
}

// detectStaleConnections modifies <staleEndpoints> and <staleServices> with detected stale connections. <staleServiceNames>
// is used to store stale udp service in order to clear udp conntrack later.
func detectStaleConnections(oldEndpointsMap, newEndpointsMap EndpointsMap, staleEndpoints *[]ServiceEndpoint, staleServiceNames *[]ServicePortName) {
	for svcPortName, epList := range oldEndpointsMap {
		if svcPortName.Protocol != v1.ProtocolUDP {
			continue
		}

		for _, ep := range epList {
			stale := true
			for i := range newEndpointsMap[svcPortName] {
				if newEndpointsMap[svcPortName][i].Equal(ep) {
					stale = false
					break
				}
			}
			if stale {
				klog.V(4).Infof("Stale endpoint %v -> %v", svcPortName, ep.String())
				*staleEndpoints = append(*staleEndpoints, ServiceEndpoint{Endpoint: ep.String(), ServicePortName: svcPortName})
			}
		}
	}

	for svcPortName, epList := range newEndpointsMap {
		if svcPortName.Protocol != v1.ProtocolUDP {
			continue
		}

		// For udp service, if its backend changes from 0 to non-0. There may exist a conntrack entry that could blackhole traffic to the service.
		if len(epList) > 0 && len(oldEndpointsMap[svcPortName]) == 0 {
			*staleServiceNames = append(*staleServiceNames, svcPortName)
		}
	}
}
