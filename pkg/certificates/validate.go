// Package certificates 校验 controller 与 agent 之间的证书输入，不持久化私钥。
package certificates

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"

	"github.com/blurh/open-loadbalancer-controller/pkg/agent/parse"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	"github.com/blurh/open-loadbalancer-controller/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func Parse(secret corev1.Secret) (*x509.Certificate, error) {
	if secret.Type != corev1.SecretTypeTLS {
		return nil, fmt.Errorf("Secret %s/%s must have type kubernetes.io/tls", secret.Namespace, secret.Name)
	}
	pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, fmt.Errorf("invalid TLS Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("invalid certificate in Secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	if len(cert.DNSNames) == 0 {
		return nil, fmt.Errorf("TLS Secret %s/%s has no DNS names", secret.Namespace, secret.Name)
	}
	for _, domain := range cert.DNSNames {
		// 域名既进入 nginx server_name，也用于文件名，禁止路径和配置语法字符。
		if len(validation.IsDNS1123Subdomain(strings.ToLower(strings.TrimPrefix(domain, "*.")))) != 0 {
			return nil, fmt.Errorf("TLS Secret %s/%s has an invalid DNS name", secret.Namespace, secret.Name)
		}
	}
	return cert, nil
}

// Validate 在任何文件修改前检查完整集合，防止缺失一个 Secret 时下发部分 HTTPS 配置。
func Validate(cr sync.ControllerResources) error {
	wanted := make(map[string]bool)
	for _, name := range utils.SplitCommaTrimSpace(cr.Service.Annotations[apis.SvcAnnCertSecrets]) {
		if name != "" {
			wanted[name] = true
		}
	}
	for _, port := range cr.Service.Spec.Ports {
		protocol, err := parse.FrontendProtocol(cr, port)
		if err != nil {
			return err
		}
		if protocol == "https" && len(wanted) == 0 {
			return fmt.Errorf("HTTPS Service %s/%s requires cert-secrets", cr.Service.Namespace, cr.Service.Name)
		}
	}
	seen := make(map[string]bool)
	domains := make(map[string]string)
	for _, secret := range cr.Secrets {
		if secret.Namespace != cr.Service.Namespace || !wanted[secret.Name] || seen[secret.Name] {
			return fmt.Errorf("unexpected or duplicate Secret %s/%s", secret.Namespace, secret.Name)
		}
		cert, err := Parse(secret)
		if err != nil {
			return err
		}
		for _, domain := range cert.DNSNames {
			key := strings.ToLower(domain)
			if owner, ok := domains[key]; ok && owner != secret.Name {
				return fmt.Errorf("TLS Secrets %s and %s both declare DNS name %s", owner, secret.Name, domain)
			}
			domains[key] = secret.Name
		}
		seen[secret.Name] = true
	}
	for name := range wanted {
		if !seen[name] {
			return fmt.Errorf("required TLS Secret %s/%s is missing", cr.Service.Namespace, name)
		}
	}
	return nil
}
