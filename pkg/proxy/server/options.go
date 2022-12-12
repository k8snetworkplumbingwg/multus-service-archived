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
	"flag"

	"github.com/k8snetworkplumbingwg/multus-service/pkg/proxy/controllers"
	"github.com/spf13/pflag"

	"k8s.io/klog"
	utilnode "k8s.io/kubernetes/pkg/util/node"
)

// Options stores option for the command
type Options struct {
	// kubeconfig is the path to a KubeConfig file.
	Kubeconfig string
	// master is used to override the kubeconfig's URL to the apiserver
	master                   string
	hostnameOverride         string
	hostPrefix               string
	containerRuntime         controllers.RuntimeKind
	containerRuntimeEndpoint string
	podIptables              string
	hostNetwork              string
	// errCh is the channel that errors will be sent
	errCh chan error
	// stopCh is used to stop the command
	stopCh chan struct{}
}

// AddFlags adds command line flags into command
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	klog.InitFlags(nil)
	fs.SortFlags = false
	fs.Var(&o.containerRuntime, "container-runtime", "Container runtime using for the cluster. Possible values: 'cri'. ")
	fs.StringVar(&o.containerRuntimeEndpoint, "container-runtime-endpoint", "", "Path to cri socket.")
	fs.StringVar(&o.hostnameOverride, "hostname-override", "", "If non-empty, will use this string as identification instead of the actual hostname.")
	//example: ./multus-proxy --host-network namespace/net-attach-def:eth1
	fs.StringVar(&o.hostNetwork, "host-network", "", "If non-empty, configure iptable rules in host network namespace (arg: <net-attach-def>:<interface>).")
	fs.StringVar(&o.hostPrefix, "host-prefix", "", "If non-empty, will use this string as prefix for host filesystem.")
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "Path to kubeconfig file with authorization information (the master location is set by the master flag).")
	fs.StringVar(&o.master, "master", "", "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	fs.StringVar(&o.podIptables, "pod-iptables", "", "If non-empty, will use this path to store pod's iptables for troubleshooting helper.")
	fs.AddGoFlagSet(flag.CommandLine)
}

// Run invokes server
func (o *Options) Run() error {
	defer close(o.errCh)

	server, err := NewServer(o)
	if err != nil {
		return err
	}

	hostname, err := utilnode.GetHostname(o.hostnameOverride)
	if err != nil {
		return err
	}
	klog.Infof("hostname: %v", hostname)
	klog.Infof("container-runtime: %v", o.containerRuntime)

	return server.Run(hostname, o.stopCh)
}

// Stop halts the command
func (o *Options) Stop() {
	o.stopCh <- struct{}{}
}

// NewOptions initializes Options
func NewOptions() *Options {
	return &Options{
		containerRuntime: controllers.Cri,
		errCh:            make(chan error),
		stopCh:           make(chan struct{}),
	}
}
