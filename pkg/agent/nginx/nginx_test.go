package nginx

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
)

func zeroBackendPodCR() sync.ControllerResources {
	return sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "web",
				Annotations: map[string]string{
					apis.SvcAnnNetworkType: string(apis.NetworkTypePod),
					apis.SvcAnnProtolPort:  "http:80",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080)}},
			},
		},
		// 无端点 → GenBackends 返回空
	}
}

func TestApply_ZeroBackends_SkipsRender(t *testing.T) {
	home := t.TempDir() + "/"
	httpDir := filepath.Join(home, ConfigHttpPath)
	if err := os.MkdirAll(httpDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ConfigStreamPath), 0755); err != nil {
		t.Fatal(err)
	}

	na := &Agent{nginxHome: home} // nginx 路径为空，reload 会报错但被忽略
	_, _ = na.Apply(zeroBackendPodCR())

	if _, err := os.Stat(filepath.Join(httpDir, "default_web_http_80.conf")); !os.IsNotExist(err) {
		t.Error("zero-backend listener should not produce a rendered conf (would be an invalid empty upstream)")
	}
}

func TestApply_ZeroBackends_PreservesExisting(t *testing.T) {
	home := t.TempDir() + "/"
	httpDir := filepath.Join(home, ConfigHttpPath)
	if err := os.MkdirAll(httpDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ConfigStreamPath), 0755); err != nil {
		t.Fatal(err)
	}

	// 预置上一轮的“好”配置
	confFile := filepath.Join(httpDir, "default_web_http_80.conf")
	upFile := filepath.Join(httpDir, "default_web_http_80_upstream.conf")
	for _, f := range []string{confFile, upFile} {
		if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	na := &Agent{nginxHome: home}
	_, _ = na.Apply(zeroBackendPodCR())

	// 零后端时应保留已有配置，不被 GC 删除
	if _, err := os.Stat(confFile); err != nil {
		t.Error("existing listener conf should be preserved on zero backends:", err)
	}
	if _, err := os.Stat(upFile); err != nil {
		t.Error("existing listener upstream should be preserved on zero backends:", err)
	}
}

func TestApply_GenBackendsError_PreservesExisting(t *testing.T) {
	home := t.TempDir() + "/"
	httpDir := filepath.Join(home, ConfigHttpPath)
	if err := os.MkdirAll(httpDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ConfigStreamPath), 0755); err != nil {
		t.Fatal(err)
	}

	// 预置上一轮的“好”配置
	confFile := filepath.Join(httpDir, "default_web_http_80.conf")
	upFile := filepath.Join(httpDir, "default_web_http_80_upstream.conf")
	for _, f := range []string{confFile, upFile} {
		if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// network-type 非法 → GenBackends 返回 error；但带有 protocol 注解使 listener 可定位
	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "web",
				Annotations: map[string]string{
					apis.SvcAnnNetworkType: "bogus",
					apis.SvcAnnProtolPort:  "http:80",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080)}},
			},
		},
	}

	na := &Agent{nginxHome: home}
	if _, err := na.Apply(cr); err == nil {
		t.Error("Apply should surface the GenBackends error")
	}

	// 解析失败时应保留已有配置，不被 GC 删除
	if _, err := os.Stat(confFile); err != nil {
		t.Error("existing listener conf should be preserved on parse error:", err)
	}
	if _, err := os.Stat(upFile); err != nil {
		t.Error("existing listener upstream should be preserved on parse error:", err)
	}
}

func TestWriteFileIfChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.conf")

	// 首次写入：文件不存在 → 应写入并报告 changed=true
	changed, err := writeFileIfChanged(path, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("first write should report changed=true")
	}

	// 相同内容：应跳过写入（changed=false），且不改动 mtime
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = writeFileIfChanged(path, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("identical content should report changed=false")
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("unchanged content must not rewrite the file (mtime changed)")
	}

	// 内容不同：应写入并报告 changed=true
	changed, err = writeFileIfChanged(path, []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("different content should report changed=true")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "b" {
		t.Errorf("file content = %q, want %q", got, "b")
	}
}

func TestGCRemovesCertsByPrefix(t *testing.T) {
	home := t.TempDir() + "/"
	tlsDir := filepath.Join(home, TLSFilePath)
	for _, d := range []string{
		filepath.Join(home, ConfigHttpPath),
		filepath.Join(home, ConfigStreamPath),
		tlsDir,
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// 本 Service 的证书 + 另一个 Service 的同域名证书
	myCert := filepath.Join(tlsDir, "default_web_example.com.crt")
	myKey := filepath.Join(tlsDir, "default_web_example.com.key")
	otherCert := filepath.Join(tlsDir, "default_api_example.com.crt")
	for _, f := range []string{myCert, myKey, otherCert} {
		if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	na := &Agent{nginxHome: home}
	// keep=nil：模拟 Delete，删除 default_web 的全部文件（含证书）
	if _, err := na.gcServiceFiles(svcPrefix("default", "web"), nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(myCert); !os.IsNotExist(err) {
		t.Error("service cert should be removed on cleanup")
	}
	if _, err := os.Stat(myKey); !os.IsNotExist(err) {
		t.Error("service private key must not leak on cleanup")
	}
	// 前缀带末尾下划线，default_web_ 不会误伤 default_api_
	if _, err := os.Stat(otherCert); err != nil {
		t.Error("another service's cert must remain:", err)
	}
}

func TestApply_SkipsReloadWhenUnchanged(t *testing.T) {
	home := t.TempDir() + "/"
	for _, d := range []string{
		filepath.Join(home, ConfigHttpPath),
		filepath.Join(home, ConfigStreamPath),
		filepath.Join(home, TLSFilePath),
	} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// 假 nginx：每次 `-s reload` 往日志追加一行，便于统计 reload 次数；其余（如 -t）直接成功
	reloadLog := filepath.Join(t.TempDir(), "reload.log")
	nginxBin := filepath.Join(t.TempDir(), "nginx")
	script := "#!/bin/sh\nif [ \"$1\" = \"-s\" ]; then echo reload >> " + reloadLog + "; fi\nexit 0\n"
	if err := os.WriteFile(nginxBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	reloadCount := func() int {
		data, err := os.ReadFile(reloadLog)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "reload")
	}

	cr := sync.ControllerResources{
		Service: corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "web",
				Annotations: map[string]string{
					apis.SvcAnnNetworkType: string(apis.NetworkTypeNodeport),
					apis.SvcAnnProtolPort:  "http:80",
				},
			},
			Spec: corev1.ServiceSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
				Ports:                 []corev1.ServicePort{{Name: "http", Port: 80, NodePort: 30080}},
			},
		},
		Nodes: []corev1.Node{node("n1", "10.0.0.1", "")},
	}

	na := &Agent{nginx: nginxBin, nginxHome: home}

	// 第一次：有文件写入 → 应 reload 一次
	if _, err := na.Apply(cr); err != nil {
		t.Fatal(err)
	}
	if n := reloadCount(); n != 1 {
		t.Fatalf("first apply should reload once, got %d", n)
	}

	// 第二次：渲染内容完全相同 → 跳过 reload，计数不变
	if _, err := na.Apply(cr); err != nil {
		t.Fatal(err)
	}
	if n := reloadCount(); n != 1 {
		t.Fatalf("unchanged apply should skip reload (still 1), got %d", n)
	}

	// 第三次：无变更，但 ForceReload=true → 强制 reload
	forced := cr
	forced.ForceReload = true
	if _, err := na.Apply(forced); err != nil {
		t.Fatal(err)
	}
	if n := reloadCount(); n != 2 {
		t.Fatalf("ForceReload should reload even when unchanged (2), got %d", n)
	}

	// 第四次：后端变化 → 配置变更 → 再次 reload
	cr.Nodes = append(cr.Nodes, node("n2", "10.0.0.2", ""))
	if _, err := na.Apply(cr); err != nil {
		t.Fatal(err)
	}
	if n := reloadCount(); n != 3 {
		t.Fatalf("changed apply should reload again (3), got %d", n)
	}
}

// node 构造一个带 InternalIP 的 Ready 节点（nginx 包内测试用）。
func node(name, internalIP, externalIP string) corev1.Node {
	addrs := []corev1.NodeAddress{}
	if internalIP != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: internalIP})
	}
	if externalIP != "" {
		addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: externalIP})
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses:  addrs,
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestSvcPrefix(t *testing.T) {
	if got := svcPrefix("default", "web"); got != "default_web_" {
		t.Fatalf("got %q", got)
	}
}

func TestListAndGC(t *testing.T) {
	home := t.TempDir() + "/"
	httpDir := filepath.Join(home, ConfigHttpPath)
	streamDir := filepath.Join(home, ConfigStreamPath)
	if err := os.MkdirAll(httpDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(streamDir, 0755); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		filepath.Join(httpDir, "default_web_http_80.conf"):               "",
		filepath.Join(httpDir, "default_web_http_80_upstream.conf"):      "",
		filepath.Join(streamDir, "kube-system_dns_tcp_53.conf"):          "",
		filepath.Join(streamDir, "kube-system_dns_tcp_53_upstream.conf"): "",
	}
	for f := range files {
		if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	na := &Agent{nginxHome: home}

	refs, err := na.List()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range refs {
		got = append(got, r.Namespace+"/"+r.Name)
	}
	sort.Strings(got)
	want := []string{"default/web", "kube-system/dns"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List got %v, want %v", got, want)
	}

	// gc default_web 的全部文件（keep 传 nil）
	if _, err := na.gcServiceFiles(svcPrefix("default", "web"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(httpDir, "default_web_http_80.conf")); !os.IsNotExist(err) {
		t.Error("default_web file should have been removed")
	}
	if _, err := os.Stat(filepath.Join(streamDir, "kube-system_dns_tcp_53.conf")); err != nil {
		t.Error("kube-system_dns file should remain:", err)
	}
}

func TestGCKeepsFilesInKeepList(t *testing.T) {
	home := t.TempDir() + "/"
	httpDir := filepath.Join(home, ConfigHttpPath)
	if err := os.MkdirAll(httpDir, 0755); err != nil {
		t.Fatal(err)
	}
	keepFile := filepath.Join(httpDir, "default_web_http_80.conf")
	staleFile := filepath.Join(httpDir, "default_web_http_8080.conf")
	for _, f := range []string{keepFile, staleFile} {
		if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	na := &Agent{nginxHome: home}
	if _, err := na.gcServiceFiles(svcPrefix("default", "web"), []string{keepFile}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keepFile); err != nil {
		t.Error("keep file should remain:", err)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Error("stale file should be removed")
	}
}
