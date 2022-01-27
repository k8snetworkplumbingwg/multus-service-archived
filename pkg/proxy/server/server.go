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

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/k8snetworkplumbingwg/multus-service/pkg/proxy/controllers"
	proxyutils "github.com/k8snetworkplumbingwg/multus-service/pkg/proxy/utils"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/util/async"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilnode "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/utils/exec"
)

// Server structure defines data for server
type Server struct {
	podChanges         *controllers.PodChangeTracker
	endpointSliceCache *controllers.EndpointSliceCache
	serviceChanges     *controllers.ServiceChangeTracker

	mu               sync.Mutex // protects the following fields
	podMap           controllers.PodMap
	endpointsMap     controllers.EndpointsMap
	serviceMap       controllers.ServiceMap
	Client           clientset.Interface
	Hostname         string
	hostPrefix       string
	Broadcaster      record.EventBroadcaster
	Recorder         record.EventRecorder
	Options          *Options
	ConfigSyncPeriod time.Duration
	NodeRef          *v1.ObjectReference
	ip4Tables        utiliptables.Interface
	ip6Tables        utiliptables.Interface
	iptableBuffer    iptableBuffer

	// Since converting probabilities (floats) to strings is expensive
	// and we are using only probabilities in the format of 1/n, we are
	// precomputing some number of those and cache for future reuse.
	precomputedProbabilities []string

	initialized int32

	podSynced       bool
	serviceSynced   bool
	endpointsSynced bool

	podLister           corelisters.PodLister
	serviceLister       corelisters.ServiceLister
	endpointSliceLister discoverylisters.EndpointSliceLister

	syncRunner *async.BoundedFrequencyRunner
}

// LabelServiceProxyName specifies Kubernetes label for service proxy
const LabelServiceProxyName = "service.kubernetes.io/service-proxy-name"

// Run ...
func (s *Server) Run(hostname string) error {
	if s.Broadcaster != nil {
		s.Broadcaster.StartRecordingToSink(
			&v1core.EventSinkImpl{Interface: s.Client.CoreV1().Events("")})
	}

	informerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.ConfigSyncPeriod)
	s.podLister = informerFactory.Core().V1().Pods().Lister()
	s.serviceLister = informerFactory.Core().V1().Services().Lister()
	s.endpointSliceLister = informerFactory.Discovery().V1().EndpointSlices().Lister()

	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.ConfigSyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", hostname).String()
		}))

	podConfig := controllers.NewPodConfig(podInformerFactory.Core().V1().Pods(), s.ConfigSyncPeriod)
	podConfig.RegisterEventHandler(s)
	go podConfig.Run(wait.NeverStop)

	serviceConfig := controllers.NewServiceConfig(informerFactory.Core().V1().Services(), s.ConfigSyncPeriod)
	serviceConfig.RegisterEventHandler(s)
	go serviceConfig.Run(wait.NeverStop)

	proxyName, err := labels.NewRequirement(LabelServiceProxyName, selection.Equals, []string{"multus-proxy"})
	if err != nil {
		return err
	}
	managerName, err := labels.NewRequirement("endpointslice.kubernetes.io/managed-by", selection.Equals, []string{"multus-endpointslice-controller.npwg.k8s.io"})
	if err != nil {
		return err
	}
	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*proxyName, *managerName)
	epInformerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.ConfigSyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}))

	endpointSliceConfig := controllers.NewEndpointSliceConfig(epInformerFactory.Discovery().V1().EndpointSlices(), s.ConfigSyncPeriod)
	endpointSliceConfig.RegisterEventHandler(s)
	go endpointSliceConfig.Run(wait.NeverStop)

	epInformerFactory.Start(wait.NeverStop)
	informerFactory.Start(wait.NeverStop)
	podInformerFactory.Start(wait.NeverStop)

	s.birthCry()
	s.SyncLoop()
	return nil
}

func (s *Server) setInitialized(value bool) {
	var initialized int32
	if value {
		initialized = 1
	}
	atomic.StoreInt32(&s.initialized, initialized)
}

func (s *Server) isInitialized() bool {
	return atomic.LoadInt32(&s.initialized) > 0
}

func (s *Server) birthCry() {
	klog.Infof("Starting multus-proxy")
	s.Recorder.Eventf(s.NodeRef, api.EventTypeNormal, "Starting", "Starting multus-proxy.")
}

// SyncLoop ...
func (s *Server) SyncLoop() {
	s.syncRunner.Loop(wait.NeverStop)
}

// NewServer ...
func NewServer(o *Options) (*Server, error) {
	var kubeConfig *rest.Config
	var err error
	if len(o.Kubeconfig) == 0 {
		klog.Info("Neither kubeconfig file nor master URL was specified. Falling back to in-cluster config.")
		kubeConfig, err = rest.InClusterConfig()
	} else {
		kubeConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: o.Kubeconfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: o.master}},
		).ClientConfig()
	}
	if err != nil {
		return nil, err
	}

	if o.podIptables != "" {
		// cleanup current pod iptables directory if it exists
		if _, err := os.Stat(o.podIptables); err == nil || !os.IsNotExist(err) {
			err = os.RemoveAll(o.podIptables)
			if err != nil {
				return nil, err
			}
		}
		// create pod iptables directory
		err = os.Mkdir(o.podIptables, 0700)
		if err != nil {
			return nil, err
		}
	}

	client, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	hostname, err := utilnode.GetHostname(o.hostnameOverride)
	if err != nil {
		return nil, err
	}

	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		v1.EventSource{Component: "multus-proxy", Host: hostname})

	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      hostname,
		UID:       types.UID(hostname),
		Namespace: "",
	}

	//syncPeriod := 30 * time.Second
	syncPeriod := 30 * time.Second
	minSyncPeriod := 0 * time.Second
	burstSyncs := 2

	serviceChanges := controllers.NewServiceChangeTracker()
	if serviceChanges == nil {
		return nil, fmt.Errorf("cannot create service change tracker")
	}
	endpointSliceCache := controllers.NewEndpointSliceCache(hostname, v1.IPv4Protocol, recorder)
	if endpointSliceCache == nil {
		return nil, fmt.Errorf("cannot create endpoint slice cache")
	}
	podChanges := controllers.NewPodChangeTracker(o.containerRuntime, o.containerRuntimeEndpoint, hostname, o.hostPrefix)
	if podChanges == nil {
		return nil, fmt.Errorf("cannot create pod change tracker")
	}

	server := &Server{
		Options:          o,
		Client:           client,
		Hostname:         hostname,
		hostPrefix:       o.hostPrefix,
		Broadcaster:      eventBroadcaster,
		Recorder:         recorder,
		ConfigSyncPeriod: 15 * time.Minute,
		NodeRef:          nodeRef,
		ip4Tables:        utiliptables.New(exec.New(), utiliptables.ProtocolIPv4),
		ip6Tables:        utiliptables.New(exec.New(), utiliptables.ProtocolIPv6),
		iptableBuffer:    newIptableBuffer(),

		podChanges:         podChanges,
		endpointSliceCache: endpointSliceCache,
		serviceChanges:     serviceChanges,
		podMap:             make(controllers.PodMap),
		serviceMap:         make(controllers.ServiceMap),
		endpointsMap:       make(controllers.EndpointsMap),
	}
	server.syncRunner = async.NewBoundedFrequencyRunner(
		"sync-runner", server.syncServiceForwarding, minSyncPeriod, syncPeriod, burstSyncs)

	return server, nil
}

// Sync ...
func (s *Server) Sync() {
	klog.V(4).Infof("Sync Done!")
	s.OnPodSynced()
	s.syncRunner.Run()
}

// OnPodAdd ...
func (s *Server) OnPodAdd(pod *v1.Pod) {
	klog.V(4).Infof("OnPodUpdate")
	s.OnPodUpdate(nil, pod)
	s.syncServiceForwarding()
}

// OnPodUpdate ...
func (s *Server) OnPodUpdate(oldPod, pod *v1.Pod) {
	klog.V(4).Infof("OnPodUpdate")
	s.podChanges.Update(oldPod, pod)
	s.syncServiceForwarding()
}

// OnPodDelete ...
func (s *Server) OnPodDelete(pod *v1.Pod) {
	klog.V(4).Infof("OnPodDelete")
	s.OnPodUpdate(pod, nil)
	if proxyutils.CheckNodeNameIdentical(s.Hostname, pod.Spec.NodeName) {
		podIptables := fmt.Sprintf("%s/%s", s.Options.podIptables, pod.UID)
		if _, err := os.Stat(podIptables); err == nil {
			err := os.RemoveAll(podIptables)
			if err != nil {
				klog.Errorf("cannot remove pod dir(%s): %v", podIptables, err)
			}
		}
	}
	s.syncServiceForwarding()
}

// OnPodSynced ...
func (s *Server) OnPodSynced() {
	klog.V(4).Infof("OnPodSynced")
	s.mu.Lock()
	s.podSynced = true
	s.setInitialized(s.podSynced)
	s.mu.Unlock()

	s.syncServiceForwarding()
}

// OnEndpointSliceAdd ...
func (s *Server) OnEndpointSliceAdd(endpointSlice *discovery.EndpointSlice) {
	klog.V(4).Infof("OnEndpointSliceAdd")
	s.endpointSliceCache.EndpointSliceUpdate(endpointSlice, false)
	s.syncServiceForwarding()
}

// OnEndpointSliceUpdate ...
func (s *Server) OnEndpointSliceUpdate(oldEndpointSlice, endpointSlice *discovery.EndpointSlice) {
	klog.V(4).Infof("OnEndpointSliceUpdate")
	s.endpointSliceCache.EndpointSliceUpdate(endpointSlice, false)
	s.syncServiceForwarding()
}

// OnEndpointSliceDelete ...
func (s *Server) OnEndpointSliceDelete(endpointSlice *discovery.EndpointSlice) {
	klog.V(4).Infof("OnEndpointSliceDelete")
	s.endpointSliceCache.EndpointSliceUpdate(endpointSlice, true)
	s.syncServiceForwarding()
}

// OnEndpointSliceSynced ...
func (s *Server) OnEndpointSliceSynced() {
	klog.V(4).Infof("OnEndpointSliceSynced")
	s.mu.Lock()
	s.endpointsSynced = true
	s.setInitialized(s.endpointsSynced)
	s.mu.Unlock()

	s.syncServiceForwarding()
}

// OnServiceAdd ...
func (s *Server) OnServiceAdd(service *v1.Service) {
	klog.V(4).Infof("OnServiceAdd")

	s.serviceChanges.Update(nil, service)
	s.syncServiceForwarding()
}

// OnServiceUpdate ...
func (s *Server) OnServiceUpdate(oldService, service *v1.Service) {
	klog.V(4).Infof("OnServiceUpdate")
	s.serviceChanges.Update(oldService, service)
	s.syncServiceForwarding()
}

// OnServiceDelete ...
func (s *Server) OnServiceDelete(service *v1.Service) {
	klog.V(4).Infof("OnServiceDelete")
	s.serviceChanges.Update(service, nil)
	s.syncServiceForwarding()
}

// OnServiceSynced ...
func (s *Server) OnServiceSynced() {
	klog.V(4).Infof("OnServiceSynced")
	s.mu.Lock()
	s.serviceSynced = true
	s.setInitialized(s.serviceSynced)
	s.mu.Unlock()

	s.syncServiceForwarding()
}

func (s *Server) syncServiceForwarding() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.podSynced || !s.serviceSynced || !s.endpointsSynced {
		return
	}
	klog.V(4).Infof("syncServiceForwarding")
	s.podMap.Update(s.podChanges)
	s.serviceMap.Update(s.serviceChanges)
	s.endpointsMap.Update(s.endpointSliceCache)

	services, err := s.serviceLister.Services(metav1.NamespaceAll).List(labels.Everything())
	pods, err := s.podLister.Pods(metav1.NamespaceAll).List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to get pods: %v", err)
		return
	}

	for _, p := range pods {
		klog.V(5).Infof("SYNC %s/%s", p.Namespace, p.Name)
		if !controllers.IsTargetPod(p) {
			klog.V(5).Infof("SKIP SYNC (not active) %s/%s", p.Namespace, p.Name)
			continue
		}
		if !proxyutils.CheckNodeNameIdentical(s.Hostname, p.Spec.NodeName) {
			klog.V(5).Infof("SKIP SYNC (not host) %s/%s", p.Namespace, p.Name)
			continue
		}

		podInfo, err := s.podMap.GetPodInfo(p)
		if err != nil {
			klog.Errorf("cannot get %s/%s podInfo: %v", p.Namespace, p.Name, err)
			continue
		}

		if len(podInfo.Interfaces) < 2 {
			klog.V(5).Infof("SKIP SYNC (no multus interfaces) %s/%s", p.Namespace, p.Name)
			continue
		}
		netnsPath := podInfo.NetNSPath
		if s.hostPrefix != "" {
			netnsPath = fmt.Sprintf("%s/%s", s.hostPrefix, netnsPath)
		}

		netns, err := ns.GetNS(netnsPath)
		if err != nil {
			klog.Errorf("cannot get netns %v(%v): %v", netnsPath, netns, err)
			continue
		}
		klog.V(4).Infof("pod: %s/%s %s", p.Namespace, p.Name, netnsPath)

		_ = netns.Do(func(_ ns.NetNS) error {
			err := s.generateServiceForwardingRules(services, p, podInfo)
			s.backupIptablesRules(p, "current-service")
			return err
		})
	}
}

const (
	serviceChain = "MULTUS-SERVICES"
)

func servicePortEndpointChainName(servicePortName string, protocol string, endpoint string) string {
	hash := sha256.Sum256([]byte(servicePortName + protocol + endpoint))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "MULTUS-SEP-" + encoded[:16]
}

func (s *Server) generateServiceForwardingRules(services []*v1.Service, pod *v1.Pod, podInfo *controllers.PodInfo) error {
	klog.V(4).Infof("Generate rules for Pod :%v/%v\n", podInfo.Namespace, podInfo.Name)
	s.ip4Tables.EnsureChain(utiliptables.TableNAT, serviceChain)

	_, err := s.ip4Tables.EnsureRule(utiliptables.Prepend, utiliptables.TableNAT,
		"OUTPUT", // "-m", "conntrack", "--ctstate", "NEW",
		"-m", "comment", "--comment", "multus service portals", "-j", serviceChain)
	if err != nil {
		klog.Errorf("failed to add iptable rules: %v", err)
		return err
	}

	iptableBuffer := newIptableBuffer()
	iptableBuffer.Init(s.ip4Tables)

	for _, status := range podInfo.NetworkStatus {
		for svcPortName, svcInfo := range s.serviceMap {
			if status.Name == svcInfo.TargetNetwork && svcInfo.ClusterIP != "None" {
				_ = iptableBuffer.generateServicePortForwardingRules(s, &svcPortName, &svcInfo, &status)
			}
		}
	}

	if iptableBuffer.FinalizeRules() == true {
		/* store generated iptables rules if podIptables is enabled */
		if s.Options.podIptables != "" {
			filePath := fmt.Sprintf("%s/%s/multus_service.iptables", s.Options.podIptables, pod.UID)
			iptableBuffer.SaveRules(filePath)
		}
		if err := iptableBuffer.SyncRules(s.ip4Tables); err != nil {
			klog.Errorf("sync rules failed: %v", err)
			return err
		}
	}

	if len(iptableBuffer.OldClusterIPs) != 0 {
		for _, v := range iptableBuffer.OldClusterIPs {
			_, dst, err := net.ParseCIDR(v)
			if err != nil {
				klog.Errorf("failed to parse CIDR: %v", err)
				continue
			}
			err = netlink.RouteDel(&netlink.Route{
				Dst: dst,
			})
			if err != unix.ESRCH {
				klog.Errorf("failed to remove route: %v", err)
			}
		}
	}

	if len(iptableBuffer.NewClusterIPs) != 0 {
		klog.V(4).Infof("new clusterIP: %v\n", iptableBuffer.NewClusterIPs)
		for _, v := range iptableBuffer.NewClusterIPs {
			_, dst, err := net.ParseCIDR(v.ClusterIP)
			if err != nil {
				klog.Errorf("failed to parse CIDR: %v", err)
				continue
			}
			err = netlink.RouteAdd(&netlink.Route{
				Dst:       dst,
				LinkIndex: v.Link.Attrs().Index,
				Protocol:  unix.RTPROT_KERNEL,
				Src:       v.Src,
			})
			if err != nil {
				klog.Errorf("failed to add route: %v", err)
			}
		}
	}
	return nil
}

func (s *Server) backupIptablesRules(pod *v1.Pod, suffix string) error {
	// skip it if no podiptables option
	if s.Options.podIptables == "" {
		return nil
	}

	podIptables := fmt.Sprintf("%s/%s", s.Options.podIptables, pod.UID)
	// create directory for pod if not exist
	if _, err := os.Stat(podIptables); os.IsNotExist(err) {
		err := os.Mkdir(podIptables, 0700)
		if err != nil {
			klog.Errorf("cannot create pod dir (%s): %v", podIptables, err)
			return err
		}
	}
	file, err := os.Create(fmt.Sprintf("%s/%s.iptables", podIptables, suffix))
	defer file.Close()
	var buffer bytes.Buffer

	// store iptable result to file
	//XXX: need error handling? (see kube-proxy)
	err = s.ip4Tables.SaveInto(utiliptables.TableMangle, &buffer)
	err = s.ip4Tables.SaveInto(utiliptables.TableFilter, &buffer)
	err = s.ip4Tables.SaveInto(utiliptables.TableNAT, &buffer)
	_, err = buffer.WriteTo(file)

	return err
}

// This assumes s.mu is held
func (s *Server) precomputeProbabilities(numberOfPrecomputed int) {
	if len(s.precomputedProbabilities) == 0 {
		s.precomputedProbabilities = append(s.precomputedProbabilities, "<bad value>")
	}
	for i := len(s.precomputedProbabilities); i <= numberOfPrecomputed; i++ {
		s.precomputedProbabilities = append(s.precomputedProbabilities, computeProbability(i))
	}
}

// This assumes s.mu is held
func (s *Server) probability(n int) string {
	if n >= len(s.precomputedProbabilities) {
		s.precomputeProbabilities(n)
	}
	return s.precomputedProbabilities[n]
}

func computeProbability(n int) string {
	return fmt.Sprintf("%0.10f", 1.0/float64(n))
}
