/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rotation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/secrets-store-csi-driver/apis/v1alpha1"
	secretsStoreClient "sigs.k8s.io/secrets-store-csi-driver/pkg/client/clientset/versioned"
	internalerrors "sigs.k8s.io/secrets-store-csi-driver/pkg/errors"
	"sigs.k8s.io/secrets-store-csi-driver/pkg/k8s"
	secretsstore "sigs.k8s.io/secrets-store-csi-driver/pkg/secrets-store"
	"sigs.k8s.io/secrets-store-csi-driver/pkg/util/fileutil"
	"sigs.k8s.io/secrets-store-csi-driver/pkg/util/k8sutil"
	"sigs.k8s.io/secrets-store-csi-driver/pkg/util/secretutil"
)

const (
	permission       os.FileMode = 0644
	maxNumOfRequeues int         = 5

	mountRotationFailedReason       = "MountRotationFailed"
	mountRotationCompleteReason     = "MountRotationComplete"
	k8sSecretRotationFailedReason   = "SecretRotationFailed"
	k8sSecretRotationCompleteReason = "SecretRotationComplete"

	csipodname      = "csi.storage.k8s.io/pod.name"
	csipodnamespace = "csi.storage.k8s.io/pod.namespace"
	csipoduid       = "csi.storage.k8s.io/pod.uid"
	csipodsa        = "csi.storage.k8s.io/serviceAccount.name"
)

// Reconciler reconciles and rotates contents in the pod
// and Kubernetes secrets periodically
type Reconciler struct {
	store                k8s.Store
	ctrlReaderClient     client.Reader
	ctrlWriterClient     client.Writer
	providerVolumePath   string
	scheme               *runtime.Scheme
	rotationPollInterval time.Duration
	providerClients      map[string]*secretsstore.CSIProviderClient
	queue                workqueue.RateLimitingInterface
	reporter             StatsReporter
	eventRecorder        record.EventRecorder
}

// NewReconciler returns a new reconciler for rotation
func NewReconciler(s *runtime.Scheme, providerVolumePath, nodeName string, rotationPollInterval time.Duration) (*Reconciler, error) {
	config, err := buildConfig()
	if err != nil {
		return nil, err
	}
	kubeClient := kubernetes.NewForConfigOrDie(config)
	crdClient := secretsStoreClient.NewForConfigOrDie(config)
	c, err := client.New(config, client.Options{Scheme: s, Mapper: nil})
	if err != nil {
		return nil, err
	}
	store, err := k8s.New(kubeClient, crdClient, nodeName, 5*time.Second)
	if err != nil {
		return nil, err
	}
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&clientcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(s, v1.EventSource{Component: "csi-secrets-store-rotation"})

	return &Reconciler{
		store:                store,
		ctrlReaderClient:     c,
		ctrlWriterClient:     c,
		scheme:               s,
		providerVolumePath:   providerVolumePath,
		rotationPollInterval: rotationPollInterval,
		providerClients:      make(map[string]*secretsstore.CSIProviderClient),
		reporter:             newStatsReporter(),
		queue:                workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		eventRecorder:        recorder,
	}, nil
}

// Run starts the rotation reconciler
func (r *Reconciler) Run(stopCh <-chan struct{}) {
	defer r.queue.ShutDown()
	klog.Infof("starting rotation reconciler with poll interval: %s", r.rotationPollInterval)

	ticker := time.NewTicker(r.rotationPollInterval)
	defer ticker.Stop()

	if err := r.store.Run(stopCh); err != nil {
		klog.Fatalf("failed to run informers for rotation reconciler, err: %+v", err)
	}

	// TODO (aramase) consider adding more workers to process reconcile concurrently
	for i := 0; i < 1; i++ {
		go wait.Until(r.runWorker, time.Second, stopCh)
	}

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			// The spc pod status informer is configured to do a filtered list watch of spc pod statuses
			// labeled for the same node as the driver. LIST will only return the filtered results.
			spcpsList, err := r.store.ListSecretProviderClassPodStatus()
			if err != nil {
				klog.ErrorS(err, "failed to list secret provider class pod status for node", "controller", "rotation")
				continue
			}
			for _, spcps := range spcpsList {
				key, err := cache.MetaNamespaceKeyFunc(spcps)
				if err == nil {
					r.queue.Add(key)
				}
			}
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context, spcps *v1alpha1.SecretProviderClassPodStatus) (err error) {
	begin := time.Now()
	errorReason := internalerrors.FailedToRotate
	// requiresUpdate is set to true when the new object versions differ from the current object versions
	// after the provider mount request is complete
	var requiresUpdate bool
	var providerName string

	defer func() {
		if err != nil {
			r.reporter.reportRotationErrorCtMetric(providerName, errorReason, requiresUpdate)
			return
		}
		r.reporter.reportRotationCtMetric(providerName, requiresUpdate)
		r.reporter.reportRotationDuration(time.Since(begin).Seconds())
	}()

	// get pod from informer cache
	podName, podNamespace := spcps.Status.PodName, spcps.Namespace
	pod, err := r.store.GetPod(podName, podNamespace)
	if err != nil {
		errorReason = internalerrors.PodNotFound
		return fmt.Errorf("failed to get pod %s/%s, err: %+v", podNamespace, podName, err)
	}
	// skip rotation if the pod is being terminated
	// or the pod is in succeeded state (for jobs that complete aren't gc yet)
	// or the pod is in a failed state (all containers get terminated).
	// the spcps will be gc when the pod is deleted and will not show up in the next rotation cycle
	if !pod.GetDeletionTimestamp().IsZero() || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		klog.V(5).InfoS("pod is being terminated, skipping rotation", "pod", klog.KObj(pod))
		return nil
	}

	spcName, spcNamespace := spcps.Status.SecretProviderClassName, spcps.Namespace

	// get the secret provider class the pod status is referencing from informer cache
	spc, err := r.store.GetSecretProviderClass(spcName, spcNamespace)
	if err != nil {
		errorReason = internalerrors.SecretProviderClassNotFound
		return fmt.Errorf("failed to get secret provider class %s/%s, err: %+v", spcNamespace, spcName, err)
	}

	// determine which pod volume this is associated with
	podVol := k8sutil.SPCVolume(pod, spc.Name)
	if podVol == nil {
		errorReason = internalerrors.PodVolumeNotFound
		return fmt.Errorf("could not find secret provider class pod status volume for pod %s/%s", podNamespace, podName)
	}

	// validate TargetPath
	if fileutil.GetPodUIDFromTargetPath(spcps.Status.TargetPath) != string(pod.UID) {
		errorReason = internalerrors.UnexpectedTargetPath
		return fmt.Errorf("secret provider class pod status targetPath did not match pod UID for pod %s/%s", podNamespace, podName)
	}
	if fileutil.GetVolumeNameFromTargetPath(spcps.Status.TargetPath) != podVol.Name {
		errorReason = internalerrors.UnexpectedTargetPath
		return fmt.Errorf("secret provider class pod status volume name did not match pod Volume for pod %s/%s", podNamespace, podName)
	}

	parameters := make(map[string]string)
	if spc.Spec.Parameters != nil {
		parameters = spc.Spec.Parameters
	}
	// Set these parameters to mimic the exact same attributes we get as part of NodePublishVolumeRequest
	parameters[csipodname] = podName
	parameters[csipodnamespace] = podNamespace
	parameters[csipoduid] = string(pod.UID)
	parameters[csipodsa] = pod.Spec.ServiceAccountName

	paramsJSON, err := json.Marshal(parameters)
	if err != nil {
		return fmt.Errorf("failed to marshal parameters, err: %+v", err)
	}
	permissionJSON, err := json.Marshal(permission)
	if err != nil {
		return fmt.Errorf("failed to marshal permission, err: %+v", err)
	}

	// check if the volume pertaining to the current spc is using nodePublishSecretRef for
	// accessing external secrets store
	nodePublishSecretRef := podVol.CSI.NodePublishSecretRef

	var secretsJSON []byte
	nodePublishSecretData := make(map[string]string)
	// read the Kubernetes secret referenced in NodePublishSecretRef and marshal it
	// This comprises the secret parameter in the MountRequest to the provider
	if nodePublishSecretRef != nil {
		secretName := strings.TrimSpace(nodePublishSecretRef.Name)
		secretNamespace := spcps.Namespace

		// read secret from the informer cache
		secret, err := r.store.GetSecret(secretName, secretNamespace)
		if err != nil {
			errorReason = internalerrors.NodePublishSecretRefNotFound
			r.generateEvent(pod, v1.EventTypeWarning, mountRotationFailedReason, fmt.Sprintf("failed to get node publish secret %s/%s, err: %+v", secretNamespace, secretName, err))
			return fmt.Errorf("failed to get node publish secret %s/%s, err: %+v", secretNamespace, secretName, err)
		}

		for k, v := range secret.Data {
			nodePublishSecretData[k] = string(v)
		}
	}

	secretsJSON, err = json.Marshal(nodePublishSecretData)
	if err != nil {
		r.generateEvent(pod, v1.EventTypeWarning, mountRotationFailedReason, fmt.Sprintf("failed to marshal node publish secret data, err: %+v", err))
		return fmt.Errorf("failed to marshal node publish secret data, err: %+v", err)
	}

	// generate a map with the current object versions stored in spc pod status
	// the old object versions are passed on to the provider as part of the MountRequest.
	// the provider can use these current object versions to decide if any action is required
	// and if the objects need to be rotated
	oldObjectVersions := make(map[string]string)
	for _, obj := range spcps.Status.Objects {
		oldObjectVersions[obj.ID] = obj.Version
	}

	providerName = string(spc.Spec.Provider)
	providerClient, err := r.getProviderClient(providerName)
	if err != nil {
		errorReason = internalerrors.FailedToCreateProviderGRPCClient
		r.generateEvent(pod, v1.EventTypeWarning, mountRotationFailedReason, fmt.Sprintf("failed to create provider client, err: %+v", err))
		return fmt.Errorf("failed to create provider client, err: %+v", err)
	}
	newObjectVersions, errorReason, err := providerClient.MountContent(ctx, string(paramsJSON), string(secretsJSON), spcps.Status.TargetPath, string(permissionJSON), oldObjectVersions)
	if err != nil {
		r.generateEvent(pod, v1.EventTypeWarning, mountRotationFailedReason, fmt.Sprintf("provider mount err: %+v", err))
		return fmt.Errorf("failed to rotate objects for pod %s/%s, err: %+v", spcps.Namespace, spcps.Status.PodName, err)
	}

	// compare the old object versions and new object versions to check if any of the objects
	// have been updated by the provider
	for k, v := range newObjectVersions {
		version, ok := oldObjectVersions[strings.TrimSpace(k)]
		if ok && strings.TrimSpace(version) == strings.TrimSpace(v) {
			continue
		}
		requiresUpdate = true
		break
	}
	// if the spc was updated after initial deployment to remove an existing object, then we
	// need to update the objects list with the current list to reflect only what's in the pod
	if len(oldObjectVersions) != len(newObjectVersions) {
		requiresUpdate = true
	}

	var errs []error
	// this loop is executed if there is a difference in the current versions cached in
	// the secret provider class pod status and the new versions returned by the provider.
	// the diff in versions is populated in the secret provider class pod status and if the
	// secret provider class contains secret objects, then the corresponding kubernetes secrets
	// data is updated with the latest versions
	if requiresUpdate {
		// generate an event for successful mount update
		r.generateEvent(pod, v1.EventTypeNormal, mountRotationCompleteReason, fmt.Sprintf("successfully rotated mounted contents for spc %s/%s", spcNamespace, spcName))
		klog.InfoS("updating versions in spc pod status", "spcps", klog.KObj(spcps), "controller", "rotation")

		var ov []v1alpha1.SecretProviderClassObject
		for k, v := range newObjectVersions {
			ov = append(ov, v1alpha1.SecretProviderClassObject{ID: strings.TrimSpace(k), Version: strings.TrimSpace(v)})
		}
		spcps.Status.Objects = ov

		updateFn := func() (bool, error) {
			err = r.updateSecretProviderClassPodStatus(ctx, spcps)
			if err != nil {
				klog.ErrorS(err, "failed to update latest versions in spc pod status", "spcps", klog.KObj(spcps), "controller", "rotation")
				return false, nil
			}
			return true, nil
		}

		if err := wait.ExponentialBackoff(wait.Backoff{
			Steps:    5,
			Duration: 1 * time.Millisecond,
			Factor:   1.0,
			Jitter:   0.1,
		}, updateFn); err != nil {
			r.generateEvent(pod, v1.EventTypeWarning, mountRotationFailedReason, fmt.Sprintf("failed to update versions in spc pod status %s, err: %+v", spcName, err))
			return fmt.Errorf("failed to update spc pod status, err: %+v", err)
		}
	}

	if len(spc.Spec.SecretObjects) == 0 {
		klog.InfoS("spc doesn't contain secret objects", "spc", klog.KObj(spc), "pod", klog.KObj(pod), "controller", "rotation")
		return nil
	}
	files, err := fileutil.GetMountedFiles(spcps.Status.TargetPath)
	if err != nil {
		r.generateEvent(pod, v1.EventTypeWarning, k8sSecretRotationFailedReason, fmt.Sprintf("failed to get mounted files, err: %+v", err))
		return fmt.Errorf("failed to get mounted files, err: %+v", err)
	}
	for _, secretObj := range spc.Spec.SecretObjects {
		secretName := strings.TrimSpace(secretObj.SecretName)

		if err = secretutil.ValidateSecretObject(*secretObj); err != nil {
			r.generateEvent(pod, v1.EventTypeWarning, k8sSecretRotationFailedReason, fmt.Sprintf("failed validation for secret object in spc %s/%s, err: %+v", spcNamespace, spcName, err))
			klog.ErrorS(err, "failed validation for secret object in spc", "spc", klog.KObj(spc), "controller", "rotation")
			errs = append(errs, err)
			continue
		}

		secretType := secretutil.GetSecretType(strings.TrimSpace(secretObj.Type))
		var datamap map[string][]byte
		if datamap, err = secretutil.GetSecretData(secretObj.Data, secretType, files); err != nil {
			r.generateEvent(pod, v1.EventTypeWarning, k8sSecretRotationFailedReason, fmt.Sprintf("failed to get data in spc %s/%s for secret %s, err: %+v", spcNamespace, spcName, secretName, err))
			klog.ErrorS(err, "failed to get data in spc for secret", "spc", klog.KObj(spc), "secret", klog.ObjectRef{Namespace: spcNamespace, Name: secretName}, "controller", "rotation")
			errs = append(errs, err)
			continue
		}

		patchFn := func() (bool, error) {
			// patch secret data with the new contents
			if err := r.patchSecret(ctx, secretObj.SecretName, spcps.Namespace, datamap); err != nil {
				klog.ErrorS(err, "failed to patch secret data", "secret", klog.ObjectRef{Namespace: spcNamespace, Name: secretName}, "spc", klog.KObj(spc), "controller", "rotation")
				return false, nil
			}
			return true, nil
		}

		if err := wait.ExponentialBackoff(wait.Backoff{
			Steps:    5,
			Duration: 1 * time.Millisecond,
			Factor:   1.0,
			Jitter:   0.1,
		}, patchFn); err != nil {
			r.generateEvent(pod, v1.EventTypeWarning, k8sSecretRotationFailedReason, fmt.Sprintf("failed to patch secret %s with new data, err: %+v", secretName, err))
			// continue to ensure error in a single secret doesn't block the updates
			// for all other secret objects defined in SPC
			continue
		}
		r.generateEvent(pod, v1.EventTypeNormal, k8sSecretRotationCompleteReason, fmt.Sprintf("successfully rotated K8s secret %s", secretName))
	}

	// for errors with individual secret objects in spc, we continue to the next secret object
	// to prevent error with one secret from affecting rotation of all other k8s secret
	// this consolidation of errors within the loop determines if the spc pod status still needs
	// to be retried at the end of this rotation reconcile loop
	if len(errs) > 0 {
		return fmt.Errorf("failed to rotate one or more k8s secrets, err: %+v", errs)
	}

	return nil
}

// updateSecretProviderClassPodStatus updates secret provider class pod status
func (r *Reconciler) updateSecretProviderClassPodStatus(ctx context.Context, spcPodStatus *v1alpha1.SecretProviderClassPodStatus) error {
	// update the secret provider class pod status
	return r.ctrlWriterClient.Update(ctx, spcPodStatus, &client.UpdateOptions{})
}

// patchSecret patches secret with the new data and returns error if any
func (r *Reconciler) patchSecret(ctx context.Context, name, namespace string, data map[string][]byte) error {
	secret := &v1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	err := r.ctrlReaderClient.Get(ctx, secretKey, secret)
	// if there is an error getting the secret -
	// 1. The secret has been deleted due to an external client
	// 		The secretproviderclasspodstatus controller will recreate the
	//		secret as part of the reconcile operation. We don't want to duplicate
	//		the operation in multiple controllers.
	// 2. An actual error communicating with the API server, then just return
	if err != nil {
		return err
	}

	currentDataSHA, err := secretutil.GetSHAFromSecret(secret.Data)
	if err != nil {
		return fmt.Errorf("failed to compute SHA for %s/%s old data, err: %+v", namespace, name, err)
	}
	newDataSHA, err := secretutil.GetSHAFromSecret(data)
	if err != nil {
		return fmt.Errorf("failed to compute SHA for %s/%s new data, err: %+v", namespace, name, err)
	}
	// if the SHA for the current data and new data match then skip
	// the redundant API call to patch the same data
	if currentDataSHA == newDataSHA {
		return nil
	}

	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	// Patching data replaces values for existing data keys
	// and appends new keys if it doesn't already exist
	secret.Data = data
	return r.ctrlWriterClient.Patch(ctx, secret, patch)
}

// getProviderClient returns the GRPC provider client to use for mount request
func (r *Reconciler) getProviderClient(providerName string) (*secretsstore.CSIProviderClient, error) {
	// check if the provider client already exists
	if providerClient, exists := r.providerClients[providerName]; exists {
		return providerClient, nil
	}
	// create a new client as it doesn't exist in the reconciler cache
	providerClient, err := secretsstore.NewProviderClient(secretsstore.CSIProviderName(providerName), r.providerVolumePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s provider client, err: %+v", providerName, err)
	}
	r.providerClients[providerName] = providerClient
	return providerClient, nil
}

// runWorker runs a thread that process the queue
func (r *Reconciler) runWorker() {
	for r.processNextItem() {

	}
}

// processNextItem picks the next available item in the queue and triggers reconcile
func (r *Reconciler) processNextItem() bool {
	key, quit := r.queue.Get()
	if quit {
		return false
	}
	defer r.queue.Done(key)
	spcps, err := r.store.GetSecretProviderClassPodStatus(key.(string))
	if err != nil {
		// set the log level to 5 so we don't spam the logs with spc pod status not found
		klog.V(5).ErrorS(err, "failed to get spc pod status", "spcps", key.(string), "controller", "rotation")
		rateLimited := false
		// If the error is that spc pod status not found in cache, only retry
		// with a limit instead of infinite retries.
		// The cache miss could be because of
		//   1. The pod was deleted and the spc pod status no longer exists
		//		We limit the requeue to only 5 times. After 5 times if the spc pod status
		//		is no longer found, then it will be retried in the next reconcile Run if it's
		//		an intermittent cache population delay.
		//   2. The spc pod status has not yet been populated in the cache
		// 		this is highly unlikely as the spc pod status was added to the queue
		// 		in Run method after the List call from the same informer cache.
		if apierrors.IsNotFound(err) {
			rateLimited = true
		}
		r.handleError(err, key, rateLimited)
		return true
	}
	klog.V(3).InfoS("reconciler started", "spcps", klog.KObj(spcps), "controller", "rotation")
	if err = r.reconcile(context.Background(), spcps); err != nil {
		klog.ErrorS(err, "failed to reconcile spc for pod", "spc",
			spcps.Status.SecretProviderClassName, "pod", spcps.Status.PodName, "controller", "rotation")
	}

	klog.V(3).InfoS("reconciler completed", "spcps", klog.KObj(spcps), "controller", "rotation")
	r.handleError(err, key, false)
	return true
}

// handleError requeue the key after 10s if there is an error while processing
func (r *Reconciler) handleError(err error, key interface{}, rateLimited bool) {
	if err == nil {
		r.queue.Forget(key)
		return
	}
	if !rateLimited {
		r.queue.AddAfter(key, 10*time.Second)
		return
	}
	// if the requeue for key is rate limited and the number of times the key
	// has been added back to queue exceeds the default allowed limit, then do nothing.
	// this is done to prevent infinitely adding the key the queue in scenarios where
	// the key was added to the queue because of an error but has since been deleted.
	if r.queue.NumRequeues(key) < maxNumOfRequeues {
		r.queue.AddRateLimited(key)
		return
	}
	klog.InfoS("retry budget exceeded, dropping from queue", "spcps", key)
	r.queue.Forget(key)
}

// generateEvent generates an event
func (r *Reconciler) generateEvent(obj runtime.Object, eventType, reason, message string) {
	r.eventRecorder.Eventf(obj, eventType, reason, message)
}

// Create the client config. Use kubeconfig if given, otherwise assume in-cluster.
func buildConfig() (*rest.Config, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	return rest.InClusterConfig()
}
