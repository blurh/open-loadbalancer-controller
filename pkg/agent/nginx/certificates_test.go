package nginx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blurh/open-loadbalancer-controller/internal/testutil"
	"github.com/blurh/open-loadbalancer-controller/pkg/apis"
	"github.com/blurh/open-loadbalancer-controller/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
)

func certificateAgent(t *testing.T) *Agent {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "nginx-test")
	// 通过环境变量分别注入语法检查和 reload 失败。
	script := "#!/bin/sh\nif [ \"$1\" = \"$OPENLB_TEST_FAIL_COMMAND\" ]; then exit 1; fi\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENLB_NGINX_HOME", home)
	t.Setenv("OPENLB_NGINX_BIN", bin)
	t.Setenv("OPENLB_TEST_FAIL_COMMAND", "")
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func httpsResources(t *testing.T, secretName, domain string) sync.ControllerResources {
	t.Helper()
	cr := zeroBackendPodCR()
	cr.Service.Spec.Ports[0].Port = 443
	cr.Service.Annotations[apis.SvcAnnProtolPort] = "https:443"
	cr.Service.Annotations[apis.SvcAnnCertSecrets] = secretName
	cr.Secrets = []corev1.Secret{*testutil.TLSSecret(t, "default", "tls", domain)}
	cr.Secrets[0].Name = secretName
	ready := true
	cr.EndpointSlice.Endpoints = []discovery.Endpoint{{Addresses: []string{"10.0.0.2"}, Conditions: discovery.EndpointConditions{Ready: &ready}}}
	return cr
}

func managedContents(t *testing.T, a *Agent) map[string]string {
	t.Helper()
	files := make(map[string]string)
	for _, dir := range a.managedDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files[path] = string(data)
		}
	}
	return files
}

func TestInvalidCertificateSetPreservesCompleteConfiguration(t *testing.T) {
	for _, failure := range []string{"missing", "partial", "no-reference", "bad-pem", "missing-key", "mismatched-key", "wrong-type", "cross-namespace", "zero-backends"} {
		t.Run(failure, func(t *testing.T) {
			a := certificateAgent(t)
			cr := httpsResources(t, "tls", "old.example.com")
			if _, err := a.Apply(cr); err != nil {
				t.Fatal(err)
			}
			before := managedContents(t, a)
			bad := httpsResources(t, "tls", "new.example.com")
			switch failure {
			case "missing":
				bad.Secrets = nil
			case "partial":
				bad.Service.Annotations[apis.SvcAnnCertSecrets] = "tls,another"
			case "no-reference":
				bad.Service.Annotations[apis.SvcAnnCertSecrets] = ""
				bad.Secrets = nil
			case "bad-pem":
				bad.Secrets[0].Data[corev1.TLSCertKey] = []byte("invalid")
			case "missing-key":
				delete(bad.Secrets[0].Data, corev1.TLSPrivateKeyKey)
			case "mismatched-key":
				bad.Secrets[0].Data[corev1.TLSPrivateKeyKey] = cr.Secrets[0].Data[corev1.TLSPrivateKeyKey]
			case "wrong-type":
				bad.Secrets[0].Type = corev1.SecretTypeOpaque
			case "cross-namespace":
				bad.Secrets[0].Namespace = "other"
			case "zero-backends":
				bad.EndpointSlice.Endpoints = nil
			}
			if _, err := a.Apply(bad); err == nil {
				t.Fatal("invalid/unfinished HTTPS configuration reported success")
			}
			if after := managedContents(t, a); !reflect.DeepEqual(before, after) {
				t.Fatal("failure changed previous server/upstream/certificate/key files")
			}
		})
	}
}

func TestCertificateTransactionRollbackAndRetry(t *testing.T) {
	for _, failure := range []string{"-t", "-s"} {
		t.Run(failure, func(t *testing.T) {
			a := certificateAgent(t)
			old := httpsResources(t, "old", "old.example.com")
			if _, err := a.Apply(old); err != nil {
				t.Fatal(err)
			}
			other := httpsResources(t, "other", "other.example.com")
			other.Service.Name = "other"
			if _, err := a.Apply(other); err != nil {
				t.Fatal(err)
			}
			before := managedContents(t, a)
			next := httpsResources(t, "next", "new.example.com")
			t.Setenv("OPENLB_TEST_FAIL_COMMAND", failure)
			if _, err := a.Apply(next); err == nil {
				t.Fatal("expected nginx failure")
			}
			if !reflect.DeepEqual(before, managedContents(t, a)) {
				t.Fatal("failed rotation did not restore all managed files")
			}
			t.Setenv("OPENLB_TEST_FAIL_COMMAND", "")
			if _, err := a.Apply(next); err != nil {
				t.Fatal(err)
			}
			after := managedContents(t, a)
			for path, data := range after {
				if strings.Contains(path, "old.example.com") {
					t.Fatal("old certificate retained after successful rotation")
				}
				if strings.Contains(path, "default_other_") && before[path] != data {
					t.Fatal("rotation changed another Service")
				}
			}
			key := filepath.Join(a.nginxHome, TLSFilePath, "default_web_new.example.com.key")
			info, err := os.Stat(key)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("private key permissions: %v, %v", info, err)
			}
			t.Setenv("OPENLB_TEST_FAIL_COMMAND", failure)
			ref := sync.ServiceRef{Namespace: "default", Name: "web"}
			if err := a.Delete(ref); err == nil {
				t.Fatal("expected delete failure")
			}
			if !reflect.DeepEqual(after, managedContents(t, a)) {
				t.Fatal("failed cleanup did not restore configuration and certificates")
			}
			t.Setenv("OPENLB_TEST_FAIL_COMMAND", "")
			if err := a.Delete(ref); err != nil {
				t.Fatal(err)
			}
			for path := range managedContents(t, a) {
				if strings.Contains(path, "default_web_") {
					t.Fatal("successful cleanup retained Service files")
				}
			}
		})
	}
}

func TestInterruptedCertificateTransactionRecoveredBeforeStartup(t *testing.T) {
	a := certificateAgent(t)
	old := httpsResources(t, "old", "old.example.com")
	if _, err := a.Apply(old); err != nil {
		t.Fatal(err)
	}
	before := managedContents(t, a)
	if err := a.beginTransaction("default_web_"); err != nil {
		t.Fatal(err)
	}
	// 模拟写了一半文件后进程退出，没有机会执行 defer 回滚。
	if _, err := a.gcServiceFiles("default_web_", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.nginxHome, TLSFilePath, "default_web_new.example.com.crt"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, managedContents(t, restarted)) {
		t.Fatal("restart did not recover last complete configuration")
	}
	if _, err := os.Stat(filepath.Join(a.nginxHome, transactionFile)); !os.IsNotExist(err) {
		t.Fatal("completed recovery left journal")
	}
	if err := os.WriteFile(filepath.Join(a.nginxHome, transactionFile), []byte("broken journal"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(); err == nil {
		t.Fatal("corrupt recovery state must fail startup")
	}
}
