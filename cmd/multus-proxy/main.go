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

// This is a 'multus-proxy'. This generates iptables rules for
// multus service dataplane.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/k8snetworkplumbingwg/multus-service/pkg/proxy/server"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const logFlushFreqFlagName = "log-flush-frequency"

var logFlushFreq = pflag.Duration(logFlushFreqFlagName, 5*time.Second, "Maximum number of seconds between log flushes")

// KlogWriter serves as a bridge between the standard log package and the glog package.
type KlogWriter struct{}

// Write implements the io.Writer interface.
func (writer KlogWriter) Write(data []byte) (n int, err error) {
	klog.InfoDepth(1, string(data))
	return len(data), nil
}

func initLogs() {
	log.SetOutput(KlogWriter{})
	log.SetFlags(0)
	go wait.Forever(klog.Flush, *logFlushFreq)
}

func main() {
	initLogs()
	defer klog.Flush()
	opts := server.NewOptions()

	cmd := &cobra.Command{
		Use:  "multus-proxy",
		Long: `TBD`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := opts.Run(); err != nil {
				klog.Exit(err)
			}
		},
	}
	opts.AddFlags(cmd.Flags())

	signalCh := make(chan os.Signal, 16)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range signalCh {
			klog.Infof("Caught %v, stopping..", sig)
			opts.Stop()
		}
	}()

	klog.Infof("Executing...")

	if err := cmd.Execute(); err != nil {
		klog.Errorf("Execute failed: %v", err)
		os.Exit(1)
	}
}
