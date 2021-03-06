package controller

import (
	"fmt"

	"github.com/golang/glog"

	apierrs "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
	kapi "k8s.io/kubernetes/pkg/api"

	"github.com/openshift/origin/pkg/client"
	"github.com/openshift/origin/pkg/controller"
	"github.com/openshift/origin/pkg/image/api"
)

type uniqueItem struct {
	uid             string
	resourceVersion string
}

type ScheduledImageStreamController struct {
	// boolean flag whether this controller is active
	enabled bool

	// image stream client
	isNamespacer client.ImageStreamsNamespacer

	// lister can list/get image streams from a shared informer's cache
	lister imageStreamLister
	// listerSynced makes sure the is store is synced before reconciling streams
	listerSynced cache.InformerSynced

	// rateLimiter to be used when re-importing images
	rateLimiter flowcontrol.RateLimiter

	// scheduler for timely image re-imports
	scheduler *controller.Scheduler
}

// Importing is invoked when the controller decides to import a stream in order to push back
// the next schedule time.
func (s *ScheduledImageStreamController) Importing(stream *api.ImageStream) {
	if !s.enabled {
		return
	}
	glog.V(5).Infof("DEBUG: stream %s was just imported", stream.Name)
	// Push the current key back to the end of the queue because it's just been imported
	key, _ := cache.MetaNamespaceKeyFunc(stream)
	s.scheduler.Delay(key)
}

// Run begins watching and syncing.
func (s *ScheduledImageStreamController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	glog.Infof("Starting scheduled import controller")

	// Wait for the stream store to sync before starting any work in this controller.
	if !cache.WaitForCacheSync(stopCh, s.listerSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	go s.scheduler.RunUntil(stopCh)

	<-stopCh
	glog.Infof("Shutting down image stream controller")
}

func (s *ScheduledImageStreamController) addImageStream(obj interface{}) {
	stream := obj.(*api.ImageStream)
	s.enqueueImageStream(stream)
}

func (s *ScheduledImageStreamController) updateImageStream(old, cur interface{}) {
	curStream, ok := cur.(*api.ImageStream)
	if !ok {
		return
	}
	oldStream, ok := old.(*api.ImageStream)
	if !ok {
		return
	}
	// we only compare resource version, since deeper inspection if a stream
	// needs to be re-imported happens in syncImageStream
	if curStream.ResourceVersion == oldStream.ResourceVersion {
		return
	}
	s.enqueueImageStream(curStream)
}

func (s *ScheduledImageStreamController) deleteImageStream(obj interface{}) {
	stream, isStream := obj.(*api.ImageStream)
	if !isStream {
		tombstone, objIsTombstone := obj.(cache.DeletedFinalStateUnknown)
		if !objIsTombstone {
			return
		}
		stream, isStream = tombstone.Obj.(*api.ImageStream)
	}
	key, err := cache.MetaNamespaceKeyFunc(stream)
	if err != nil {
		glog.V(2).Infof("unable to get namespace key function for stream %q: %v", stream, err)
		return
	}
	s.scheduler.Remove(key, nil)
}

// enqueueImageStream ensures an image stream is checked for scheduling
func (s *ScheduledImageStreamController) enqueueImageStream(stream *api.ImageStream) {
	if !s.enabled {
		return
	}
	if needsScheduling(stream) {
		key, err := cache.MetaNamespaceKeyFunc(stream)
		if err != nil {
			glog.V(2).Infof("unable to get namespace key function for stream %q: %v", stream, err)
			return
		}
		s.scheduler.Add(key, uniqueItem{uid: string(stream.UID), resourceVersion: stream.ResourceVersion})
	}
}

// syncTimed is invoked when a key is ready to be processed.
func (s *ScheduledImageStreamController) syncTimed(key, value interface{}) {
	if !s.enabled {
		s.scheduler.Remove(key, value)
		return
	}
	if s.rateLimiter != nil && !s.rateLimiter.TryAccept() {
		glog.V(5).Infof("DEBUG: check of %s exceeded rate limit, will retry later", key)
		return
	}
	namespace, name, err := cache.SplitMetaNamespaceKey(key.(string))
	if err != nil {
		glog.V(2).Infof("unable to split namespace key for key %q: %v", key, err)
		return
	}
	if err := s.syncTimedByName(namespace, name); err != nil {
		// the stream cannot be imported
		if err == ErrNotImportable {
			// value must match to be removed, so we avoid races against creation by ensuring that we only
			// remove the stream if the uid and resource version in the scheduler are exactly the same.
			s.scheduler.Remove(key, value)
			return
		}
		utilruntime.HandleError(err)
		return
	}
}

func (s *ScheduledImageStreamController) syncTimedByName(namespace, name string) error {
	sharedStream, err := s.lister.ImageStreams(namespace).Get(name)
	if err != nil {
		if apierrs.IsNotFound(err) {
			return ErrNotImportable
		}
		return err
	}
	if !needsScheduling(sharedStream) {
		return ErrNotImportable
	}

	copy, err := kapi.Scheme.DeepCopy(sharedStream)
	if err != nil {
		return err
	}
	stream := copy.(*api.ImageStream)
	resetScheduledTags(stream)

	glog.V(3).Infof("Scheduled import of stream %s/%s...", stream.Namespace, stream.Name)
	return handleImageStream(stream, s.isNamespacer, nil)
}

// resetScheduledTags artificially increments the generation on the tags that should be imported.
func resetScheduledTags(stream *api.ImageStream) {
	next := stream.Generation + 1
	for tag, tagRef := range stream.Spec.Tags {
		if tagImportable(tagRef) && tagRef.ImportPolicy.Scheduled {
			tagRef.Generation = &next
			stream.Spec.Tags[tag] = tagRef
		}
	}
}

// needsScheduling returns true if this image stream has any scheduled tags
func needsScheduling(stream *api.ImageStream) bool {
	for _, tagRef := range stream.Spec.Tags {
		if tagImportable(tagRef) && tagRef.ImportPolicy.Scheduled {
			return true
		}
	}
	return false
}
