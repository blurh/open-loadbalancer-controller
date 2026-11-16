package apis

// LoadBalancerClass 是 Service.Spec.LoadBalancerClass 的值
// 用于标识此 Service 应由 openlb 控制器处理
const LoadBalancerClass = "kubernetes.io/openlb"

// Service 注解 key，controller 与 agent 共用。
const (
	SvcAnnInstances       = "kubernetes.io/openlb.instances"
	SvcAnnClass           = "kubernetes.io/openlb.class"
	SvcAnnProtolPort      = "kubernetes.io/openlb.protocol-port"
	SvcAnnCertSecrets     = "kubernetes.io/openlb.cert-secrets"
	SvcAnnNetworkType     = "kubernetes.io/openlb.network-type"
	SvcAnnNodeNetworkType = "kubernetes.io/openlb.node-network-type"
)

type NetworkType string

const (
	NetworkTypeNodeport NetworkType = "nodeport"
	NetworkTypePod      NetworkType = "pod"
)

type NodeNetworkType string

const (
	NodeNetworkTypeInternal NodeNetworkType = "internal"
	NodeNetworkTypeExternal NodeNetworkType = "external"
)
