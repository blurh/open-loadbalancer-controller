package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestCRDSchemaIsStructural(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "install", "crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(data, crd); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Scope != apiextensionsv1.NamespaceScoped || crd.Spec.Group != GroupName {
		t.Fatal("CRD scope/group differ from Go types")
	}
	for _, version := range crd.Spec.Versions {
		if version.Name != GroupVersion.Version || version.Subresources == nil || version.Subresources.Status == nil {
			t.Fatal("status subresource/version missing")
		}
		internal := &apiextensions.JSONSchemaProps{}
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, internal, nil); err != nil {
			t.Fatal(err)
		}
		structural, err := schema.NewStructural(internal)
		if err != nil {
			t.Fatal(err)
		}
		if errs := schema.ValidateStructural(field.NewPath("openAPIV3Schema"), structural); len(errs) != 0 {
			t.Fatal(errs)
		}
	}
}
