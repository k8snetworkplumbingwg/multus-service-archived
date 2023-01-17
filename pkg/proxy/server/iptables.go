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
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/k8snetworkplumbingwg/multus-service/pkg/proxy/controllers"
	"github.com/vishvananda/netlink"

	netdefv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"k8s.io/klog"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
)

// PolicyNetworkAnnotation is annotation for multiNetworkPolicy,
// to specify which networks(i.e. net-attach-def) are the targets
// of the policy
const PolicyNetworkAnnotation = "k8s.v1.cni.cncf.io/policy-for"

type clusterIPRoute struct {
	ClusterIP string
	Src       net.IP
	Link      netlink.Link
}

type iptableBuffer struct {
	currentNAT    map[utiliptables.Chain][]byte
	currentChain  map[utiliptables.Chain]bool
	activeChain   map[utiliptables.Chain]bool
	natRules      *bytes.Buffer // contains everything below
	natChains     *bytes.Buffer // chain definition
	sepRules      *bytes.Buffer // chain for MULTUS-SEP-...
	svcRules      *bytes.Buffer // chain for MULTUS-SVC-...
	topRules      *bytes.Buffer // MULTUS-SERVICES and chain deletion ...
	clusterIPs    map[string]string
	NewClusterIPs []clusterIPRoute
	OldClusterIPs []string
}

func newIptableBuffer() iptableBuffer {
	buf := iptableBuffer{
		currentNAT:    make(map[utiliptables.Chain][]byte),
		currentChain:  map[utiliptables.Chain]bool{},
		natChains:     bytes.NewBuffer(make([]byte, 0, 2048)),
		natRules:      bytes.NewBuffer(make([]byte, 0, 8192)),
		sepRules:      bytes.NewBuffer(make([]byte, 0, 8192)),
		svcRules:      bytes.NewBuffer(make([]byte, 0, 8192)),
		topRules:      bytes.NewBuffer(make([]byte, 0, 8192)),
		activeChain:   map[utiliptables.Chain]bool{},
		clusterIPs:    map[string]string{},
		OldClusterIPs: []string{},
		NewClusterIPs: []clusterIPRoute{},
	}
	return buf
}

func (ipt *iptableBuffer) Init(iptables utiliptables.Interface) {
	tmpbuf := bytes.NewBuffer(nil)
	tmpbuf.Reset()
	tmpbuf2 := bytes.NewBuffer(nil)
	tmpbuf2.Reset()

	buf := bytes.NewBuffer(nil)
	buf.Reset()
	err := iptables.SaveInto(utiliptables.TableNAT, buf)
	if err != nil {
		klog.Error("failed to get iptable nat")
		return
	}

	w := io.MultiWriter(tmpbuf, tmpbuf2)
	io.Copy(w, buf)

	// Get current chain name into currentChain
	ipt.currentNAT = utiliptables.GetChainLines(utiliptables.TableNAT, tmpbuf.Bytes())
	for k := range ipt.currentNAT {
		if strings.HasPrefix(string(k), "MULTUS-") {
			ipt.currentChain[k] = true
		}
	}

	s := bufio.NewScanner(tmpbuf2)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), "-A MULTUS-SERVICES") {
			chainStrings := strings.Split(s.Text(), " ")
			chainName := chainStrings[17]
			clusterIP := chainStrings[3]
			ipt.clusterIPs[chainName] = clusterIP
		}
	}

	ipt.topRules.Reset()
	ipt.natRules.Reset()
	ipt.natChains.Reset()
	ipt.sepRules.Reset()
	ipt.svcRules.Reset()
	writeLine(ipt.natRules, "*nat")

	// Make sure we keep stats for the top-level chains, if they existed
	// (which most should have because we created them above).
	for _, chainName := range []utiliptables.Chain{"MULTUS-SERVICES"} {
		ipt.activeChain[chainName] = true
		if chain, ok := ipt.currentNAT[chainName]; ok {
			writeBytesLine(ipt.natRules, chain)
		} else {
			writeLine(ipt.natRules, utiliptables.MakeChainLine(chainName))
		}
	}
}

// FinalizeRules return whether we should update iptables or not
func (ipt *iptableBuffer) FinalizeRules() bool {
	for k := range ipt.activeChain {
		delete(ipt.currentChain, k)
	}

	for chainName := range ipt.currentChain {
		if chain, ok := ipt.currentNAT[chainName]; ok {
			writeBytesLine(ipt.natChains, chain)
		}
		writeLine(ipt.topRules, "-X", string(chainName))

		if clusterIP, ok := ipt.clusterIPs[string(chainName)]; ok {
			//fmt.Fprintf(os.Stderr, "clusterIP: %s will be removed!\n", clusterIP)
			ipt.OldClusterIPs = append(ipt.OldClusterIPs, clusterIP)
		}
	}
	if ipt.sepRules.Len() == 0 && ipt.svcRules.Len() == 0 && ipt.topRules.Len() == 0 {
		return false
	}

	ipt.natRules.Write(ipt.natChains.Bytes())
	ipt.natRules.Write(ipt.sepRules.Bytes())
	ipt.natRules.Write(ipt.svcRules.Bytes())
	ipt.natRules.Write(ipt.topRules.Bytes())
	writeLine(ipt.natRules, "COMMIT")
	return true
}

func (ipt *iptableBuffer) SaveRules(path string) error {
	file, err := os.Create(path)
	defer file.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(file, "%s", ipt.natRules.String())
	return err
}

func (ipt *iptableBuffer) SyncRules(iptables utiliptables.Interface) error {
	/*
		// how to emit the empty rules sync....
		if len(ipt.activeChain) != 1 {
			for k, _ := range ipt.activeChain {
				fmt.Fprintf(os.Stderr, "XXX: %s\n", k)
			}
		}
		fmt.Fprintf(os.Stderr, "========= natRules\n")
		fmt.Fprintf(os.Stderr, "%s", ipt.natRules.String())
		fmt.Fprintf(os.Stderr, "=========\n")
	*/
	return iptables.RestoreAll(ipt.natRules.Bytes(), utiliptables.NoFlushTables, utiliptables.RestoreCounters)
}

func (ipt *iptableBuffer) IsUsed() bool {
	return (len(ipt.activeChain) != 0)
}

// return true if the chain is new, to be added.
func (ipt *iptableBuffer) CreateNATChain(chainName string) bool {
	ipt.activeChain[utiliptables.Chain(chainName)] = true
	// Create chain if not exists
	if chain, ok := ipt.currentNAT[utiliptables.Chain(chainName)]; ok {
		writeBytesLine(ipt.natChains, chain)
		return false
	}

	writeLine(ipt.natChains, utiliptables.MakeChainLine(utiliptables.Chain(chainName)))
	return true
}

func (ipt *iptableBuffer) generateServiceEndpointSliceForwardingRules(s *Server, svcPortName *controllers.ServicePortName, svcInfo *controllers.ServicePortInfo, status *netdefv1.NetworkStatus) error {
	endpoints, ok := s.endpointsMap[*svcPortName]
	if !ok {
		return fmt.Errorf("not found: %s", svcInfo.Name)
	}

	totalEndpointSlices := len(endpoints)
	for _, endpoint := range endpoints {
		//-A MULTUS-SVC-XXXXX -m comment --comment "NAMESPACE/POD:port1"
		//-m statistic --mode random --probability YYYYY -j MULTUS-SEP-ZZZZZ
		endpointChainName := servicePortEndpointChainName(svcInfo.Name, svcInfo.Protocol, endpoint.String())

		ipt.CreateNATChain(endpointChainName)
		writeLine(ipt.svcRules, "-A", svcInfo.ChainName,
			"-m", "comment", "--comment", fmt.Sprintf("\"%s\"", svcInfo.Name),
			"-m", "statistic", "--mode", "random",
			"--probability", s.probability(totalEndpointSlices), "-j", endpointChainName)

		if svcInfo.Protocol != "" {
			//-A MULTUS-SEP-ZZZZZ -p tcp -m comment --comment "NAMESPACE/POD:port1"
			//-m tcp -j DNAT --to-destination <POD IP>
			writeLine(ipt.sepRules, "-A", endpointChainName,
			"-p", svcInfo.Protocol,
			"-m", "comment", "--comment", fmt.Sprintf("\"%s\"", svcInfo.Name),
			"-m", svcInfo.Protocol, "-j", "DNAT", "--to-destination",
			fmt.Sprintf("%s", endpoint.String()))
		} else {
			//-A MULTUS-SEP-ZZZZZ -p tcp -m comment --comment "NAMESPACE/POD:port1"
			//-m tcp -j DNAT --to-destination <POD IP>
			writeLine(ipt.sepRules, "-A", endpointChainName,
			"-m", "comment", "--comment", fmt.Sprintf("\"%s\"", svcInfo.Name),
			"-m", "tcp", "-j", "DNAT", "--to-destination",
			fmt.Sprintf("%s", endpoint.String()))
		}
		totalEndpointSlices--
	}
	return nil
}

func (ipt *iptableBuffer) generateServicePortForwardingRules(s *Server, svcPortName *controllers.ServicePortName, svcInfo *controllers.ServicePortInfo, status *netdefv1.NetworkStatus) error {

	clusterIP := fmt.Sprintf("%s/32", svcInfo.ClusterIP)
	if ipt.CreateNATChain(svcInfo.ChainName) {
		// This iptables rule is new rule to be added, hence add clusterIP into NewClusterIPs
		dev, err := netlink.LinkByName(status.Interface)
		if err != nil {
			return err
		}
		iproute := clusterIPRoute{
			ClusterIP: clusterIP,
			Src:       net.ParseIP(status.IPs[0]),
			Link:      dev,
		}
		ipt.NewClusterIPs = append(ipt.NewClusterIPs, iproute)
	}

	writeLine(ipt.topRules, "-A", serviceChain, "-d", clusterIP,
		"-p", svcInfo.Protocol,
		"-m", "comment", "--comment", fmt.Sprintf("\"%s cluster IP\"", svcInfo.Name),
		"-m", svcInfo.Protocol, "--dport", strconv.Itoa(svcInfo.Port), "-j", svcInfo.ChainName)
	ipt.generateServiceEndpointSliceForwardingRules(s, svcPortName, svcInfo, status)

	return nil
}

// Join all words with spaces, terminate with newline and write to buf.
func writeLine(buf *bytes.Buffer, words ...string) {
	// We avoid strings.Join for performance reasons.
	for i := range words {
		buf.WriteString(words[i])
		if i < len(words)-1 {
			buf.WriteByte(' ')
		} else {
			buf.WriteByte('\n')
		}
	}
}

func writeBytesLine(buf *bytes.Buffer, bytes []byte) {
	buf.Write(bytes)
	buf.WriteByte('\n')
}
