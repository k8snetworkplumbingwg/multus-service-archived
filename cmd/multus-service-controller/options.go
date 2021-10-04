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
	"flag"

	"github.com/spf13/pflag"

	"k8s.io/klog/v2"
	utilnode "k8s.io/kubernetes/pkg/util/node"
)

// Options stores option for the command
type Options struct {
	// kubeconfig is the path to a KubeConfig file.
	Kubeconfig string
	// master is used to override the kubeconfig's URL to the apiserver
	master           string
	hostnameOverride string
	// errCh is the channel that errors will be sent
	errCh chan error
}

// AddFlags adds command line flags into command
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	klog.InitFlags(nil)
	fs.SortFlags = false
	fs.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "Path to kubeconfig file with authorization information (the master location is set by the master flag).")
	fs.StringVar(&o.master, "master", o.master, "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	fs.StringVar(&o.hostnameOverride, "hostname-override", o.hostnameOverride, "If non-empty, will use this string as identification instead of the actual hostname.")
	fs.AddGoFlagSet(flag.CommandLine)
}

// Validate will check command line options
func (o *Options) Validate() error {
	return nil
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

	stopCh := make(chan struct{})
	go func() {
		server.Run(5, stopCh)
		//o.errCh <- err
	}()

	for {
		err := <-o.errCh
		if err != nil {
			return err
		}
	}
}

// NewOptions initializes Options
func NewOptions() *Options {
	return &Options{
		errCh: make(chan error),
	}
}
