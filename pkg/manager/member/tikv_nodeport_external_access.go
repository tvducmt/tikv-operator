package member

import (
	"fmt"

	"github.com/tikv/tikv-operator/pkg/apis/tikv/v1alpha1"
	"github.com/tikv/tikv-operator/pkg/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func getNewNodeportServiceForTikvCluster(tc *v1alpha1.TikvCluster, id int32, extListener v1alpha1.ExternalListenerConfig, nodePortExternalIP string) *corev1.Service {
	ns := tc.Namespace
	tcName := tc.Name
	nodePort := int32(0)
	if extListener.ExternalStartingPort > 0 {
		nodePort = extListener.ExternalStartingPort + id
	}
	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-%d-%s", tcName, id, extListener.Name),
			Labels:          MergeLabels(LabelsTikv(tcName), map[string]string{"statefulset.kubernetes.io/pod-name": fmt.Sprintf("basic-tikv-%d", id)}),
			Namespace:       ns,
			OwnerReferences: []metav1.OwnerReference{controller.GetOwnerRef(tc)},
		},
		Spec: corev1.ServiceSpec{
			Selector: MergeLabels(LabelsTikv(tcName), map[string]string{"statefulset.kubernetes.io/pod-name": fmt.Sprintf("basic-tikv-%d", id)}),
			Type:     corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{
				{
					Name:       fmt.Sprintf("basic-tikv-%d", id),
					Port:       extListener.ContainerPort,
					NodePort:   nodePort,
					TargetPort: intstr.FromInt(int(20160)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			ExternalIPs: []string{nodePortExternalIP},
		},
	}

	return &svc
}
