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
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

// ServiceHandler is an abstract interface of objects which receive
// notifications about service object changes.
type ServiceHandler interface {
	// OnServiceAdd is called whenever creation of new object
	// is observed.
	OnServiceAdd(service *v1.Service)
	// OnServiceUpdate is called whenever modification of an existing
	// object is observed.
	OnServiceUpdate(oldService, service *v1.Service)
	// OnServiceDelete is called whenever deletion of an existing
	// object is observed.
	OnServiceDelete(service *v1.Service)
	// OnServiceSynced is called once all the initial event handlers were
	// called and the state is fully propagated to local cache.
	OnServiceSynced()
}

// ServiceConfig ...
type ServiceConfig struct {
	listerSynced  cache.InformerSynced
	eventHandlers []ServiceHandler
}

// ServiceConfig ...
func NewServiceConfig(serviceInformer coreinformers.ServiceInformer, resyncPeriod time.Duration) *ServiceConfig {
	result := &ServiceConfig{
		listerSynced: serviceInformer.Informer().HasSynced,
	}

	serviceInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    result.handleAddService,
			UpdateFunc: result.handleUpdateService,
			DeleteFunc: result.handleDeleteService,
		}, resyncPeriod,
	)

	return result
}

// RegisterEventHandler registers a handler which is called on every service change.
func (c *ServiceConfig) RegisterEventHandler(handler ServiceHandler) {
	c.eventHandlers = append(c.eventHandlers, handler)
}

// Run ...
func (c *ServiceConfig) Run(stopCh <-chan struct{}) {
	klog.Info("Starting Service config controller")

	if !cache.WaitForNamedCacheSync("Service config", stopCh, c.listerSynced) {
		return
	}

	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnServiceSynced()")
		c.eventHandlers[i].OnServiceSynced()
	}
}

func (c *ServiceConfig) handleAddService(obj interface{}) {
	service, ok := obj.(*v1.Service)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", obj))
		return
	}

	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnServiceAdd")
		c.eventHandlers[i].OnServiceAdd(service)
	}
}

func (c *ServiceConfig) handleUpdateService(oldObj, newObj interface{}) {
	oldService, ok := oldObj.(*v1.Service)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", oldObj))
		return
	}
	service, ok := newObj.(*v1.Service)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", newObj))
		return
	}
	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnServiceUpdate")
		c.eventHandlers[i].OnServiceUpdate(oldService, service)
	}
}

func (c *ServiceConfig) handleDeleteService(obj interface{}) {
	service, ok := obj.(*v1.Service)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", obj))
		}
		if service, ok = tombstone.Obj.(*v1.Service); !ok {
			utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", obj))
			return
		}
	}
	for i := range c.eventHandlers {
		klog.V(4).Infof("Calling handler.OnServiceDelete")
		c.eventHandlers[i].OnServiceDelete(service)
	}
}

// portProtoHash takes the ServicePortName and protocol for a service
// returns the associated 16 character hash. This is computed by hashing (sha256)
// then encoding to base32 and truncating to 16 chars. We do this because IPTables
// Chain Names must be <= 28 chars long, and the longer they are the harder they are to read.
func portProtoHash(servicePortName string, protocol string) string {
	hash := sha256.Sum256([]byte(servicePortName + protocol))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return encoded[:16]
}

func fmtPortName(in string) string {
	if in == "" {
		return ""
	}
	return fmt.Sprintf(":%s", in)
}

// ServicePortInfo contains information that defines a object.
type ServicePortInfo struct {
	Name          string
	ClusterIP     string
	TargetNetwork string
	Port          int
	TargetPort    intstr.IntOrString
	Protocol      string
	ChainName     string
}

// ServiceMap ...
//type ServiceMap map[types.NamespacedName]ServiceInfo
type ServiceMap map[ServicePortName]ServicePortInfo

// Update ...
func (n *ServiceMap) Update(changes *ServiceChangeTracker) {
	if n != nil {
		n.apply(changes)
	}
}

func (n *ServiceMap) apply(changes *ServiceChangeTracker) {
	if n == nil || changes == nil {
		return
	}

	changes.lock.Lock()
	defer changes.lock.Unlock()
	for _, change := range changes.items {
		n.unmerge(change.previous)
		n.merge(change.current)
	}
	// clear changes after applying them to ServiceMap.
	changes.items = make(map[types.NamespacedName]*serviceChange)
	return
}

func (n *ServiceMap) merge(other ServiceMap) {
	for serviceName, info := range other {
		(*n)[serviceName] = info
	}
}

func (n *ServiceMap) unmerge(other ServiceMap) {
	for serviceName := range other {
		delete(*n, serviceName)
	}
}

type serviceChange struct {
	previous ServiceMap
	current  ServiceMap
}

// ServiceChangeTracker ...
type ServiceChangeTracker struct {
	// lock protects items.
	lock sync.Mutex
	// items maps a service to its serviceChange.
	items      map[types.NamespacedName]*serviceChange
	serviceMap ServiceMap
}

func (sct *ServiceChangeTracker) newServicePortInfo(service *v1.Service, port *v1.ServicePort, svcPortName *ServicePortName, serviceNetwork string) *ServicePortInfo {
	protocol := strings.ToLower(string(port.Protocol))
	name := svcPortName.String()
	portName := &ServicePortInfo{
		Name:          name,
		ClusterIP:     service.Spec.ClusterIP,
		TargetNetwork: serviceNetwork,
		Port:          int(port.Port),
		TargetPort:    port.TargetPort,
		Protocol:      protocol,
		ChainName:     fmt.Sprintf("MULTUS-SVC-%s", portProtoHash(name, protocol)),
	}
	return portName
}

func (sct *ServiceChangeTracker) serviceToServiceMap(service *v1.Service) ServiceMap {
	if service == nil {
		return nil
	}
	serviceMap := make(ServiceMap)
	serviceNetwork, ok := service.Annotations["k8s.v1.cni.cncf.io/service-network"]
	if !ok {
		return nil
	}
	if strings.Index(serviceNetwork, "/") == -1 {
		serviceNetwork = fmt.Sprintf("%s/%s", service.Namespace, strings.ReplaceAll(serviceNetwork, " ", ""))
	}

	serviceName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	for _, port := range service.Spec.Ports {
		svcPortName := ServicePortName{
			NamespacedName: serviceName,
			Port:           port.Name,
			Protocol:       port.Protocol,
		}
		servicePortInfo := sct.newServicePortInfo(service, &port, &svcPortName, serviceNetwork)
		serviceMap[svcPortName] = *servicePortInfo
	}
	return serviceMap
}

// Update ...
func (sct *ServiceChangeTracker) Update(previous, current *v1.Service) bool {
	service := current

	if sct == nil {
		return false
	}
	if service == nil {
		service = previous
	}
	if service == nil {
		return false
	}

	namespacedName := types.NamespacedName{Namespace: service.Namespace, Name: service.Name}

	sct.lock.Lock()
	defer sct.lock.Unlock()

	change, exists := sct.items[namespacedName]
	if !exists {
		change = &serviceChange{}
		prevServiceMap := sct.serviceToServiceMap(previous)
		change.previous = prevServiceMap
		sct.items[namespacedName] = change
	}

	curServiceMap := sct.serviceToServiceMap(current)
	change.current = curServiceMap
	if reflect.DeepEqual(change.previous, change.current) {
		delete(sct.items, namespacedName)
	}

	return len(sct.items) >= 0
}

// NewServiceChangeTracker ...
func NewServiceChangeTracker() *ServiceChangeTracker {
	return &ServiceChangeTracker{
		items:      make(map[types.NamespacedName]*serviceChange),
		serviceMap: make(ServiceMap),
	}
}
