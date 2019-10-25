package sync

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// resource filters
var (
	crdFilter = func(o unstructured.Unstructured) bool {
		return o.GroupVersionKind().GroupKind() == schema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}
	}
	nsFilter = func(o unstructured.Unstructured) bool {
		return o.GroupVersionKind().GroupKind() == schema.GroupKind{Kind: "Namespace"}
	}
	saFilter = func(o unstructured.Unstructured) bool {
		return o.GroupVersionKind().GroupKind() == schema.GroupKind{Kind: "ServiceAccount"}
	}
	cfgFilter = func(o unstructured.Unstructured) bool {
		return o.GroupVersionKind().GroupKind() == schema.GroupKind{Kind: "Secret"} ||
			o.GroupVersionKind().GroupKind() == schema.GroupKind{Kind: "ConfigMap"}
	}
	storageClassFilter = func(o unstructured.Unstructured) bool {
		return o.GroupVersionKind().GroupKind() == schema.GroupKind{Group: "storage.k8s.io", Kind: "StorageClass"}
	}
	everythingElseFilter = func(o unstructured.Unstructured) bool {
		return !crdFilter(o) &&
			!nsFilter(o) &&
			!saFilter(o) &&
			!cfgFilter(o) &&
			!storageClassFilter(o)
	}
)
