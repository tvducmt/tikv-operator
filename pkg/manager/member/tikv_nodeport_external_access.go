package member

import (
	"fmt"

	"github.com/tikv/tikv-operator/pkg/apis/tikv/v1alpha1"
	"github.com/tikv/tikv-operator/pkg/controller"
	"github.com/tikv/tikv-operator/pkg/label"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func getNewNodeportServiceForTikvCluster(tc *v1alpha1.TikvCluster, id int32, extListener v1alpha1.ExternalListenerConfig, nodePortExternalIP string, isPD bool) *corev1.Service {
	var (
		tcName   = tc.Name
		nodePort = int32(0)
		svc      = corev1.Service{}
	)

	if extListener.ExternalStartingPort > 0 {
		nodePort = extListener.ExternalStartingPort + id
	}
	if isPD {
		lbPD := label.New().Instance(tcName).PD().Labels()
		svc = corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("%s-pb-%d-%s", tcName, id, extListener.Name),
				Labels:          MergeLabels(lbPD, map[string]string{"statefulset.kubernetes.io/pod-name": fmt.Sprintf("basic-pd-%d", id)}),
				Namespace:       tc.Namespace,
				OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
			},
			Spec: corev1.ServiceSpec{
				Selector: MergeLabels(lbPD, map[string]string{"statefulset.kubernetes.io/pod-name": fmt.Sprintf("basic-pd-%d", id)}),
				Type:     corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{
					{
						Name:       fmt.Sprintf("%s-%d-%s", tcName, id, extListener.Name),
						Port:       extListener.ContainerPort,
						NodePort:   nodePort,
						TargetPort: intstr.FromInt(int(2380)),
						Protocol:   corev1.ProtocolTCP,
					},
				},
				ExternalIPs: []string{nodePortExternalIP},
			},
		}
	} else {
		svc = corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("%s-tikv-%d-%s", tcName, id, extListener.Name),
				Labels:          MergeLabels(LabelsTikv(tcName), map[string]string{"statefulset.kubernetes.io/pod-name": fmt.Sprintf("basic-tikv-%d", id)}),
				Namespace:       tc.Namespace,
				OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
			},
			Spec: corev1.ServiceSpec{
				Selector: MergeLabels(LabelsTikv(tcName), map[string]string{"statefulset.kubernetes.io/pod-name": fmt.Sprintf("basic-tikv-%d", id)}),
				Type:     corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{
					{
						Name:       fmt.Sprintf("%s-%d-%s", tcName, id, extListener.Name),
						Port:       extListener.ContainerPort,
						NodePort:   nodePort,
						TargetPort: intstr.FromInt(int(20160)),
						Protocol:   corev1.ProtocolTCP,
					},
				},
				ExternalIPs: []string{nodePortExternalIP},
			},
		}
	}

	return &svc
}
