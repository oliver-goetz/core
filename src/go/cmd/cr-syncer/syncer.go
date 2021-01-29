// Copyright 2019 The Cloud Robotics Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	crdtypes "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// Annotations attached to CRDs.
	annotationStatusSubtree     = "cr-syncer.cloudrobotics.com/status-subtree"
	annotationFilterByRobotName = "cr-syncer.cloudrobotics.com/filter-by-robot-name"
	annotationSpecSource        = "cr-syncer.cloudrobotics.com/spec-source"

	// Annotations and labels attached to CRs.
	labelRobotName = "cloudrobotics.com/robot-name"
	// Annotation for remote resource version. Note that for resources in
	// the cloud cluster, this is a resource version on the robot's cluster
	// (and vice versa). This will only be set when the status subresource
	// is disabled, otherwise the status and annotation cannot be updated
	// in a single request.
	annotationResourceVersion = "cr-syncer.cloudrobotics.com/remote-resource-version"
)

var (
	mSyncs = stats.Int64(
		"cr-syncer.cloudrobotics.com/syncs",
		"Synchronizations triggered by resource events",
		stats.UnitDimensionless,
	)
	mSyncErrors = stats.Int64(
		"cr-syncer.cloudrobotics.com/sync_errors",
		"Synchronization errors on resource events",
		stats.UnitDimensionless,
	)
	tagEventSource = mustNewTagKey("event_source")
	tagResource    = mustNewTagKey("resource")
)

func init() {
	if err := view.Register(
		&view.View{
			Name:        "cr-syncer.cloudrobotics.com/syncs_total",
			Description: "Total number of synchronizations triggered resource events",
			Measure:     mSyncs,
			TagKeys:     []tag.Key{tagEventSource, tagResource},
			Aggregation: view.Count(),
		},
		&view.View{
			Name:        "cr-syncer.cloudrobotics.com/sync_errors_total",
			Description: "Total number of synchronizations errors on resource events",
			Measure:     mSyncErrors,
			TagKeys:     []tag.Key{tagEventSource, tagResource},
			Aggregation: view.Count(),
		},
	); err != nil {
		panic(err)
	}
}

// removeFinalizer removes the cr-syncer finalizer for this robot. Finalizers
// for offline robots have to be removed manually (eg with `kubectl edit`).
// TODO(rodrigoq): remove after migration
func removeFinalizer(client dynamic.ResourceInterface, obj *unstructured.Unstructured, clusterName string) {
	update := false
	thisFinalizer := fmt.Sprintf("%s.synced.cr-syncer.cloudrobotics.com", clusterName)
	finalizers := []string{}
	for _, x := range obj.GetFinalizers() {
		if x == thisFinalizer {
			update = true
		} else {
			finalizers = append(finalizers, x)
		}
	}
	if !update {
		return
	}
	obj.SetFinalizers(finalizers)
	if _, err := client.Update(obj, metav1.UpdateOptions{}); err != nil {
		if isNotFoundError(err) {
			return
		}
		log.Printf("failed to remove finalizers: %v", err)
	}
}

// crSyncer synchronizes custom resources from an upstream source cluster to a
// downstream cluster.
// Updates to the status subresource in the downstream are propagated back to
// the upstream cluster.
type crSyncer struct {
	clusterName   string // Name of downstream cluster.
	crd           crdtypes.CustomResourceDefinition
	upstream      dynamic.ResourceInterface // Source of the spec.
	downstream    dynamic.ResourceInterface // Source of the status.
	labelSelector string
	subtree       string

	// Informers and the queues they feed. Upstream/downstream describes
	// the source of the change events, _not_ the direction they are heading.
	// For example, upstream{Inf,Queue} receive updates that will result in the
	// syncer taking actions against the downstream cluster.
	upstreamInf     cache.SharedIndexInformer
	downstreamInf   cache.SharedIndexInformer
	upstreamQueue   workqueue.RateLimitingInterface
	downstreamQueue workqueue.RateLimitingInterface

	done chan struct{} // Terminates all background processes.
}

func newCRSyncer(
	crd crdtypes.CustomResourceDefinition,
	local, remote dynamic.Interface,
	robotName string,
) (*crSyncer, error) {
	var (
		annotations        = crd.ObjectMeta.Annotations
		filterByRobotValue = annotations[annotationFilterByRobotName]
		filterByRobot      = false
	)
	if filterByRobotValue != "" {
		if v, err := strconv.ParseBool(filterByRobotValue); err != nil {
			log.Printf("Value for %s  must be boolean on %s, got %q",
				annotationFilterByRobotName, crd.ObjectMeta.Name, filterByRobotValue)
		} else {
			filterByRobot = v
		}
	}
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  crd.Spec.Version,
		Resource: crd.Spec.Names.Plural,
	}
	ns := ""
	if crd.Spec.Scope == crdtypes.NamespaceScoped {
		// TODO(https://github.com/googlecloudrobotics/core/issues/19): allow syncing CRs in other namespaces
		ns = "default"
	}
	s := &crSyncer{
		crd:             crd,
		subtree:         annotations[annotationStatusSubtree],
		upstream:        remote.Resource(gvr).Namespace(ns),
		downstream:      local.Resource(gvr).Namespace(ns),
		upstreamQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.NewItemFastSlowRateLimiter(time.Millisecond*500, time.Second*5, 5), "upstream"),
		downstreamQueue: workqueue.NewNamedRateLimitingQueue(workqueue.NewItemFastSlowRateLimiter(time.Millisecond*500, time.Second*5, 5), "downstream"),
		done:            make(chan struct{}),
	}
	switch src := annotations[annotationSpecSource]; src {
	case "robot":
		s.clusterName = "cloud"
		// Swap upstream and downstream if the robot is the spec source.
		s.upstream, s.downstream = s.downstream, s.upstream
	case "cloud":
		s.clusterName = fmt.Sprintf("robot-%s", robotName)
	default:
		return nil, fmt.Errorf("unknown spec source %q", src)
	}
	if filterByRobot {
		if robotName != "" {
			s.labelSelector = labelRobotName + "=" + robotName
		} else {
			// TODO(fabxc): should this return an error instead?
			log.Printf("%s requested to filter by robot-name, but no robot-name was given to cr-syncer", crd.ObjectMeta.Name)
		}
	}

	newInformer := func(client dynamic.ResourceInterface) cache.SharedIndexInformer {
		return cache.NewSharedIndexInformer(
			&cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.LabelSelector = s.labelSelector
					return client.List(options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.LabelSelector = s.labelSelector
					return client.Watch(options)
				},
			},
			&unstructured.Unstructured{},
			resyncPeriod,
			nil,
		)
	}
	s.upstreamInf = newInformer(s.upstream)
	s.downstreamInf = newInformer(s.downstream)

	return s, nil
}

func (s *crSyncer) startInformers() error {
	go s.upstreamInf.Run(s.done)
	go s.downstreamInf.Run(s.done)

	if ok := cache.WaitForCacheSync(s.done, s.upstreamInf.HasSynced); !ok {
		return fmt.Errorf("stopped while syncing upstream informer for %s", s.crd.GetName())
	}
	if ok := cache.WaitForCacheSync(s.done, s.downstreamInf.HasSynced); !ok {
		return fmt.Errorf("stopped while syncing downstream informer for %s", s.crd.GetName())
	}
	s.setupInformerHandlers(s.upstreamInf, s.upstreamQueue, "upstream")
	s.setupInformerHandlers(s.downstreamInf, s.downstreamQueue, "downstream")

	return nil
}

func (s *crSyncer) setupInformerHandlers(
	inf cache.SharedIndexInformer,
	queue workqueue.RateLimitingInterface,
	direction string,
) {
	receive := func(obj interface{}, action string) {
		u := obj.(*unstructured.Unstructured)
		log.Printf("Got %s event from %s for %s %s@v%s",
			action, direction, u.GetKind(), u.GetName(), u.GetResourceVersion())
		if key, ok := keyFunc(obj); ok {
			queue.AddRateLimited(key)
		}
	}
	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			receive(obj, "add")
		},
		UpdateFunc: func(_, obj interface{}) {
			receive(obj, "update")
		},
		DeleteFunc: func(obj interface{}) {
			receive(obj, "delete")
		},
	})
}

func (s *crSyncer) processNextWorkItem(
	ctx context.Context,
	q workqueue.RateLimitingInterface,
	syncf func(string) error,
	qName string,
) bool {
	key, quit := q.Get()
	if quit {
		return false
	}
	defer q.Done(key)

	ctx, err := tag.New(ctx, tag.Insert(tagEventSource, qName))
	if err != nil {
		panic(err)
	}
	err = syncf(key.(string))
	stats.Record(ctx, mSyncs.M(1))
	if err == nil {
		q.Forget(key)
		return true
	}
	// Synchronization failed, retry later.
	stats.Record(ctx, mSyncErrors.M(1))
	log.Printf("Syncing key %q from queue %q failed: %v", key, qName, err)
	q.AddRateLimited(key)

	return true
}

func (s *crSyncer) run() {
	defer s.upstreamQueue.ShutDown()
	defer s.downstreamQueue.ShutDown()

	log.Printf("Starting syncer for %s", s.crd.GetName())

	// Start informers that will populate their associated workqueue.
	if err := s.startInformers(); err != nil {
		log.Printf("Starting informers for %s failed: %s", s.crd.GetName(), err)
		return
	}

	ctx, err := tag.New(context.Background(), tag.Insert(tagResource, s.crd.Name))
	if err != nil {
		panic(err)
	}
	// Process the upstream and downstream work queues.
	go func() {
		for s.processNextWorkItem(ctx, s.upstreamQueue, s.syncUpstream, "upstream") {
		}
	}()
	go func() {
		for s.processNextWorkItem(ctx, s.downstreamQueue, s.syncDownstream, "downstream") {
		}
	}()
	<-s.done
}

func (s *crSyncer) stop() {
	log.Printf("Stopping syncer for %s", s.crd.GetName())
	close(s.done)
}

// jsonPatch provides a JSON patch conform structure for add and replace. Please see: https://tools.ietf.org/html/rfc6902
type jsonPatchAddReplace struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

// syncDownstream reconciles state after receiving change events from the
// downstream cluster. It synchronizes the status from the downstream to the
// upstream cluster, and deletes orphaned downstream resources.
func (s *crSyncer) syncDownstream(key string) error {
	var (
		statusIsSubresource = s.crd.Spec.Subresources != nil && s.crd.Spec.Subresources.Status != nil
	)
	// Get the downstream status (src) and upstream spec (dst).
	srcObj, srcExists, err := s.downstreamInf.GetIndexer().GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to retrieve resource for key %s: %s", key, err)
	}
	if !srcExists {
		// The downstream resource has been deleted: possibly because
		// the upstream resource was deleted and recreated. Add this to
		// the upstream queue so that syncUpstream() can check if it needs
		// to recreate the downstream resource.
		s.upstreamQueue.Add(key)
		return nil
	}
	src := srcObj.(*unstructured.Unstructured).DeepCopy()
	removeFinalizer(s.downstream, src, s.clusterName)

	dstObj, dstExists, err := s.upstreamInf.GetIndexer().GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to retrieve resource for key %s: %s", key, err)
	}
	// If the upstream resource no longer exists, delete the downstream
	// resource. Normally, this occurs when syncUpstream() handles the
	// upstream deletion, but if the resource was deleted when the robot
	// was offline, upstream doesn't know about the old resource and we'll
	// hit this condition.
	if !dstExists {
		if src.GetDeletionTimestamp() != nil {
			return nil // Already being deleted.
		}
		if err := s.downstream.Delete(src.GetName(), nil); err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return fmt.Errorf("delete resource: %s", err)
		}
		return nil
	}

	// Copy full status or subtree from src status.
	var status interface{}
	if s.subtree == "" {
		status = src.Object["status"]
	} else if src.Object["status"] != nil {
		srcStatus, ok := src.Object["status"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("Expected status of %s in downstream cluster to be a dict", src.GetName())
		}
		if srcStatus[s.subtree] != nil {
			status = srcStatus[s.subtree]
		}
	}

	dst := dstObj.(*unstructured.Unstructured).DeepCopy()
	dst.Object["status"] = status
	setAnnotation(dst, annotationResourceVersion, src.GetResourceVersion())

	patchData := make([]jsonPatchAddReplace, 0, 2)
	patchData = append(patchData, jsonPatchAddReplace{
		Op:    "add",
		Path:  "/metadata/annotations",
		Value: dst.GetAnnotations(),
	})

	// We need to make a dedicated UpdateStatus call if the status is defined
	// as an explicit subresource of the CRD.
	if statusIsSubresource {
		// Status must not be null/nil.
		if status == nil {
			status = struct{}{}
		}
		if s.subtree == "" || status == struct{}{} {
			patchData = append(patchData, jsonPatchAddReplace{
				Op:    "add",
				Path:  "/status",
				Value: status,
			})
		} else {
			patchData = append(patchData, jsonPatchAddReplace{
				Op:    "add",
				Path:  fmt.Sprintf("/status/%s", s.subtree),
				Value: status,
			})
		}
		patchDataJSON, err := json.Marshal(patchData)
		if err != nil {
			return newAPIErrorf(dst, "Marshalling JSON failed: %s", err)
		}
		updated, err := s.upstream.Patch(dstObj.(*unstructured.Unstructured).GetName(), types.JSONPatchType, patchDataJSON, metav1.PatchOptions{}, "status")
		if err != nil {
			return newAPIErrorf(dst, "update status failed: %s", err)
		}
		dst = updated
	} else {
		if s.subtree == "" || status == nil {
			patchData = append(patchData, jsonPatchAddReplace{
				Op:    "add",
				Path:  "/status",
				Value: status,
			})
		} else {
			patchData = append(patchData, jsonPatchAddReplace{
				Op:    "add",
				Path:  fmt.Sprintf("/status/%s", s.subtree),
				Value: status,
			})
		}
		patchDataJSON, err := json.Marshal(patchData)
		if err != nil {
			return newAPIErrorf(dst, "Marshalling JSON failed: %s", err)
		}
		updated, err := s.upstream.Patch(dstObj.(*unstructured.Unstructured).GetName(), types.JSONPatchType, patchDataJSON, metav1.PatchOptions{})
		if err != nil {
			return newAPIErrorf(dst, "update failed: %s", err)
		}
		dst = updated
	}
	log.Printf("Copied %s %s status@v%s to upstream@v%s",
		src.GetKind(), src.GetName(), src.GetResourceVersion(), dst.GetResourceVersion())
	return nil
}

// syncUpstream reconciles the state after receiving a change event from upstream.
// It synchronizes the spec changes from upstream to the downstream cluster and propagates
// deletions.
func (s *crSyncer) syncUpstream(key string) error {
	// Get the upstream spec (src) and downstream status (dst).
	src := &unstructured.Unstructured{make(map[string]interface{})}
	dst := &unstructured.Unstructured{make(map[string]interface{})}
	srcObj, srcExists, err := s.upstreamInf.GetIndexer().GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to retrieve resource for key %s: %s", key, err)
	}
	if srcExists {
		src = srcObj.(*unstructured.Unstructured).DeepCopy()
		removeFinalizer(s.upstream, src, s.clusterName)
	}
	dstObj, dstExists, err := s.downstreamInf.GetIndexer().GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to retrieve resource for key %s: %s", key, err)
	}
	if dstExists {
		dst = dstObj.(*unstructured.Unstructured).DeepCopy()
	}

	// Check if the downstream resource (dst) should be created, updated,
	// or deleted. If we don't need to create/update dst, return early.
	var createOrUpdate func(*unstructured.Unstructured) (*unstructured.Unstructured, error)
	switch {
	case !srcExists && !dstExists:
		// Both deleted, nothing to do.
		return nil
	case srcExists && !dstExists:
		// Create object and set base fields.
		createOrUpdate = func(o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			o.SetGroupVersionKind(src.GroupVersionKind())
			o.SetNamespace(src.GetNamespace())
			o.SetName(src.GetName())
			// Copy upstream status on initial creation.
			o.Object["status"] = src.Object["status"]

			return s.downstream.Create(o, metav1.CreateOptions{})
		}
	case srcExists && dstExists:
		// Update dst.
		createOrUpdate = func(o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			patchData := make([]jsonPatchAddReplace, 0, 1)
			patchData = append(patchData, jsonPatchAddReplace{
				Op:    "add",
				Path:  "/spec",
				Value: o.Object["spec"],
			})
			patchDataJSON, err := json.Marshal(patchData)
			if err != nil {
				return nil, newAPIErrorf(dst, "Marshalling JSON failed: %s", err)
			}
			return s.downstream.Patch(o.GetName(), types.JSONPatchType, patchDataJSON, metav1.PatchOptions{})
		}
	case !srcExists && dstExists:
		// Delete dst.
		if err := s.downstream.Delete(dst.GetName(), nil); err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return newAPIErrorf(dst, "downstream delete failed: %s", err)
		}
		return nil
	default:
		log.Fatalf("unhandled condition: srcExists=%t, dstExists=%t", srcExists, dstExists)
		return nil
	}

	// Before creating/updating, check if deletion is in progress. This
	// is checked separately to src/dstExists for readability (hopefully).
	if src.GetDeletionTimestamp() != nil {
		if err := s.downstream.Delete(src.GetName(), nil); err != nil {
			if isNotFoundError(err) {
				return nil
			}
			return newAPIErrorf(dst, "downstream delete failed: %s", err)
		}
		return nil
	}

	// Create/update dst with the labels+annotations+spec of src.
	dst.SetLabels(src.GetLabels())
	dst.SetAnnotations(src.GetAnnotations())
	dst.Object["spec"] = src.Object["spec"]

	// The remote-resource-version annotation is removed from dst to
	// prevent an infinite loop, because changing the annotation would
	// change the resource version.
	deleteAnnotation(dst, annotationResourceVersion)

	if _, err = createOrUpdate(dst); err != nil {
		return newAPIErrorf(dst, "failed to create or update downstream: %s", err)
	}
	return nil
}

func isNotFoundError(err error) bool {
	status, ok := err.(*errors.StatusError)
	return ok && status.ErrStatus.Code == http.StatusNotFound
}

type apiError struct {
	o   *unstructured.Unstructured
	msg string
}

func (e apiError) Error() string {
	return fmt.Sprintf("%s %s/%s @ %s: %s", e.o.GetKind(), e.o.GetNamespace(), e.o.GetName(), e.o.GetResourceVersion(), e.msg)
}

func newAPIErrorf(o *unstructured.Unstructured, format string, args ...interface{}) apiError {
	return apiError{o: o, msg: fmt.Sprintf(format, args...)}
}

// keyFunc extracts a key of the form [<namespace>/]<name> from a resource
// which is used to access the informer's store and index.
func keyFunc(obj interface{}) (string, bool) {
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Printf("deriving key failed: %s", err)
		return k, false
	}
	return k, true
}

func setAnnotation(o *unstructured.Unstructured, key, value string) {
	annotations := o.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[key] = value
	o.SetAnnotations(annotations)
}

func deleteAnnotation(o *unstructured.Unstructured, key string) {
	annotations := o.GetAnnotations()
	if annotations != nil {
		delete(annotations, key)
	}
	if len(annotations) > 0 {
		o.SetAnnotations(annotations)
	} else {
		o.SetAnnotations(nil)
	}

}
