# multus-service (archived)

## Current Status of Repository

In [the NPWG call at April 18](https://docs.google.com/document/d/1oE93V3SgOGWJ4O1zeD1UmpeToa0ZiiO6LqRAmZBPFWM/edit#heading=h.40vwjmncy42s), we decide that this repository is archived. But this does NOT means Kubernetes service for secondary interfaces is impossible. Here is the reason why we decide to close.

- Insufficient use-cases and feedback

 Secondary network is used varous scenarios, such as to connect isolated network (i.e. closed network), to connect high-speed network and so on. Hence multus-service also needs to apply such various use-cases and we need to gather the use-cases and requirements not only from users also from vendors. We have insufficient use-cases and feedback in NPWG meeting hence we cannot disucss how user want to use the service for secondary network. Kubernetes service functionality is not a single functionality, composition of various functions (i.e. NodePort, headless service, load-balancer and so on), so use-cases and feedback were pretty important.

- Kubernetes community want to use Gateway API, instead of endpoint/endpointslices API

 [Tim Hockin's LT at KubeCon 2023 NA](https://www.youtube.com/watch?v=Oslwx3hj2Eg) mentions that Kubernetes Gateway API could be replaced with Kubernetes Service API. This repository uses service API, hence this repository is not aligned with current Kubernetes architecture based on Tim Hockin's presentation. So if we need to implement it, Gateway API should be used to align Kubernetes architecture. It should be implemented from scratch, not from this repository.

- Several community, including Kubernetes community itself, started the discussion about secondary network service

Currently, Kubernetes sig-network launches [Multi-network WG](https://github.com/kubernetes/community/blob/master/sig-network/README.md) and discusses about multiple network interfaces in Kubernetes Pod, including Service and other Kubernetes functionalities (e.g. NetworkPolicy). So I expects that this working group will provide the design and API for Kubernetes service for secondary network interfaces, not this repository. So if there is someone who really wants this feature, I strongly recommend to join this multi-network WG and provide your use-case scenarios.

Here are the reasons. So this repository archives is not end of development, just restart the design discussion with use-cases in another working group. Please join the community and discuss about your use-cases in the community if you want the feature.

---

## Description

This repository contains various components which provides service abstraction, which similar to Kubernetes services, for Multus network attachment definitions.


## Supported Feature

Currently these compoents supports following functionalities:

- Cluster IP
- Load-balancing Cluster IP among the service Pods with iptables

Other service related features is to be discussed in [Kubernetes Network Plumbing WG](https://github.com/k8snetworkplumbingwg/community). Some of them might be supported in the future but this does not guarantee that all Kubernetes service features are going to be supported.

### Limitation

As noted above section, we do not support everything except above yet. We need to discuss how to implement and how to support it. Please keep in mind that these feature should be discussed but it may be concluded NOT to support (due to multus network design perspective or some other reason).

Example (not support it yet):

- Load balancer
- Expose multus service to outside cluster
- `kubectl port-forward svc` command
- Headless service (see [#22](https://github.com/k8snetworkplumbingwg/multus-service/issues/22) for the detail)

## Requirements

TBD (verified with kubeadm k8s and cri-o as container runtime for now)
Note: docker for container runtime is not supported as latest Kubernetes does.


## Container Images

Available in [Packages](https://github.com/k8snetworkplumbingwg/multus-service/pkgs/container/multus-service).
Currently amd64 only.

## How to Deploy

Sample deployment is in following:

- [deploy.yml](https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-service/main/deploy.yml), for iptables-legacy users
- [deploy-nft.yml](https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-service/main/deploy-nft.yml), for iptalbes-nft users

## TODO

TBD
