package sync

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ghodss/yaml"
	"golang.org/x/sync/errgroup"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	deprecated_dynamic "k8s.io/client-go/deprecated-dynamic"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configsv1alpha1 "github.com/openshift/static-config-operator/pkg/apis/staticcontent/v1alpha1"
	"github.com/openshift/static-config-operator/pkg/util/cmp"
)

// deleteOrphans looks for the "belongs-to-syncpod: yes" annotation, if found
// and object not in current db, remove it.
func (s *Sync) deleteOrphans() (reconcile.Result, error) {
	log.Info("Deleting orphan objects from the running cluster")
	done := map[schema.GroupKind]struct{}{}

	for _, gr := range s.grs {
		for version, resources := range gr.VersionedResources {
			for _, resource := range resources {
				if strings.ContainsRune(resource.Name, '/') { // no subresources
					continue
				}

				if !contains(resource.Verbs, "list") {
					continue
				}

				gvk := schema.GroupVersionKind{Group: gr.Group.Name, Version: version, Kind: resource.Kind}
				gk := gvk.GroupKind()
				if isDouble(gk) {
					continue
				}

				if gk.String() == "Endpoints" { // Services transfer their labels to Endpoints; ignore the latter
					continue
				}

				if _, found := done[gk]; found {
					continue
				}
				done[gk] = struct{}{}

				dc, err := s.dyn.ClientForGroupVersionKind(gvk)
				if err != nil {
					return reconcile.Result{}, err
				}

				o, err := dc.Resource(&resource, "").List(metav1.ListOptions{})
				if err != nil {
					return reconcile.Result{}, err
				}

				l, ok := o.(*unstructured.UnstructuredList)
				if !ok {
					continue
				}

				for _, i := range l.Items {
					// check that the object is marked by the sync pod
					l := i.GetLabels()
					if l[syncOwnedByPodLabelKey] == "true" {
						// if object is marked as owned by the sync pod
						// but not in current DB, OR marked as don't apply
						// then remove it
						_, inDB := s.store[keyFunc(i.GroupVersionKind().GroupKind(), i.GetNamespace(), i.GetName())]
						if !inDB {
							log.Info("Delete " + keyFunc(i.GroupVersionKind().GroupKind(), i.GetNamespace(), i.GetName()))
							err = dc.Resource(&resource, i.GetNamespace()).Delete(i.GetName(), nil)
							if err != nil {
								return reconcile.Result{}, err
							}
						}
					}
				}
			}
		}
	}

	return reconcile.Result{}, nil
}

// contains returns true if haystack contains needle
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ReadDB reads previously exported objects into a map via go-bindata as well as
// populating configuration items via translate().
func (s *Sync) readDB() error {
	s.store = map[string]unstructured.Unstructured{}

	var g errgroup.Group
	for _, asset := range AssetNames() {
		asset := asset // https://golang.org/doc/faq#closures_and_goroutines

		g.Go(func() error {
			b, err := Asset(asset)
			if err != nil {
				return err
			}

			o, err := unmarshal(b)
			if err != nil {
				log.Error(err, "unmarshal error", asset)
				return err
			}

			// defore we even add this object to store for sync,
			// check if platform matched and clean labels
			if !validatePlatform(s.config, o) {
				return nil
			}

			o, err = translateAsset(o, s.config)
			if err != nil {
				log.Error(err, "translateAsset error", asset)
				return err
			}
			s.storeLock.Lock()
			defer s.storeLock.Unlock()

			defaults(o)
			s.store[keyFunc(o.GroupVersionKind().GroupKind(), o.GetNamespace(), o.GetName())] = o

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	log.Info("finish translate")

	return nil
}

func validatePlatform(config *configsv1alpha1.Config, o unstructured.Unstructured) bool {
	// if no managed-cluster label set - return false. It is mandatory
	if len(o.GetLabels()[syncOpenshiftManagedClusterLabelKey]) == 0 {
		log.Error(fmt.Errorf("objectdoes not contains managed-cluster label key"), o.GetName())
		return false
	}
	// IF platform key is set and match config - return true
	// OR platform key is not set - true - all clouds
	if (len(o.GetLabels()[syncHiveClusterPlatformLabelKey]) > 0 &&
		strings.EqualFold(config.Spec.Platform, o.GetLabels()[syncHiveClusterPlatformLabelKey])) ||
		len(o.GetLabels()[syncHiveClusterPlatformLabelKey]) == 0 {
		return true
	}
	return false
}

// unmarshal has to reimplement yaml.unmarshal because it universally mangles yaml
// integers into float64s, whereas the Kubernetes client library uses int64s
// wherever it can.  Such a difference can cause us to update objects when
// we don't actually need to.
func unmarshal(b []byte) (unstructured.Unstructured, error) {
	json, err := yaml.YAMLToJSON(b)
	if err != nil {
		return unstructured.Unstructured{}, err
	}

	var o unstructured.Unstructured
	_, _, err = unstructured.UnstructuredJSONScheme.Decode(json, nil, &o)
	if err != nil {
		return unstructured.Unstructured{}, err
	}

	return o, nil
}

// updateDynamicClient updates the client's server API group resource
// information and dynamic client pool.
func (s *Sync) updateDynamicClient() error {
	grs, err := restmapper.GetAPIGroupResources(s.cli)
	if err != nil {
		return err
	}
	s.grs = grs

	rm := restmapper.NewDiscoveryRESTMapper(s.grs)
	s.dyn = deprecated_dynamic.NewClientPool(s.restconfig, rm, dynamic.LegacyAPIPathResolverFunc)

	return nil
}

// applyResources creates or updates all resources in db that match the provided
// filter.
func (s *Sync) applyResources(filter func(unstructured.Unstructured) bool, keys []string) error {
	for _, k := range keys {
		o := s.store[k]

		if !filter(o) {
			continue
		}

		if err := s.write(&o); err != nil {
			return err
		}
	}
	return nil
}

// write synchronises a single object with the API server.
func (s *Sync) write(o *unstructured.Unstructured) error {
	dc, err := s.dyn.ClientForGroupVersionKind(o.GroupVersionKind())
	if err != nil {
		return err
	}
	var gr *restmapper.APIGroupResources
	for _, g := range s.grs {
		if g.Group.Name == o.GroupVersionKind().Group {
			gr = g
			break
		}
	}
	if gr == nil {
		return errors.New("couldn't find group " + o.GroupVersionKind().Group)
	}

	o = o.DeepCopy()
	var res *metav1.APIResource
	for _, r := range gr.VersionedResources[o.GroupVersionKind().Version] {
		if gr.Group.Name == "template.openshift.io" && r.Name == "processedtemplates" {
			continue
		}
		if r.Kind == o.GroupVersionKind().Kind {
			res = &r
			break
		}
	}
	if res == nil {
		return errors.New("couldn't find kind " + o.GroupVersionKind().Kind)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() (err error) {
		var existing *unstructured.Unstructured
		existing, err = dc.Resource(res, o.GetNamespace()).Get(o.GetName(), metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			log.Info("Create " + keyFunc(o.GroupVersionKind().GroupKind(), o.GetNamespace(), o.GetName()))
			markSyncPodOwned(o)
			_, err = dc.Resource(res, o.GetNamespace()).Create(o)
			if kerrors.IsAlreadyExists(err) {
				// The "hot path" in write() is Get, check, then maybe Update.
				// Optimising for this has the disadvantage that at cluster
				// creation we can race with API server or controller
				// initialisation. Between Get returning NotFound and us trying
				// to Create, the object might be created.  In this case we
				// return a synthetic Conflict to force a retry.
				log.Error(err, "error while creating", o.GetNamespace(), o.GetName())
				err = kerrors.NewConflict(schema.GroupResource{Group: res.Group, Resource: res.Name}, o.GetName(), errors.New("synthetic"))
			}
			return
		}
		if err != nil {
			return
		}

		rv := existing.GetResourceVersion()

		err = clean(*existing)
		if err != nil {
			return
		}
		defaults(*existing)

		markSyncPodOwned(o)

		if !s.needsUpdate(existing, o) {
			return
		}
		printDiff(existing, o)

		o.SetResourceVersion(rv)
		_, err = dc.Resource(res, o.GetNamespace()).Update(o)
		if err != nil && strings.Contains(err.Error(), "updates to parameters are forbidden") {
			log.Info("object %s is not updateable, will delete and re-create", o.GetName())
			err = dc.Resource(res, o.GetNamespace()).Delete(o.GetName(), &metav1.DeleteOptions{})
			if err != nil {
				return
			}
			o.SetResourceVersion("")
			_, err = dc.Resource(res, o.GetNamespace()).Create(o)
		}

		return
	})

	return err
}

func printDiff(existing, o *unstructured.Unstructured) bool {
	// Don't show a diff if kind is Secret
	gk := o.GroupVersionKind().GroupKind()
	diffShown := false
	if gk.String() != "Secret" {
		if diff := cmp.Diff(*existing, *o); diff != "" {
			log.Info("diff", diff)
			diffShown = true
		}
	}
	return diffShown
}

// mark object as sync pod owned
func markSyncPodOwned(o *unstructured.Unstructured) {
	l := o.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[syncOwnedByPodLabelKey] = "true"
	o.SetLabels(l)
}
