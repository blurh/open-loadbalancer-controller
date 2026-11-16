// Package v1alpha1 定义 controller 的持久化同步记录。
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	GroupName             = "openlb.io"
	CleanupFinalizer      = GroupName + "/agent-cleanup"
	CertificateFinalizer  = GroupName + "/certificate-protection"
	InstancePendingApply  = "PendingApply"
	InstanceApplied       = "Applied"
	InstancePendingDelete = "PendingDelete"
	InstanceCleaned       = "Cleaned"
)

var GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &OpenLBServiceState{}, &OpenLBServiceStateList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

type ServiceReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

type ServiceStateSpec struct {
	ServiceRef ServiceReference `json:"serviceRef"`
}

type InstanceState struct {
	Address string `json:"address"`
	Phase   string `json:"phase"`
	// ConfigHash 只包含转发配置，排除资源版本、status 和 finalizer 等元数据。
	ConfigHash string                       `json:"configHash,omitempty"`
	LastError  string                       `json:"lastError,omitempty"`
	Ingress    []corev1.LoadBalancerIngress `json:"ingress,omitempty"`
}

type ServiceStateStatus struct {
	// CertificateRefs 包含当前和可能已下发的证书；所有 agent 确认切换后才能收缩。
	// Secret 与记录同 namespace，仅保存身份，不复制证书或私钥。
	CertificateRefs []SecretReference  `json:"certificateRefs,omitempty"`
	Instances       []InstanceState    `json:"instances,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}

type SecretReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// OpenLBServiceState 与 Service 同名且同 namespace；spec 只保存身份，期望配置仍来自 Service。
type OpenLBServiceState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServiceStateSpec   `json:"spec"`
	Status            ServiceStateStatus `json:"status,omitempty"`
}

type OpenLBServiceStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OpenLBServiceState `json:"items"`
}

func (in *ServiceStateStatus) DeepCopy() *ServiceStateStatus {
	if in == nil {
		return nil
	}
	out := *in
	if in.CertificateRefs != nil {
		out.CertificateRefs = append([]SecretReference{}, in.CertificateRefs...)
	}
	if in.Instances != nil {
		out.Instances = make([]InstanceState, len(in.Instances))
		copy(out.Instances, in.Instances)
		for i := range in.Instances {
			if in.Instances[i].Ingress != nil {
				out.Instances[i].Ingress = make([]corev1.LoadBalancerIngress, len(in.Instances[i].Ingress))
				for j := range in.Instances[i].Ingress {
					in.Instances[i].Ingress[j].DeepCopyInto(&out.Instances[i].Ingress[j])
				}
			}
		}
	}
	if in.Conditions != nil {
		out.Conditions = append([]metav1.Condition{}, in.Conditions...)
	}
	return &out
}

func (in *OpenLBServiceState) DeepCopy() *OpenLBServiceState {
	if in == nil {
		return nil
	}
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status = *in.Status.DeepCopy()
	return &out
}

func (in *OpenLBServiceState) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

func (in *OpenLBServiceStateList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]OpenLBServiceState, len(in.Items))
		for i := range in.Items {
			out.Items[i] = *in.Items[i].DeepCopy()
		}
	}
	return &out
}
