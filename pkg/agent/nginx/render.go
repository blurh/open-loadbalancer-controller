package nginx

import (
	"bytes"
	"os"
	"text/template"

	"github.com/blurh/open-loadbalancer-controller/pkg/certificates"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

// listenerConfigFiles 返回某 listener 的 server / upstream 两个配置文件路径，
// 命名规则与 Render* 保持一致。
func listenerConfigFiles(dir, prefix, key, port string) []string {
	base := dir + prefix + key + "_" + port
	return []string{base + ".conf", base + "_upstream.conf"}
}

// renderTemplate 把 data 渲染进模板并返回字节，不落盘。
func renderTemplate(name, text string, data any) ([]byte, error) {
	tmpl, err := template.New(name).Parse(text)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileIfChanged 仅在磁盘内容与 data 不一致时才写入，返回是否实际写入。
// 内容无变更则跳过，避免无谓改写文件（mtime 抖动、触发文件监听等）。
func writeFileIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := atomicWrite(path, data); err != nil {
		return false, err
	}
	return true, nil
}

func RenderStreamFile(dir, namePrefix string, streamConfig StreamConfig, existsFiles *[]string) (bool, error) {
	changed := false
	// 渲染 server: <dir>/<ns>_<name>_tcp_3000.conf
	configFile := dir + namePrefix + streamConfig.Protocol + "_" + streamConfig.ListenerPort + ".conf"
	serverData, err := renderTemplate("streamserver", templStreamServer, streamConfig)
	if err != nil {
		return changed, err
	}
	c, err := writeFileIfChanged(configFile, serverData)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	// 渲染 upstream: <dir>/<ns>_<name>_tcp_3000_upstream.conf
	upstreamFile := dir + namePrefix + streamConfig.Protocol + "_" + streamConfig.ListenerPort + "_upstream.conf"
	upstreamData, err := renderTemplate("streamupstream", templStreamUpstream, streamConfig)
	if err != nil {
		return changed, err
	}
	c, err = writeFileIfChanged(upstreamFile, upstreamData)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	*existsFiles = append(*existsFiles, configFile, upstreamFile)
	return changed, nil
}

func RenderHttpFile(dir, namePrefix string, httpConfig HttpConfig, existsFiles *[]string) (bool, error) {
	changed := false
	// <dir>/<ns>_<name>_http_80.conf
	configFile := dir + namePrefix + "http_" + httpConfig.ListenerPort + ".conf"
	serverData, err := renderTemplate("httpserver", templHttpServer, httpConfig)
	if err != nil {
		return changed, err
	}
	c, err := writeFileIfChanged(configFile, serverData)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	// <dir>/<ns>_<name>_http_80_upstream.conf
	upstreamFile := dir + namePrefix + "http_" + httpConfig.ListenerPort + "_upstream.conf"
	upstreamData, err := renderTemplate("httpupstream", templHttpUpstream, httpConfig)
	if err != nil {
		return changed, err
	}
	c, err = writeFileIfChanged(upstreamFile, upstreamData)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	*existsFiles = append(*existsFiles, configFile, upstreamFile)
	return changed, nil
}

// RenderHttpsFile 把多个域名渲染进同一个 <ns>_<name>_https_443.conf（每个域名一个
// server 块，靠 SNI 区分），共享同一个 upstream。
func RenderHttpsFile(dir, namePrefix string, httpConfig HttpConfig, domains []string, existsFiles *[]string) (bool, error) {
	changed := false
	configFile := dir + namePrefix + "https_" + httpConfig.ListenerPort + ".conf"
	tmpl, err := template.New("httpsserver").Parse(templHttpServer)
	if err != nil {
		return changed, err
	}
	// 多个域名 server 块拼进同一个 buffer，整体比对后一次性写入
	var serverBuf bytes.Buffer
	for _, domain := range domains {
		cfg := httpConfig
		cfg.ServerName = domain
		cfg.CertFile = TLSFilePath + namePrefix + domain + ".crt"
		cfg.CertKeyFile = TLSFilePath + namePrefix + domain + ".key"
		if err := tmpl.Execute(&serverBuf, cfg); err != nil {
			return changed, err
		}
	}
	c, err := writeFileIfChanged(configFile, serverBuf.Bytes())
	if err != nil {
		return changed, err
	}
	changed = changed || c

	upstreamFile := dir + namePrefix + "https_" + httpConfig.ListenerPort + "_upstream.conf"
	upstreamData, err := renderTemplate("httpsupstream", templHttpUpstream, httpConfig)
	if err != nil {
		return changed, err
	}
	c, err = writeFileIfChanged(upstreamFile, upstreamData)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	*existsFiles = append(*existsFiles, configFile, upstreamFile)
	return changed, nil
}

// RenderSecrets 把 cr.Secrets 中的证书写到 tls 目录，文件按 `<prefix><domain>.{crt,key}`
// 命名，使证书归属于该 Service、可被 gcServiceFiles 按前缀回收（删除/换域名时不留私钥）。
func RenderSecrets(tlsFilePath, namePrefix string, cr sync.ControllerResources, existsFile *[]string) ([]string, bool, error) {
	if err := certificates.Validate(cr); err != nil {
		return nil, false, err
	}
	domains := []string{}
	changed := false
	crSecrets := cr.Secrets
	for _, secret := range crSecrets {
		// secret.Data 经 client-go 已解码为原始 PEM 字节，无需再做 base64 解码
		certCrt := secret.Data["tls.crt"]
		certKey := secret.Data["tls.key"]

		certBody, err := certificates.Parse(secret)
		if err != nil {
			return domains, changed, err
		}
		for _, domain := range certBody.DNSNames {
			domains = append(domains, domain)
			for _, item := range []struct {
				suffix string
				data   []byte
			}{
				{".crt", certCrt},
				{".key", certKey},
			} {
				certFile := tlsFilePath + namePrefix + domain + item.suffix
				c, err := writeFileIfChanged(certFile, item.data)
				if err != nil {
					return domains, changed, err
				}
				changed = changed || c
				*existsFile = append(*existsFile, certFile)
			}
		}
	}

	return domains, changed, nil
}
