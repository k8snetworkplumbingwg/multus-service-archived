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
	"time"

	netdefv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type FakePodConfigStub struct {
	CounterAdd    int
	CounterUpdate int
	CounterDelete int
	CounterSynced int
}

func (f *FakePodConfigStub) OnPodAdd(_ *v1.Pod) {
	f.CounterAdd++
}

func (f *FakePodConfigStub) OnPodUpdate(_, _ *v1.Pod) {
	f.CounterUpdate++
}

func (f *FakePodConfigStub) OnPodDelete(_ *v1.Pod) {
	f.CounterDelete++
}

func (f *FakePodConfigStub) OnPodSynced() {
	f.CounterSynced++
}

func NewFakePodConfig(stub *FakePodConfigStub) *PodConfig {
	configSync := 15 * time.Minute
	fakeClient := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactoryWithOptions(fakeClient, configSync)
	podConfig := NewPodConfig(informerFactory.Core().V1().Pods(), configSync)
	podConfig.RegisterEventHandler(stub)
	return podConfig
}

func NewFakePodWithNetAnnotation(namespace, name, annot, status string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			UID:       "testUID",
			Annotations: map[string]string{
				"k8s.v1.cni.cncf.io/networks": annot,
				netdefv1.NetworkStatusAnnot:   status,
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "ctr1", Image: "image"},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
}

func NewFakeNetworkStatus(netns, netname string) string {
	baseStr := `
	[{
            "name": "",
            "interface": "eth0",
            "ips": [
                "10.244.1.4"
            ],
            "mac": "aa:e1:20:71:15:01",
            "default": true,
            "dns": {}
        },{
            "name": "%s/%s",
            "interface": "net1",
            "ips": [
                "10.1.1.101"
            ],
            "mac": "42:90:65:12:3e:bf",
            "dns": {}
        }]
`
	return fmt.Sprintf(baseStr, netns, netname)
}

func NewFakePod(namespace, name string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			UID:       "testUID",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "ctr1", Image: "image"},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
}

func NewFakePodChangeTracker(hostname, hostPrefix string) *PodChangeTracker {
	return &PodChangeTracker{
		items:    make(map[types.NamespacedName]*podChange),
		hostname: hostname,
	}
}

var _ = Describe("pod config", func() {
	It("check add handler", func() {
		stub := &FakePodConfigStub{}
		nsConfig := NewFakePodConfig(stub)
		nsConfig.handleAddPod(NewFakePod("testns1", "pod"))
		Expect(stub.CounterAdd).To(Equal(1))
		Expect(stub.CounterUpdate).To(Equal(0))
		Expect(stub.CounterDelete).To(Equal(0))
		Expect(stub.CounterSynced).To(Equal(0))
	})

	It("check update handler", func() {
		stub := &FakePodConfigStub{}
		nsConfig := NewFakePodConfig(stub)
		nsConfig.handleUpdatePod(NewFakePod("testns1", "pod"), NewFakePod("testns2", "pod"))
		Expect(stub.CounterAdd).To(Equal(0))
		Expect(stub.CounterUpdate).To(Equal(1))
		Expect(stub.CounterDelete).To(Equal(0))
		Expect(stub.CounterSynced).To(Equal(0))
	})

	It("check update handler", func() {
		stub := &FakePodConfigStub{}
		nsConfig := NewFakePodConfig(stub)
		nsConfig.handleDeletePod(NewFakePod("testns1", "pod"))
		Expect(stub.CounterAdd).To(Equal(0))
		Expect(stub.CounterUpdate).To(Equal(0))
		Expect(stub.CounterDelete).To(Equal(1))
		Expect(stub.CounterSynced).To(Equal(0))
	})
})

var _ = Describe("pod controller", func() {
	It("Initialize and verify empty", func() {
		podChanges := NewFakePodChangeTracker("nodeName", "hostPrefix")
		podMap := make(PodMap)
		podMap.Update(podChanges)
		Expect(len(podMap)).To(Equal(0))
	})

	It("Add pod and verify", func() {
		podChanges := NewFakePodChangeTracker("nodeName", "hostPrefix")

		Expect(podChanges.Update(nil, NewFakePod("testns1", "testpod1"))).To(BeTrue())

		podMap := make(PodMap)
		podMap.Update(podChanges)
		Expect(len(podMap)).To(Equal(1))

		pod1, ok := podMap[types.NamespacedName{Namespace: "testns1", Name: "testpod1"}]
		Expect(ok).To(BeTrue())
		Expect(pod1.Name).To(Equal("testpod1"))
		Expect(pod1.Namespace).To(Equal("testns1"))
	})

	It("Add ns then del ns and verify", func() {
		podChanges := NewFakePodChangeTracker("nodeName", "hostPrefix")

		Expect(podChanges.Update(nil, NewFakePod("testns1", "testpod1"))).To(BeTrue())
		Expect(podChanges.Update(NewFakePod("testns1", "testpod1"), nil)).To(BeTrue())
		Expect(podChanges.Update(nil, NewFakePod("testns2", "testpod2"))).To(BeTrue())

		podMap := make(PodMap)
		podMap.Update(podChanges)
		Expect(len(podMap)).To(Equal(1))

		pod1, ok := podMap[types.NamespacedName{Namespace: "testns2", Name: "testpod2"}]
		Expect(ok).To(BeTrue())
		Expect(pod1.Name).To(Equal("testpod2"))
		Expect(pod1.Namespace).To(Equal("testns2"))
	})

	It("invalid Update case", func() {
		podChanges := NewFakePodChangeTracker("nodeName", "hostPrefix")
		Expect(podChanges.Update(nil, nil)).To(BeFalse())
	})

	It("Add pod with net-attach annotation and verify", func() {
		podChanges := NewFakePodChangeTracker("nodeName", "hostPrefix")

		Expect(podChanges.Update(nil, NewFakePodWithNetAnnotation("testns1", "testpod1", "net-attach1", NewFakeNetworkStatus("testns1", "net-attach1")))).To(BeTrue())
		podMap := make(PodMap)
		podMap.Update(podChanges)
		Expect(len(podMap)).To(Equal(1))

		pod1, ok := podMap[types.NamespacedName{Namespace: "testns1", Name: "testpod1"}]
		Expect(ok).To(BeTrue())
		Expect(pod1.Name).To(Equal("testpod1"))
		Expect(pod1.Namespace).To(Equal("testns1"))
		Expect(len(pod1.Interfaces)).To(Equal(1))
	})

	It("Add pod with net-attach annotation and verify", func() {
		podChanges := NewFakePodChangeTracker("nodeName", "hostPrefix")

		Expect(podChanges.Update(nil, NewFakePod("testns1", "testpod1"))).To(BeTrue())
		podMap := make(PodMap)
		podMap.Update(podChanges)
		Expect(len(podMap)).To(Equal(1))

		pod1, ok := podMap[types.NamespacedName{Namespace: "testns1", Name: "testpod1"}]
		Expect(ok).To(BeTrue())
		Expect(pod1.Name).To(Equal("testpod1"))
		Expect(pod1.Namespace).To(Equal("testns1"))
		Expect(len(pod1.Interfaces)).To(Equal(0))

		Expect(podChanges.Update(NewFakePod("testns1", "testpod1"), NewFakePodWithNetAnnotation("testns1", "testpod1", "net-attach1", NewFakeNetworkStatus("testns1", "net-attach1")))).To(BeTrue())

		podMap.Update(podChanges)
		Expect(len(podMap)).To(Equal(1))

		pod2, ok := podMap[types.NamespacedName{Namespace: "testns1", Name: "testpod1"}]
		Expect(ok).To(BeTrue())
		Expect(pod2.Name).To(Equal("testpod1"))
		Expect(pod2.Namespace).To(Equal("testns1"))
		Expect(len(pod2.Interfaces)).To(Equal(1))
	})

})

var _ = Describe("runtime kind", func() {
	It("Check container runtime valid case", func() {
		var runtime RuntimeKind
		Expect(runtime.Set("docker")).To(BeNil())
		Expect(runtime.Set("Docker")).To(BeNil())
		Expect(runtime.Set("cri")).To(BeNil())
		Expect(runtime.Set("CRI")).To(BeNil())
	})
	It("Check container runtime option invalid case", func() {
		var runtime RuntimeKind
		Expect(runtime.Set("Foobar")).To(MatchError("Invalid container-runtime option Foobar (possible values: \"docker\", \"cri\")"))
	})
})
