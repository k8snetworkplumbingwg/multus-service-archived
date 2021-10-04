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
	"time"

	"github.com/k8snetworkplumbingwg/multus-service/pkg/service-controller"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
)

func NewServer(o *Options) (*endpointslice.Controller, error) {
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

	client, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	syncPeriod := 30 * time.Second
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, syncPeriod)

	controller := endpointslice.NewController(
		informerFactory.Core().V1().Pods(),
		informerFactory.Core().V1().Services(),
		informerFactory.Core().V1().Nodes(),
		informerFactory.Discovery().V1().EndpointSlices(),
		100, //maxEndpointsPerSlice
		client, syncPeriod)

	informerFactory.Start(wait.NeverStop)

	return controller, nil
}
