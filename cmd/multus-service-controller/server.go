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

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"github.com/k8snetworkplumbingwg/multus-service/pkg/service-controller"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
)

// NamespacePath is namespace file of service account
const NamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func makeLeaderElectionConfig(o *Options, client clientset.Interface) (*leaderelection.LeaderElectionConfig, error) {
	if !o.enableLeaderElection {
		return nil, nil
	}

	// Get namespace for lease object.
	leaseNamespace := o.leaseNamespace

	// If no leaseNamespace, then try to get service account namespace (same as pod)
	if leaseNamespace == "" {
		_, err := os.Stat(NamespacePath)
		if err != nil {
			return nil, fmt.Errorf("unable to get leaseNamespace")
		}
		namespaceBytes, err := ioutil.ReadFile(NamespacePath)
		if err != nil {
			return nil, fmt.Errorf("unable to get service account namespace: %v", err)
		}
		leaseNamespace = string(namespaceBytes)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("unable to get hostname: %v", err)
	}

	id := fmt.Sprintf("%s-%s", hostname, string(uuid.NewUUID()))

	return &leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name: "multus-service-controller",
				Namespace: leaseNamespace,
			},
			Client: client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: id,
			},
		},
		LeaseDuration: time.Duration(o.leaseDuration),
		RenewDeadline: time.Duration(o.renewDeadline),
		RetryPeriod: time.Duration(o.retryPeriod),
		WatchDog: leaderelection.NewLeaderHealthzAdaptor(time.Second * 20),
		Name: "multus-service-controller",
		ReleaseOnCancel: true,
	}, nil
}

// NewServer ...
func NewServer(o *Options) (*endpointslice.Controller, clientset.Interface, error) {
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
		return nil, nil, err
	}

	client, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, client, err
	}

	syncPeriod := 5 * time.Second
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, syncPeriod)

	proxyName, err := labels.NewRequirement(endpointslice.LabelServiceProxyName, selection.Equals, []string{"multus-proxy"})
	if err != nil {
		return nil, client, err
	}
	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*proxyName)
	svcInformerFactory := informers.NewSharedInformerFactoryWithOptions(client, syncPeriod,
	informers.WithTweakListOptions(func(options *metav1.ListOptions) {
		options.LabelSelector = labelSelector.String()
        }))

	controller := endpointslice.NewController(
		informerFactory.Core().V1().Pods(),
		svcInformerFactory.Core().V1().Services(),
		informerFactory.Core().V1().Nodes(),
		informerFactory.Discovery().V1().EndpointSlices(),
		100, //maxEndpointsPerSlice
		client, syncPeriod)
	informerFactory.Start(wait.NeverStop)
	svcInformerFactory.Start(wait.NeverStop)

	return controller, client, nil
}
