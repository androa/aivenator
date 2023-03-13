package aiven_application

import (
	"context"
	"errors"
	"fmt"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	"time"

	nais_io_v1 "github.com/nais/liberator/pkg/apis/nais.io/v1"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/nais/aivenator/constants"

	aiven_nais_io_v1 "github.com/nais/liberator/pkg/apis/aiven.nais.io/v1"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/nais/aivenator/pkg/credentials"
	"github.com/nais/aivenator/pkg/metrics"
	"github.com/nais/aivenator/pkg/utils"
)

const (
	requeueInterval    = time.Second * 10
	secretWriteTimeout = time.Second * 2
	rolloutComplete    = "RolloutComplete"
	rolloutFailed      = "RolloutFailed"
	AivenVolumeName    = "aiven-credentials"
)

var relevantKinds map[string]bool

func init() {
	// This list should probably be kept in sync with the one in pkg/credentials/janitor.go
	relevantKinds = map[string]bool{
		"ReplicaSet": true,
		"CronJob":    true,
		"Job":        true,
	}
}

func NewReconciler(mgr manager.Manager, logger *log.Logger, credentialsManager credentials.Manager, credentialsJanitor credentials.Janitor) AivenApplicationReconciler {
	return AivenApplicationReconciler{
		Client:  mgr.GetClient(),
		Logger:  logger.WithFields(log.Fields{"component": "AivenApplicationReconciler"}),
		Manager: credentialsManager,
		Janitor: credentialsJanitor,
	}
}

type AivenApplicationReconciler struct {
	client.Client
	Logger  *log.Entry
	Manager credentials.Manager
	Janitor credentials.Janitor
}

func (r *AivenApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var application aiven_nais_io_v1.AivenApplication

	logger := r.Logger.WithFields(log.Fields{
		"aiven_application": req.Name,
		"namespace":         req.Namespace,
	})

	logger.Infof("Processing request")
	defer func() {
		logger.Infof("Finished processing request")
		syncState := application.Status.SynchronizationState
		if syncState == "" {
			syncState = "unknown"
		}
		metrics.ApplicationsProcessed.With(prometheus.Labels{
			metrics.LabelSyncState: syncState,
		}).Inc()
	}()

	fail := func(err error) (ctrl.Result, error) {
		if err != nil {
			logger.Error(err)
		}
		application.Status.SynchronizationState = rolloutFailed
		cr := ctrl.Result{}

		if !errors.Is(err, utils.UnrecoverableError) {
			cr.RequeueAfter = requeueInterval
		}

		return cr, nil
	}

	err := r.Get(ctx, req.NamespacedName, &application)
	switch {
	case k8serrors.IsNotFound(err):
		return fail(fmt.Errorf("resource deleted from cluster; noop: %w", utils.UnrecoverableError))
	case err != nil:
		return fail(fmt.Errorf("unable to retrieve resource from cluster: %s", err))
	}

	logger = logger.WithFields(log.Fields{
		"secret_name": application.Spec.SecretName,
	})

	applicationDeleted, err := r.HandleProtectedAndTimeLimited(ctx, application, logger)
	if err != nil {
		utils.LocalFail("HandleProtectedAndTimeLimited", &application, err, logger)
		return fail(err)
	} else if applicationDeleted {
		return ctrl.Result{}, nil
	}

	logger.Infof("Application exists; processing")
	defer func() {
		application.Status.SynchronizationTime = &v1.Time{time.Now()}
		application.Status.ObservedGeneration = application.GetGeneration()
		err := metrics.ObserveKubernetesLatency("AivenApplication_Update", func() error {
			return r.Status().Update(ctx, &application)
		})
		if err != nil {
			logger.Errorf("Unable to update status of application: %s\nWanted to save status: %+v", err, application.Status)
		} else {
			metrics.KubernetesResourcesWritten.With(prometheus.Labels{
				metrics.LabelResourceType: "AivenApplication",
				metrics.LabelNamespace:    application.GetNamespace(),
			}).Inc()
		}
	}()

	errs := r.Janitor.CleanUnusedSecrets(ctx, application)
	if len(errs) > 0 {
		for _, err := range errs {
			logger.Error(err)
		}
	}

	hash, err := application.Hash()
	if err != nil {
		utils.LocalFail("Hash", &application, err, logger)
		return fail(err)
	}

	objs := r.FindDependentObjects(ctx, application, logger)
	needsSync, err := r.NeedsSynchronization(ctx, application, hash, objs, logger)
	if err != nil {
		utils.LocalFail("NeedsSynchronization", &application, err, logger)
		return fail(err)
	}

	if !needsSync {
		return ctrl.Result{}, nil
	}

	processingStart := time.Now()
	defer func() {
		used := time.Now().Sub(processingStart)
		syncState := application.Status.SynchronizationState
		if syncState == "" {
			syncState = "unknown"
		}
		metrics.ApplicationProcessingTime.With(prometheus.Labels{
			metrics.LabelSyncState: syncState,
		}).Observe(used.Seconds())
	}()

	logger.Infof("Creating secret")
	secret := r.initSecret(ctx, application, logger)
	secret, err = r.Manager.CreateSecret(&application, objs, secret, logger)
	if err != nil {
		utils.LocalFail("CreateSecret", &application, err, logger)
		return fail(err)
	}

	logger.Infof("Saving secret to cluster")
	err = r.SaveSecret(ctx, secret, logger)
	if err != nil {
		utils.LocalFail("SaveSecret", &application, err, logger)
		return fail(err)
	}

	success(&application, hash)

	if r.missingRelevantOwnerReference(ctx, *secret, logger) {
		interval := utils.NextRequeueInterval(secret, requeueInterval)
		logger.Infof("Missing replicaset owner reference; requeueing in %d seconds", int(interval.Seconds()))
		metrics.ApplicationsRequeued.With(prometheus.Labels{
			metrics.LabelSyncState: application.Status.SynchronizationState,
		}).Inc()
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	return ctrl.Result{}, nil
}

func (r *AivenApplicationReconciler) initSecret(ctx context.Context, application aiven_nais_io_v1.AivenApplication, logger log.FieldLogger) *corev1.Secret {
	secret := corev1.Secret{}
	err := metrics.ObserveKubernetesLatency("Secret_Get", func() error {
		return r.Get(ctx, application.SecretKey(), &secret)
	})
	switch {
	case k8serrors.IsNotFound(err):
		return &secret
	case err != nil:
		logger.Warnf("error retrieving existing secret from cluster: %w", err)
	}
	return &secret
}

func (r *AivenApplicationReconciler) FindDependentObjects(ctx context.Context, app aiven_nais_io_v1.AivenApplication, logger *log.Entry) []client.Object {
	result := make([]client.Object, 0, 10)

	// These should be kept in sync with the list in r.missingRelevantOwnerReference()
	rs := r.findReplicaSets(ctx, app, logger)
	result = append(result, rs...)

	cj := r.findCronJobs(ctx, app, logger)
	result = append(result, cj...)

	j := r.findJobs(ctx, app, logger)
	result = append(result, j...)

	return result
}

func (r *AivenApplicationReconciler) findReplicaSets(ctx context.Context, app aiven_nais_io_v1.AivenApplication, logger *log.Entry) []client.Object {
	var correlationId string
	var ok bool
	if correlationId, ok = app.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]; !ok {
		logger.Infof("AivenApplication %s missing DeploymentCorrelationID, unable to find owning ReplicaSet", app.GetName())
		return nil
	}
	var replicaSets appsv1.ReplicaSetList
	var mLabels = client.MatchingLabels{
		constants.AppLabel: app.GetName(),
	}

	err := metrics.ObserveKubernetesLatency("ReplicaSet_List", func() error {
		return r.List(ctx, &replicaSets, mLabels, client.InNamespace(app.GetNamespace()))
	})
	if err != nil {
		logger.Warnf("failed to list replicasets: %v", err)
		return nil
	}

	found := make([]client.Object, 0, len(replicaSets.Items))
	for i, rs := range replicaSets.Items {
		if rsCorrId, ok := rs.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]; ok && rsCorrId == correlationId {
			for _, volume := range rs.Spec.Template.Spec.Volumes {
				if volume.Name == AivenVolumeName && volume.Secret.SecretName == app.Spec.SecretName {
					found = append(found, &replicaSets.Items[i])
				}
			}
		}
	}

	if len(found) == 0 {
		logger.Infof("No ReplicaSet found for correlation ID %s and secret %s", correlationId, app.Spec.SecretName)
	}
	return found
}

func (r *AivenApplicationReconciler) findCronJobs(ctx context.Context, app aiven_nais_io_v1.AivenApplication, logger *log.Entry) []client.Object {
	var correlationId string
	var ok bool
	if correlationId, ok = app.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]; !ok {
		logger.Infof("AivenApplication %s missing DeploymentCorrelationID, unable to find owning CronJob", app.GetName())
		return nil
	}
	var cronJobs batchv1.CronJobList
	var mLabels = client.MatchingLabels{
		constants.AppLabel: app.GetName(),
	}

	err := metrics.ObserveKubernetesLatency("CronJob_List", func() error {
		return r.List(ctx, &cronJobs, mLabels, client.InNamespace(app.GetNamespace()))
	})
	if err != nil {
		logger.Warnf("failed to list replicasets: %v", err)
		return nil
	}

	found := make([]client.Object, 0, len(cronJobs.Items))
	for i, cronJob := range cronJobs.Items {
		if cjCorrId, ok := cronJob.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]; ok && cjCorrId == correlationId {
			for _, volume := range cronJob.Spec.JobTemplate.Spec.Template.Spec.Volumes {
				if volume.Name == AivenVolumeName && volume.Secret.SecretName == app.Spec.SecretName {
					found = append(found, &cronJobs.Items[i])
				}
			}
		}
	}

	if len(found) == 0 {
		logger.Infof("No CronJob found for correlation ID %s and secret %s", correlationId, app.Spec.SecretName)
	}
	return found
}

func (r *AivenApplicationReconciler) findJobs(ctx context.Context, app aiven_nais_io_v1.AivenApplication, logger *log.Entry) []client.Object {
	var correlationId string
	var ok bool
	if correlationId, ok = app.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]; !ok {
		logger.Infof("AivenApplication %s missing DeploymentCorrelationID, unable to find owning Job", app.GetName())
		return nil
	}
	var jobs batchv1.JobList
	var mLabels = client.MatchingLabels{
		constants.AppLabel: app.GetName(),
	}

	err := metrics.ObserveKubernetesLatency("Job_List", func() error {
		return r.List(ctx, &jobs, mLabels, client.InNamespace(app.GetNamespace()))
	})
	if err != nil {
		logger.Warnf("failed to list replicasets: %v", err)
		return nil
	}

	found := make([]client.Object, 0, len(jobs.Items))
	for i, job := range jobs.Items {
		if jobCorrId, ok := job.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]; ok && jobCorrId == correlationId {
			for _, volume := range job.Spec.Template.Spec.Volumes {
				if volume.Name == AivenVolumeName && volume.Secret.SecretName == app.Spec.SecretName {
					found = append(found, &jobs.Items[i])
				}
			}
		}
	}

	if len(found) == 0 {
		logger.Infof("No Job found for correlation ID %s and secret %s", correlationId, app.Spec.SecretName)
	}
	return found
}

func (r *AivenApplicationReconciler) HandleProtectedAndTimeLimited(ctx context.Context, application aiven_nais_io_v1.AivenApplication, logger *log.Entry) (bool, error) {
	if application.Spec.ExpiresAt == nil {
		return false, nil
	}

	parsedTimeStamp, err := utils.Parse(application.FormatExpiresAt())
	if err != nil {
		return false, fmt.Errorf("could not parse timestamp: %s", err)
	}

	if !utils.Expired(parsedTimeStamp) {
		return false, nil
	}

	logger.Infof("Application timelimit exceded: %s", parsedTimeStamp.String())
	err = r.DeleteApplication(ctx, application, logger)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (r *AivenApplicationReconciler) DeleteApplication(ctx context.Context, application aiven_nais_io_v1.AivenApplication, logger *log.Entry) error {
	err := r.Delete(ctx, &application)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Debugf("application do not exist in cluster: %s", err)
		} else {
			return fmt.Errorf("unable to delete application from cluster: %s", err)
		}
	} else {
		logger.Infof("Application deleted from cluster")
	}
	return nil
}

func success(application *aiven_nais_io_v1.AivenApplication, hash string) {
	s := &application.Status
	s.SynchronizationHash = hash
	s.SynchronizationState = rolloutComplete
	s.SynchronizedGeneration = application.GetGeneration()
	s.AddCondition(aiven_nais_io_v1.AivenApplicationCondition{
		Type:   aiven_nais_io_v1.AivenApplicationSucceeded,
		Status: corev1.ConditionTrue,
	})
	s.AddCondition(aiven_nais_io_v1.AivenApplicationCondition{
		Type:   aiven_nais_io_v1.AivenApplicationAivenFailure,
		Status: corev1.ConditionFalse,
	})
	s.AddCondition(aiven_nais_io_v1.AivenApplicationCondition{
		Type:   aiven_nais_io_v1.AivenApplicationLocalFailure,
		Status: corev1.ConditionFalse,
	})
}

func (r *AivenApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	opts := controller.Options{
		MaxConcurrentReconciles: 10,
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiven_nais_io_v1.AivenApplication{}).
		WithOptions(opts).
		WithEventFilter(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		)).
		Complete(r)
}

func (r *AivenApplicationReconciler) SaveSecret(ctx context.Context, secret *corev1.Secret, logger *log.Entry) error {
	key := client.ObjectKey{
		Namespace: secret.Namespace,
		Name:      secret.Name,
	}

	ctx, cancel := context.WithTimeout(ctx, secretWriteTimeout)
	defer cancel()

	old := &corev1.Secret{}
	err := metrics.ObserveKubernetesLatency("Secret_Get", func() error {
		return r.Get(ctx, key, old)
	})

	if err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Infof("Saving secret")
			err = metrics.ObserveKubernetesLatency("Secret_Create", func() error {
				return r.Create(ctx, secret)
			})
		}
	} else {
		logger.Infof("Updating secret")
		secret.ResourceVersion = old.ResourceVersion
		err = metrics.ObserveKubernetesLatency("Secret_Update", func() error {
			return r.Update(ctx, secret)
		})
	}

	if err == nil {
		metrics.KubernetesResourcesWritten.With(prometheus.Labels{
			metrics.LabelResourceType: "Secret",
			metrics.LabelNamespace:    secret.GetNamespace(),
		}).Inc()
	}

	return err
}

func (r *AivenApplicationReconciler) NeedsSynchronization(ctx context.Context, application aiven_nais_io_v1.AivenApplication, hash string, objs []client.Object, logger *log.Entry) (bool, error) {
	if application.Status.SynchronizationHash != hash {
		logger.Infof("Hash changed; needs synchronization")
		metrics.ProcessingReason.WithLabelValues(metrics.HashChanged.String()).Inc()
		return true, nil
	}

	old := corev1.Secret{}
	err := r.Get(ctx, application.SecretKey(), &old)
	switch {
	case k8serrors.IsNotFound(err):
		logger.Infof("Secret not found; needs synchronization")
		metrics.ProcessingReason.WithLabelValues(metrics.MissingSecret.String()).Inc()
		return true, nil
	case err != nil:
		return false, fmt.Errorf("unable to retrieve secret from cluster: %s", err)
	}

	if isProtected(old) {
		logger.Infof("Protected and already synchronized")
		return false, nil
	}

	missing, err := r.missingActualOwnerReference(objs, old)
	if err != nil {
		return false, err
	}
	if missing {
		logger.Infof("Missing ownerReference for existing object; needs synchronization")
		metrics.ProcessingReason.WithLabelValues(metrics.MissingOwnerReference.String()).Inc()
		return true, nil
	}

	if r.missingRelevantOwnerReference(ctx, old, logger) {
		logger.Infof("Missing relevant ownerReference; needs synchronization")
		metrics.ProcessingReason.WithLabelValues(metrics.MissingOwnerReference.String()).Inc()
		return true, nil
	}

	logger.Infof("Already synchronized")
	return false, nil
}

func (r *AivenApplicationReconciler) missingActualOwnerReference(objs []client.Object, old corev1.Secret) (bool, error) {
	presentOwnerReferences := make(map[v1.OwnerReference]bool, len(objs))
	for _, obj := range objs {
		ownerReference, err := utils.MakeOwnerReference(obj)
		if err != nil {
			return false, err
		}
		presentOwnerReferences[ownerReference] = false
	}
	for _, ownerReference := range old.GetOwnerReferences() {
		presentOwnerReferences[ownerReference] = true
	}

	for _, present := range presentOwnerReferences {
		if !present {
			return true, nil
		}
	}
	return false, nil
}

// missingRelevantOwnerReference checks if at least one OwnerReference points to a "pod-owning" resource with the correct correlationId
// "pod-owning" is defined as the kinds listed in the map relevantKinds
func (r *AivenApplicationReconciler) missingRelevantOwnerReference(ctx context.Context, secret corev1.Secret, logger log.FieldLogger) bool {
	correlationId, ok := secret.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]
	if !ok {
		return false
	}

	for _, ownerReference := range secret.GetOwnerReferences() {
		if relevantKinds[ownerReference.Kind] && r.ownerReferenceMatches(ctx, ownerReference, correlationId, secret.GetNamespace(), logger) {
			return false
		}
	}
	return true
}

func (r *AivenApplicationReconciler) ownerReferenceMatches(ctx context.Context, ownerReference v1.OwnerReference, correlationId string, namespace string, logger log.FieldLogger) bool {
	object := v1.PartialObjectMetadata{
		TypeMeta: v1.TypeMeta{
			Kind:       ownerReference.Kind,
			APIVersion: ownerReference.APIVersion,
		},
	}
	key := types.NamespacedName{
		Namespace: namespace,
		Name:      ownerReference.Name,
	}
	err := r.Get(ctx, key, &object)
	if err != nil {
		logger.Warnf("Unable to get referenced owner %v: %v", key, err)
		return false
	}
	referenceCorrelationId, ok := object.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation]
	if !ok {
		return false
	}
	return referenceCorrelationId == correlationId
}

func isProtected(secret corev1.Secret) bool {
	protected, ok := secret.GetAnnotations()[constants.AivenatorProtectedAnnotation]
	return ok && protected == "true"
}
