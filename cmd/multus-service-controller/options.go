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
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"k8s.io/client-go/tools/leaderelection"
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

	// enableLeaderElection is optional
	enableLeaderElection	bool
	// leaseNamespace is the namespace for lease object of leaderelection
	leaseNamespace string
	// leaseDuration...
	leaseDuration	int64
	// renewDeadline
	renewDeadline int64
	// retryPeriod
	retryPeriod int64
}

// AddFlags adds command line flags into command
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	klog.InitFlags(nil)
	fs.SortFlags = false
	fs.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "Path to kubeconfig file with authorization information (the master location is set by the master flag).")
	fs.StringVar(&o.master, "master", o.master, "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	fs.StringVar(&o.hostnameOverride, "hostname-override", o.hostnameOverride, "If non-empty, will use this string as identification instead of the actual hostname.")
	fs.BoolVar(&o.enableLeaderElection, "leaderelection", true, "Enable eladerelection.")
	fs.StringVar(&o.leaseNamespace, "lease-namespace", "kube-system", "Namespace for lease object")
	fs.Int64Var(&o.leaseDuration, "lease-duration", int64(15 * time.Second), "Lease duration of leaderelection.")
	fs.Int64Var(&o.renewDeadline, "renew-deadline", int64(10 * time.Second), "Renew Deadline of leaderelection.")
	fs.Int64Var(&o.retryPeriod, "retry-period", int64(2 * time.Second), "Retry period of leaderelection.")
	fs.AddGoFlagSet(flag.CommandLine)
}

// Validate will check command line options
func (o *Options) Validate() error {
	return nil
}

// Run invokes server
func (o *Options) Run() error {
	defer close(o.errCh)

	server, client, err := NewServer(o)
	if err != nil {
		return err
	}

	hostname, err := utilnode.GetHostname(o.hostnameOverride)
	if err != nil {
		return err
	}
	klog.Infof("hostname: %v", hostname)

	ctx := context.Background()
	stopCh := make(chan struct{})
	waitingForLeader := make(chan struct{})

	if o.enableLeaderElection {
		leaderElection, err := makeLeaderElectionConfig(o, client)
		if err != nil {
			return err
		}

		leaderElection.Callbacks = leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				close(waitingForLeader)
				server.Run(5, stopCh)
			},
			OnStoppedLeading: func() {
				select {
				case <-ctx.Done():
					// We were asked to terminate Exit 0.
					klog.Info("Requested to terminate. Exiting.")
					os.Exit(0)
				default:
					// We lost the lock.
					klog.Exitf("leaderelection lost")
				}
			},
		}

		leaderElector, eErr := leaderelection.NewLeaderElector(*leaderElection)
		if eErr != nil {
			return fmt.Errorf("cannot create leader elector: %v", eErr)
		}

		leaderElector.Run(ctx)
		return fmt.Errorf("lost lease")
	}

	server.Run(5, stopCh)
	/*
	for {
		err := <-o.errCh
		if err != nil {
			return err
		}
	}
	*/
	return nil
}

// NewOptions initializes Options
func NewOptions() *Options {
	return &Options{
	//	errCh: make(chan error),
	}
}
