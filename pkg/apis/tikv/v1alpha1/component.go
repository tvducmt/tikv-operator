// Copyright 2019 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	defaultHostNetwork = false
)

// +kubebuilder:object:generate=false
// ComponentAccessor is the interface to access component details, which respects the cluster-level properties
// and component-level overrides
type ComponentAccessor interface {
	ImagePullPolicy() corev1.PullPolicy
	HostNetwork() bool
	Affinity() *corev1.Affinity
	PriorityClassName() *string
	NodeSelector() map[string]string
	Annotations() map[string]string
	Tolerations() []corev1.Toleration
	PodSecurityContext() *corev1.PodSecurityContext
	SchedulerName() string
	DnsPolicy() corev1.DNSPolicy
	ConfigUpdateStrategy() ConfigUpdateStrategy
	BuildPodSpec() corev1.PodSpec
	Env() []corev1.EnvVar
}

type componentAccessorImpl struct {
	// Cluster is the TikvCluster Spec
	ClusterSpec *TikvClusterSpec

	// Cluster is the Component Spec
	ComponentSpec *ComponentSpec
}

func (a *componentAccessorImpl) PodSecurityContext() *corev1.PodSecurityContext {
	return a.ComponentSpec.PodSecurityContext
}

func (a *componentAccessorImpl) ImagePullPolicy() corev1.PullPolicy {
	pp := a.ComponentSpec.ImagePullPolicy
	if pp == nil {
		return a.ClusterSpec.ImagePullPolicy
	}
	return *pp
}

func (a *componentAccessorImpl) HostNetwork() bool {
	hostNetwork := a.ComponentSpec.HostNetwork
	if hostNetwork == nil {
		hostNetwork = a.ClusterSpec.HostNetwork
	}
	if hostNetwork == nil {
		return defaultHostNetwork
	}
	return *hostNetwork
}

func (a *componentAccessorImpl) Affinity() *corev1.Affinity {
	affi := a.ComponentSpec.Affinity
	if affi == nil {
		affi = a.ClusterSpec.Affinity
	}
	return affi
}

func (a *componentAccessorImpl) PriorityClassName() *string {
	pcn := a.ComponentSpec.PriorityClassName
	if pcn == nil {
		pcn = a.ClusterSpec.PriorityClassName
	}
	return pcn
}

func (a *componentAccessorImpl) SchedulerName() string {
	pcn := a.ComponentSpec.SchedulerName
	if pcn == nil {
		pcn = &a.ClusterSpec.SchedulerName
	}
	return *pcn
}

func (a *componentAccessorImpl) NodeSelector() map[string]string {
	sel := map[string]string{}
	for k, v := range a.ClusterSpec.NodeSelector {
		sel[k] = v
	}
	for k, v := range a.ComponentSpec.NodeSelector {
		sel[k] = v
	}
	return sel
}

func (a *componentAccessorImpl) Annotations() map[string]string {
	anno := map[string]string{}
	for k, v := range a.ClusterSpec.Annotations {
		anno[k] = v
	}
	for k, v := range a.ComponentSpec.Annotations {
		anno[k] = v
	}
	return anno
}

func (a *componentAccessorImpl) Tolerations() []corev1.Toleration {
	tols := a.ComponentSpec.Tolerations
	if len(tols) == 0 {
		tols = a.ClusterSpec.Tolerations
	}
	return tols
}

func (a *componentAccessorImpl) DnsPolicy() corev1.DNSPolicy {
	dnsPolicy := corev1.DNSClusterFirst // same as kubernetes default
	if a.HostNetwork() {
		dnsPolicy = corev1.DNSClusterFirstWithHostNet
	}
	return dnsPolicy
}

func (a *componentAccessorImpl) ConfigUpdateStrategy() ConfigUpdateStrategy {
	strategy := a.ComponentSpec.ConfigUpdateStrategy
	if strategy == nil {
		strategy = &a.ClusterSpec.ConfigUpdateStrategy
	}
	// defaulting logic will set a default value for configUpdateStrategy field, but if the
	// object is created in early version without this field being set, we should set a safe default
	if string(*strategy) == "" {
		return ConfigUpdateStrategyInPlace
	}
	return *strategy
}

func (a *componentAccessorImpl) BuildPodSpec() corev1.PodSpec {
	spec := corev1.PodSpec{
		SchedulerName:   a.SchedulerName(),
		Affinity:        a.Affinity(),
		NodeSelector:    a.NodeSelector(),
		HostNetwork:     a.HostNetwork(),
		RestartPolicy:   corev1.RestartPolicyAlways,
		Tolerations:     a.Tolerations(),
		SecurityContext: a.PodSecurityContext(),
	}
	if a.PriorityClassName() != nil {
		spec.PriorityClassName = *a.PriorityClassName()
	}
	return spec
}

func (a *componentAccessorImpl) Env() []corev1.EnvVar {
	return a.ComponentSpec.Env
}

// BaseTiKVSpec returns the base spec of TiKV servers
func (tc *TikvCluster) BaseTiKVSpec() ComponentAccessor {
	return &componentAccessorImpl{&tc.Spec, &tc.Spec.TiKV.ComponentSpec}
}

// BasePDSpec returns the base spec of PD servers
func (tc *TikvCluster) BasePDSpec() ComponentAccessor {
	return &componentAccessorImpl{&tc.Spec, &tc.Spec.PD.ComponentSpec}
}
