// Copyright 2018 TiKV Project Authors.
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

package member

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/tikv/tikv-operator/pkg/apis/tikv/v1alpha1"
	"github.com/tikv/tikv-operator/pkg/controller"
	"github.com/tikv/tikv-operator/pkg/label"
	"github.com/tikv/tikv-operator/pkg/manager"
	"github.com/tikv/tikv-operator/pkg/pdapi"
	"github.com/tikv/tikv-operator/pkg/util"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	v1 "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"
)

const (
	// tikvClusterCertPath is where the cert for inter-cluster communication stored (if any)
	tikvClusterCertPath = "/var/lib/tikv-tls"

	//find a better way to manage store only managed by tikv in Operator
	tikvStoreLimitPattern = `%s-tikv-\d+\.%s-tikv-peer\.%s\.svc\:\d+`
)

// tikvMemberManager implements manager.Manager.
type tikvMemberManager struct {
	setControl                   controller.StatefulSetControlInterface
	svcControl                   controller.ServiceControlInterface
	pdControl                    pdapi.PDControlInterface
	typedControl                 controller.TypedControlInterface
	setLister                    v1.StatefulSetLister
	svcLister                    corelisters.ServiceLister
	podLister                    corelisters.PodLister
	nodeLister                   corelisters.NodeLister
	autoFailover                 bool
	tikvFailover                 Failover
	tikvScaler                   Scaler
	tikvUpgrader                 Upgrader
	tikvStatefulSetIsUpgradingFn func(corelisters.PodLister, pdapi.PDControlInterface, *apps.StatefulSet, *v1alpha1.TikvCluster) (bool, error)
}

// NewTiKVMemberManager returns a *tikvMemberManager
func NewTiKVMemberManager(
	pdControl pdapi.PDControlInterface,
	setControl controller.StatefulSetControlInterface,
	svcControl controller.ServiceControlInterface,
	typedControl controller.TypedControlInterface,
	setLister v1.StatefulSetLister,
	svcLister corelisters.ServiceLister,
	podLister corelisters.PodLister,
	nodeLister corelisters.NodeLister,
	autoFailover bool,
	tikvFailover Failover,
	tikvScaler Scaler,
	tikvUpgrader Upgrader) manager.Manager {
	kvmm := tikvMemberManager{
		pdControl:    pdControl,
		podLister:    podLister,
		nodeLister:   nodeLister,
		setControl:   setControl,
		svcControl:   svcControl,
		typedControl: typedControl,
		setLister:    setLister,
		svcLister:    svcLister,
		autoFailover: autoFailover,
		tikvFailover: tikvFailover,
		tikvScaler:   tikvScaler,
		tikvUpgrader: tikvUpgrader,
	}
	kvmm.tikvStatefulSetIsUpgradingFn = tikvStatefulSetIsUpgrading
	return &kvmm
}

// SvcConfig corresponds to a K8s service
type SvcConfig struct {
	Name       string
	Port       int32
	SvcLabel   func(label.Label) label.Label
	MemberName func(clusterName string) string
	Headless   bool
	Type       corev1.ServiceType
}

// Sync fulfills the manager.Manager interface
func (tkmm *tikvMemberManager) Sync(tc *v1alpha1.TikvCluster) error {
	ns := tc.GetNamespace()
	tcName := tc.GetName()

	if !tc.PDIsAvailable() {
		return controller.RequeueErrorf("TikvCluster: [%s/%s], waiting for PD cluster running", ns, tcName)
	}

	if err := tkmm.syncStatefulSetForTikvCluster(tc); err != nil {
		return err
	}

	svcList := []*corev1.Service{}
	if tc.Spec.TiKV.ListenersConfig.ExternalListeners != nil {
		for _, eListener := range tc.Spec.TiKV.ListenersConfig.ExternalListeners {
			if eListener.GetAccessMethod() == corev1.ServiceTypeNodePort {
				selectorsTikv, err := label.New().Instance(tcName).TiKV().Selector()
				if err != nil {
					return err
				}

				pods, err := tkmm.podLister.Pods(ns).List(selectorsTikv)
				if err != nil {
					return err
				}

				for idx, pod := range pods {
					svcList = append(svcList, getNewNodeportServiceForTikvCluster(tc, int32(idx), eListener, pod.Status.HostIP, false))
				}
			}
		}
	}

	svcConfig := SvcConfig{
		Name:       "peer",
		Port:       20160,
		Headless:   true,
		SvcLabel:   func(l label.Label) label.Label { return l.TiKV() },
		MemberName: controller.TiKVPeerMemberName,
	}

	svcList = append(svcList, getNewServiceForTikvCluster(tc, svcConfig))

	for i := 0; i < len(svcList); i++ {
		if err := tkmm.syncServiceForTikvCluster(tc, svcList[i]); err != nil {
			return err
		}
	}

	return nil
}

func (tkmm *tikvMemberManager) syncServiceForTikvCluster(tc *v1alpha1.TikvCluster, newSvc *corev1.Service) error {
	if tc.Spec.Paused {
		klog.V(4).Infof("tikv cluster %s/%s is paused, skip syncing for tikv service", tc.GetNamespace(), tc.GetName())
		return nil
	}

	ns := tc.GetNamespace()

	oldSvcTmp, err := tkmm.svcLister.Services(ns).Get(newSvc.GetName())
	if errors.IsNotFound(err) {
		err = controller.SetServiceLastAppliedConfigAnnotation(newSvc)
		if err != nil {
			return err
		}
		return tkmm.svcControl.CreateService(tc, newSvc)
	}
	if err != nil {
		return err
	}

	oldSvc := oldSvcTmp.DeepCopy()

	equal, err := controller.ServiceEqual(newSvc, oldSvc)
	if err != nil {
		return err
	}
	if !equal {
		svc := *oldSvc
		svc.Spec = newSvc.Spec
		// TODO add unit test
		err = controller.SetServiceLastAppliedConfigAnnotation(&svc)
		if err != nil {
			return err
		}
		svc.Spec.ClusterIP = oldSvc.Spec.ClusterIP
		_, err = tkmm.svcControl.UpdateService(tc, &svc)
		return err
	}

	return nil
}

func (tkmm *tikvMemberManager) syncStatefulSetForTikvCluster(tc *v1alpha1.TikvCluster) error {
	ns := tc.GetNamespace()
	tcName := tc.GetName()

	oldSetTmp, err := tkmm.setLister.StatefulSets(ns).Get(controller.TiKVMemberName(tcName))
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	setNotExist := errors.IsNotFound(err)

	oldSet := oldSetTmp.DeepCopy()

	if err := tkmm.syncTikvClusterStatus(tc, oldSet); err != nil {
		return err
	}

	if tc.Spec.Paused {
		klog.V(4).Infof("tikv cluster %s/%s is paused, skip syncing for tikv statefulset", tc.GetNamespace(), tc.GetName())
		return nil
	}

	cm, err := tkmm.syncTiKVConfigMap(tc, oldSet)
	if err != nil {
		return err
	}

	// Recover failed stores if any before generating desired statefulset
	if len(tc.Status.TiKV.FailureStores) > 0 {
		tkmm.tikvFailover.Recover(tc)
	}

	newSet, err := getNewTiKVSetForTikvCluster(tc, cm)
	if err != nil {
		return err
	}
	if setNotExist {
		err = SetStatefulSetLastAppliedConfigAnnotation(newSet)
		if err != nil {
			return err
		}
		err = tkmm.setControl.CreateStatefulSet(tc, newSet)
		if err != nil {
			return err
		}
		tc.Status.TiKV.StatefulSet = &apps.StatefulSetStatus{}
		return nil
	}

	if _, err := tkmm.setStoreLabelsForTiKV(tc); err != nil {
		return err
	}

	if !templateEqual(newSet, oldSet) || tc.Status.TiKV.Phase == v1alpha1.UpgradePhase {
		if err := tkmm.tikvUpgrader.Upgrade(tc, oldSet, newSet); err != nil {
			return err
		}
	}

	if err := tkmm.tikvScaler.Scale(tc, oldSet, newSet); err != nil {
		return err
	}

	if tkmm.autoFailover && tc.Spec.TiKV.MaxFailoverCount != nil {
		if tc.TiKVAllPodsStarted() && !tc.TiKVAllStoresReady() {
			if err := tkmm.tikvFailover.Failover(tc); err != nil {
				return err
			}
		}
	}

	return updateStatefulSet(tkmm.setControl, tc, newSet, oldSet)
}

func (tkmm *tikvMemberManager) syncTiKVConfigMap(tc *v1alpha1.TikvCluster, set *apps.StatefulSet) (*corev1.ConfigMap, error) {
	// For backward compatibility, only sync tidb configmap when .tikv.config is non-nil
	if tc.Spec.TiKV.Config == nil {
		return nil, nil
	}
	newCm, err := getTikVConfigMap(tc)
	if err != nil {
		return nil, err
	}
	if set != nil && tc.BaseTiKVSpec().ConfigUpdateStrategy() == v1alpha1.ConfigUpdateStrategyInPlace {
		inUseName := FindConfigMapVolume(&set.Spec.Template.Spec, func(name string) bool {
			return strings.HasPrefix(name, controller.TiKVMemberName(tc.Name))
		})
		if inUseName != "" {
			newCm.Name = inUseName
		}
	}

	return tkmm.typedControl.CreateOrUpdateConfigMap(tc, newCm)
}

func getNewServiceForTikvCluster(tc *v1alpha1.TikvCluster, svcConfig SvcConfig) *corev1.Service {
	ns := tc.Namespace
	tcName := tc.Name
	instanceName := tc.GetInstanceName()
	svcName := svcConfig.MemberName(tcName)
	svcLabel := svcConfig.SvcLabel(label.New().Instance(instanceName)).Labels()

	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            svcName,
			Namespace:       ns,
			Labels:          svcLabel,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       svcConfig.Name,
					Port:       svcConfig.Port,
					TargetPort: intstr.FromInt(int(svcConfig.Port)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector:                 svcLabel,
			PublishNotReadyAddresses: true,
		},
	}
	if svcConfig.Headless {
		svc.Spec.ClusterIP = "None"
	} else {
		svc.Spec.Type = corev1.ServiceTypeClusterIP
	}
	return &svc
}

func getNewTiKVSetForTikvCluster(tc *v1alpha1.TikvCluster, cm *corev1.ConfigMap) (*apps.StatefulSet, error) {
	ns := tc.GetNamespace()
	tcName := tc.GetName()
	baseTiKVSpec := tc.BaseTiKVSpec()

	tikvConfigMap := controller.MemberConfigMapName(tc, v1alpha1.TiKVMemberType)
	if cm != nil {
		tikvConfigMap = cm.Name
	}

	annMount, annVolume := annotationsMountVolume()
	volMounts := []corev1.VolumeMount{
		annMount,
		{Name: v1alpha1.TiKVMemberType.String(), MountPath: "/var/lib/tikv"},
		{Name: "config", ReadOnly: true, MountPath: "/etc/tikv"},
		{Name: "startup-script", ReadOnly: true, MountPath: "/usr/local/bin"},
	}
	if tc.IsTLSClusterEnabled() {
		volMounts = append(volMounts, corev1.VolumeMount{
			Name: "tikv-tls", ReadOnly: true, MountPath: "/var/lib/tikv-tls",
		})
	}

	vols := []corev1.Volume{
		annVolume,
		{Name: "config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: tikvConfigMap,
				},
				Items: []corev1.KeyToPath{{Key: "config-file", Path: "tikv.toml"}},
			}},
		},
		{Name: "startup-script", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: tikvConfigMap,
				},
				Items: []corev1.KeyToPath{{Key: "startup-script", Path: "tikv_start_script.sh"}},
			}},
		},
	}
	if tc.IsTLSClusterEnabled() {
		vols = append(vols, corev1.Volume{
			Name: "tikv-tls", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: util.ClusterTLSSecretName(tc.Name, label.TiKVLabelVal),
				},
			},
		})
	}

	sysctls := "sysctl -w"
	var initContainers []corev1.Container
	if baseTiKVSpec.Annotations() != nil {
		init, ok := baseTiKVSpec.Annotations()[label.AnnSysctlInit]
		if ok && (init == label.AnnSysctlInitVal) {
			if baseTiKVSpec.PodSecurityContext() != nil && len(baseTiKVSpec.PodSecurityContext().Sysctls) > 0 {
				for _, sysctl := range baseTiKVSpec.PodSecurityContext().Sysctls {
					sysctls = sysctls + fmt.Sprintf(" %s=%s", sysctl.Name, sysctl.Value)
				}
				privileged := true
				initContainers = append(initContainers, corev1.Container{
					Name:  "init",
					Image: tc.HelperImage(),
					Command: []string{
						"sh",
						"-c",
						sysctls,
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
				})
			}
		}
	}
	// Init container is only used for the case where allowed-unsafe-sysctls
	// cannot be enabled for kubelet, so clean the sysctl in statefulset
	// SecurityContext if init container is enabled
	podSecurityContext := baseTiKVSpec.PodSecurityContext().DeepCopy()
	if len(initContainers) > 0 {
		podSecurityContext.Sysctls = []corev1.Sysctl{}
	}

	storageRequest, err := controller.ParseStorageRequest(tc.Spec.TiKV.Requests)
	if err != nil {
		return nil, fmt.Errorf("cannot parse storage request for tikv, tidbcluster %s/%s, error: %v", tc.Namespace, tc.Name, err)
	}

	tikvLabel := labelTiKV(tc)
	setName := controller.TiKVMemberName(tcName)
	podAnnotations := CombineAnnotations(controller.AnnProm(20180), baseTiKVSpec.Annotations())
	stsAnnotations := getStsAnnotations(tc, label.TiKVLabelVal)
	capacity := controller.TiKVCapacity(tc.Spec.TiKV.Limits)
	headlessSvcName := controller.TiKVPeerMemberName(tcName)

	env := []corev1.EnvVar{
		{
			Name: "NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name:  "CLUSTER_NAME",
			Value: tcName,
		},
		{
			Name:  "HEADLESS_SERVICE_NAME",
			Value: headlessSvcName,
		},
		{
			Name:  "CAPACITY",
			Value: capacity,
		},
		{
			Name:  "TZ",
			Value: tc.Spec.Timezone,
		},
	}
	tikvContainer := corev1.Container{
		Name:            v1alpha1.TiKVMemberType.String(),
		Image:           tc.TiKVImage(),
		ImagePullPolicy: baseTiKVSpec.ImagePullPolicy(),
		Command:         []string{"/bin/sh", "/usr/local/bin/tikv_start_script.sh"},
		SecurityContext: &corev1.SecurityContext{
			Privileged: tc.TiKVContainerPrivilege(),
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "server",
				ContainerPort: int32(20160),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: volMounts,
		Resources:    controller.ContainerResource(tc.Spec.TiKV.ResourceRequirements),
	}
	podSpec := baseTiKVSpec.BuildPodSpec()
	if baseTiKVSpec.HostNetwork() {
		podSpec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
		env = append(env, corev1.EnvVar{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		})
	}
	tikvContainer.Env = util.AppendEnv(env, baseTiKVSpec.Env())
	podSpec.Volumes = vols
	podSpec.SecurityContext = podSecurityContext
	podSpec.InitContainers = initContainers
	podSpec.Containers = []corev1.Container{tikvContainer}
	podSpec.ServiceAccountName = tc.Spec.TiKV.ServiceAccount

	tikvset := &apps.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            setName,
			Namespace:       ns,
			Labels:          tikvLabel.Labels(),
			Annotations:     stsAnnotations,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Spec: apps.StatefulSetSpec{
			Replicas: controller.Int32Ptr(tc.TiKVStsDesiredReplicas()),
			Selector: tikvLabel.LabelSelector(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      tikvLabel.Labels(),
					Annotations: podAnnotations,
				},
				Spec: podSpec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				volumeClaimTemplate(storageRequest, v1alpha1.TiKVMemberType.String(), tc.Spec.TiKV.StorageClassName),
			},
			ServiceName:         headlessSvcName,
			PodManagementPolicy: apps.ParallelPodManagement,
			UpdateStrategy: apps.StatefulSetUpdateStrategy{
				Type: apps.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &apps.RollingUpdateStatefulSetStrategy{
					Partition: controller.Int32Ptr(tc.TiKVStsDesiredReplicas()),
				},
			},
		},
	}
	return tikvset, nil
}

func volumeClaimTemplate(r corev1.ResourceRequirements, metaName string, storageClassName *string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: metaName},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			StorageClassName: storageClassName,
			Resources:        r,
		},
	}
}

func getTikVConfigMap(tc *v1alpha1.TikvCluster) (*corev1.ConfigMap, error) {

	config := tc.Spec.TiKV.Config
	if config == nil {
		return nil, nil
	}

	confText, err := MarshalTOML(config)
	if err != nil {
		return nil, err
	}
	startScript, err := RenderTiKVStartScript(&TiKVStartScriptModel{
		Scheme: tc.Scheme(),
	})
	if err != nil {
		return nil, err
	}
	instanceName := tc.GetInstanceName()
	tikvLabel := label.New().Instance(instanceName).TiKV().Labels()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            controller.TiKVMemberName(tc.Name),
			Namespace:       tc.Namespace,
			Labels:          tikvLabel,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Data: map[string]string{
			"config-file":    string(confText),
			"startup-script": startScript,
		},
	}

	if tc.BaseTiKVSpec().ConfigUpdateStrategy() == v1alpha1.ConfigUpdateStrategyRollingUpdate {
		if err := AddConfigMapDigestSuffix(cm); err != nil {
			return nil, err
		}
	}

	return cm, nil
}

func labelTiKV(tc *v1alpha1.TikvCluster) label.Label {
	instanceName := tc.GetInstanceName()
	return label.New().Instance(instanceName).TiKV()
}

func (tkmm *tikvMemberManager) syncTikvClusterStatus(tc *v1alpha1.TikvCluster, set *apps.StatefulSet) error {
	if set == nil {
		// skip if not created yet
		return nil
	}
	tc.Status.TiKV.StatefulSet = &set.Status
	upgrading, err := tkmm.tikvStatefulSetIsUpgradingFn(tkmm.podLister, tkmm.pdControl, set, tc)
	if err != nil {
		return err
	}
	if upgrading && tc.Status.PD.Phase != v1alpha1.UpgradePhase {
		tc.Status.TiKV.Phase = v1alpha1.UpgradePhase
	} else {
		tc.Status.TiKV.Phase = v1alpha1.NormalPhase
	}

	previousStores := tc.Status.TiKV.Stores
	stores := map[string]v1alpha1.TiKVStore{}
	tombstoneStores := map[string]v1alpha1.TiKVStore{}

	pdCli := controller.GetPDClient(tkmm.pdControl, tc)
	// This only returns Up/Down/Offline stores
	storesInfo, err := pdCli.GetStores()
	if err != nil {
		tc.Status.TiKV.Synced = false
		return err
	}

	pattern, err := regexp.Compile(fmt.Sprintf(tikvStoreLimitPattern, tc.Name, tc.Name, tc.Namespace))
	if err != nil {
		return err
	}
	for _, store := range storesInfo.Stores {
		// In theory, the external tikv can join the cluster, and the operator would only manage the internal tikv.
		// So we check the store owner to make sure it.
		if store.Store != nil && !pattern.Match([]byte(store.Store.Address)) {
			continue
		}
		status := tkmm.getTiKVStore(store)
		if status == nil {
			continue
		}
		// avoid LastHeartbeatTime be overwrite by zero time when pd lost LastHeartbeatTime
		if status.LastHeartbeatTime.IsZero() {
			if oldStatus, ok := previousStores[status.ID]; ok {
				klog.V(4).Infof("the pod:%s's store LastHeartbeatTime is zero,so will keep in %v", status.PodName, oldStatus.LastHeartbeatTime)
				status.LastHeartbeatTime = oldStatus.LastHeartbeatTime
			}
		}

		oldStore, exist := previousStores[status.ID]

		status.LastTransitionTime = metav1.Now()
		if exist && status.State == oldStore.State {
			status.LastTransitionTime = oldStore.LastTransitionTime
		}

		stores[status.ID] = *status
	}

	//this returns all tombstone stores
	tombstoneStoresInfo, err := pdCli.GetTombStoneStores()
	if err != nil {
		tc.Status.TiKV.Synced = false
		return err
	}
	for _, store := range tombstoneStoresInfo.Stores {
		if store.Store != nil && !pattern.Match([]byte(store.Store.Address)) {
			continue
		}
		status := tkmm.getTiKVStore(store)
		if status == nil {
			continue
		}
		tombstoneStores[status.ID] = *status
	}

	tc.Status.TiKV.Synced = true
	tc.Status.TiKV.Stores = stores
	tc.Status.TiKV.TombstoneStores = tombstoneStores
	tc.Status.TiKV.Image = ""
	c := filterContainer(set, "tikv")
	if c != nil {
		tc.Status.TiKV.Image = c.Image
	}
	return nil
}

func (tkmm *tikvMemberManager) getTiKVStore(store *pdapi.StoreInfo) *v1alpha1.TiKVStore {
	if store.Store == nil || store.Status == nil {
		return nil
	}
	storeID := fmt.Sprintf("%d", store.Store.GetId())
	ip := strings.Split(store.Store.GetAddress(), ":")[0]
	podName := strings.Split(ip, ".")[0]

	return &v1alpha1.TiKVStore{
		ID:                storeID,
		PodName:           podName,
		IP:                ip,
		LeaderCount:       int32(store.Status.LeaderCount),
		State:             store.Store.StateName,
		LastHeartbeatTime: metav1.Time{Time: store.Status.LastHeartbeatTS},
	}
}

func (tkmm *tikvMemberManager) setStoreLabelsForTiKV(tc *v1alpha1.TikvCluster) (int, error) {
	ns := tc.GetNamespace()
	// for unit test
	setCount := 0

	pdCli := controller.GetPDClient(tkmm.pdControl, tc)
	storesInfo, err := pdCli.GetStores()
	if err != nil {
		return setCount, err
	}

	config, err := pdCli.GetConfig()
	if err != nil {
		return setCount, err
	}

	locationLabels := []string(config.Replication.LocationLabels)
	if locationLabels == nil {
		return setCount, nil
	}

	pattern, err := regexp.Compile(fmt.Sprintf(tikvStoreLimitPattern, tc.Name, tc.Name, tc.Namespace))
	if err != nil {
		return -1, err
	}
	for _, store := range storesInfo.Stores {
		// In theory, the external tikv can join the cluster, and the operator would only manage the internal tikv.
		// So we check the store owner to make sure it.
		if store.Store != nil && !pattern.Match([]byte(store.Store.Address)) {
			continue
		}
		status := tkmm.getTiKVStore(store)
		if status == nil {
			continue
		}
		podName := status.PodName

		pod, err := tkmm.podLister.Pods(ns).Get(podName)
		if err != nil {
			return setCount, err
		}

		nodeName := pod.Spec.NodeName
		ls, err := tkmm.getNodeLabels(nodeName, locationLabels)
		if err != nil || len(ls) == 0 {
			klog.Warningf("node: [%s] has no node labels, skipping set store labels for Pod: [%s/%s]", nodeName, ns, podName)
			continue
		}

		if !tkmm.storeLabelsEqualNodeLabels(store.Store.Labels, ls) {
			set, err := pdCli.SetStoreLabels(store.Store.Id, ls)
			if err != nil {
				klog.Warningf("failed to set pod: [%s/%s]'s store labels: %v", ns, podName, ls)
				continue
			}
			if set {
				setCount++
				klog.Infof("pod: [%s/%s] set labels: %v successfully", ns, podName, ls)
			}
		}
	}

	return setCount, nil
}

func (tkmm *tikvMemberManager) getNodeLabels(nodeName string, storeLabels []string) (map[string]string, error) {
	node, err := tkmm.nodeLister.Get(nodeName)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	ls := node.GetLabels()
	for _, storeLabel := range storeLabels {
		if value, found := ls[storeLabel]; found {
			labels[storeLabel] = value
			continue
		}

		// TODO after pd supports storeLabel containing slash character, these codes should be deleted
		if storeLabel == "host" {
			if host, found := ls[corev1.LabelHostname]; found {
				labels[storeLabel] = host
			}
		}

	}
	return labels, nil
}

// storeLabelsEqualNodeLabels compares store labels with node labels
// for historic reasons, PD stores TiKV labels as []*StoreLabel which is a key-value pair slice
func (tkmm *tikvMemberManager) storeLabelsEqualNodeLabels(storeLabels []*metapb.StoreLabel, nodeLabels map[string]string) bool {
	ls := map[string]string{}
	for _, label := range storeLabels {
		key := label.GetKey()
		if _, ok := nodeLabels[key]; ok {
			val := label.GetValue()
			ls[key] = val
		}
	}
	return reflect.DeepEqual(ls, nodeLabels)
}

func tikvStatefulSetIsUpgrading(podLister corelisters.PodLister, pdControl pdapi.PDControlInterface, set *apps.StatefulSet, tc *v1alpha1.TikvCluster) (bool, error) {
	if statefulSetIsUpgrading(set) {
		return true, nil
	}
	instanceName := tc.GetInstanceName()
	selector, err := label.New().Instance(instanceName).TiKV().Selector()
	if err != nil {
		return false, err
	}
	tikvPods, err := podLister.Pods(tc.GetNamespace()).List(selector)
	if err != nil {
		return false, err
	}
	for _, pod := range tikvPods {
		revisionHash, exist := pod.Labels[apps.ControllerRevisionHashLabelKey]
		if !exist {
			return false, nil
		}
		if revisionHash != tc.Status.TiKV.StatefulSet.UpdateRevision {
			return true, nil
		}
	}

	return false, nil
}

type FakeTiKVMemberManager struct {
	err error
}

func NewFakeTiKVMemberManager() *FakeTiKVMemberManager {
	return &FakeTiKVMemberManager{}
}

func (ftmm *FakeTiKVMemberManager) SetSyncError(err error) {
	ftmm.err = err
}

func (ftmm *FakeTiKVMemberManager) Sync(tc *v1alpha1.TikvCluster) error {
	if ftmm.err != nil {
		return ftmm.err
	}
	if len(tc.Status.TiKV.Stores) != 0 {
		// simulate status update
		tc.Status.ClusterID = string(uuid.NewUUID())
	}
	return nil
}
