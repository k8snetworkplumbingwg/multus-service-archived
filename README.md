# multus-service


## Current Status of Repository

It is now pretty early stage of development, hence everything may be changed without any notification. Please do NOT use it as production. We don't support any question about how-to-use/how-to-deploy until current development status is changed.


## Description

This repository contains various components which provides service abstraction, which similar to Kubernetes services, for Multus network attachment definitions.


## Supported Feature

Currently these compoents supports following functionalities:

- Cluster IP
- Load-balancing Cluster IP among the service Pods with iptables

Other service related features is to be discussed in [Kubernetes Network Plumbing WG](https://github.com/k8snetworkplumbingwg/community). Some of them might be supported in the future but this does not guarantee that all Kubernetes service features are going to be supported.


## Container Images

Available in [Packages](https://github.com/k8snetworkplumbingwg/multus-service/pkgs/container/multus-service).
Currently amd64 only.

## How to Deploy

Sample deployment is in following:

- [deploy.yml](https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-service/main/deploy.yml), for iptables-legacy users
- [deploy-nft.yml](https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-service/main/deploy-nft.yml), for iptalbes-nft users


## TODO

TBD
