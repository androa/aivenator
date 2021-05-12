package aiven_application

import (
	"context"
	"fmt"
	"github.com/nais/aivenator/pkg/credentials"
	"github.com/nais/aivenator/pkg/metrics"
	"github.com/nais/aivenator/pkg/utils"
	kafka_nais_io_v1 "github.com/nais/liberator/pkg/apis/kafka.nais.io/v1"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"time"
)

const (
	requeueInterval    = time.Second * 10
	secretWriteTimeout = time.Second * 2

	rolloutComplete = "RolloutComplete"
	rolloutFailed   = "RolloutFailed"
)

type AivenApplicationReconciler struct {
	client.Client
	Logger  *log.Entry
	Manager credentials.Manager
}

func (r *AivenApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var application kafka_nais_io_v1.AivenApplication

	logger := r.Logger.WithFields(log.Fields{
		"aiven_application": req.Name,
		"namespace":         req.Namespace,
	})

	logger.Infof("Processing request")
	defer func() {
		logger.Infof("Finished processing request")
		syncState := application.Status.SynchronizationState
		if len(syncState) > 0 {
			metrics.ApplicationsProcessed.With(prometheus.Labels{
				metrics.LabelSyncState: syncState,
			}).Inc()
		}
	}()

	fail := func(err error, requeue bool) (ctrl.Result, error) {
		if err != nil {
			logger.Error(err)
		}
		application.Status.SynchronizationState = rolloutFailed
		cr := ctrl.Result{}
		if requeue {
			cr.RequeueAfter = requeueInterval
		}
		return cr, nil
	}

	err := r.Get(ctx, req.NamespacedName, &application)
	switch {
	case errors.IsNotFound(err):
		return fail(fmt.Errorf("resource deleted from cluster; noop"), false)
	case err != nil:
		return fail(fmt.Errorf("unable to retrieve resource from cluster: %s", err), true)
	}

	logger = logger.WithFields(log.Fields{
		"secret_name": application.Spec.SecretName,
	})

	logger.Infof("Application exists; processing")
	defer func() {
		application.Status.SynchronizationTime = &v1.Time{time.Now()}
		application.Status.ObservedGeneration = application.GetGeneration()
		err := r.Status().Update(ctx, &application)
		if err != nil {
			logger.Errorf("Unable to update status of application: %s\nWanted to save status: %+v", err, application.Status)
		} else {
			metrics.KubernetesResourcesWritten.With(prometheus.Labels{
				metrics.LabelResourceType: application.GroupVersionKind().String(),
				metrics.LabelNamespace:    application.GetNamespace(),
			}).Inc()
		}
	}()

	hash, err := application.Hash()
	if err != nil {
		utils.LocalFail("Hash", &application, err, logger)
		return fail(nil, true)
	}

	needsSync, err := r.NeedsSynchronization(ctx, application, hash, logger)
	if err != nil {
		utils.LocalFail("NeedsSynchronization", &application, err, logger)
		return fail(nil, true)
	}

	if !needsSync {
		return ctrl.Result{}, nil
	}

	logger.Infof("Creating secret")
	secret, err := r.Manager.CreateSecret(&application, logger)
	if err != nil {
		utils.LocalFail("CreateSecret", &application, err, logger)
		return fail(nil, true)
	}

	logger.Infof("Saving secret to cluster")
	err = r.SaveSecret(ctx, secret, logger)
	if err != nil {
		utils.LocalFail("SaveSecret", &application, err, logger)
		return fail(nil, true)
	}

	success(&application, hash)

	return ctrl.Result{}, nil
}

func success(application *kafka_nais_io_v1.AivenApplication, hash string) {
	s := &application.Status
	s.SynchronizationHash = hash
	s.SynchronizationState = rolloutComplete
	s.SynchronizedGeneration = application.GetGeneration()
	s.AddCondition(kafka_nais_io_v1.AivenApplicationCondition{
		Type:   kafka_nais_io_v1.AivenApplicationSucceeded,
		Status: corev1.ConditionTrue,
	})
	s.AddCondition(kafka_nais_io_v1.AivenApplicationCondition{
		Type:   kafka_nais_io_v1.AivenApplicationAivenFailure,
		Status: corev1.ConditionFalse,
	})
	s.AddCondition(kafka_nais_io_v1.AivenApplicationCondition{
		Type:   kafka_nais_io_v1.AivenApplicationLocalFailure,
		Status: corev1.ConditionFalse,
	})
}

func (r *AivenApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kafka_nais_io_v1.AivenApplication{}).
		WithEventFilter(predicate.Funcs{
			DeleteFunc: func(event event.DeleteEvent) bool {
				return false // The secrets will get deleted because of OwnerReference, and no other cleanup is needed
			},
			UpdateFunc: func(updateEvent event.UpdateEvent) bool {
				return updateEvent.ObjectNew.GetGeneration() > updateEvent.ObjectOld.GetGeneration()
			},
		}).
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
	err := r.Get(ctx, key, old)

	if err != nil {
		if errors.IsNotFound(err) {
			logger.Infof("Creating secret")
			err = r.Create(ctx, secret)
		}
	} else {
		logger.Infof("Updating secret")
		secret.ResourceVersion = old.ResourceVersion
		err = r.Update(ctx, secret)
	}

	if err == nil {
		metrics.KubernetesResourcesWritten.With(prometheus.Labels{
			metrics.LabelResourceType: secret.GroupVersionKind().String(),
			metrics.LabelNamespace:    secret.GetNamespace(),
		}).Inc()
	}

	return err
}

func (r *AivenApplicationReconciler) NeedsSynchronization(ctx context.Context, application kafka_nais_io_v1.AivenApplication, hash string, logger *log.Entry) (bool, error) {
	if application.Status.SynchronizationHash != hash {
		logger.Infof("Hash changed; needs synchronization")
		return true, nil
	}

	key := client.ObjectKey{
		Namespace: application.GetNamespace(),
		Name:      application.Spec.SecretName,
	}
	old := corev1.Secret{}
	err := r.Get(ctx, key, &old)
	switch {
	case errors.IsNotFound(err):
		logger.Infof("Secret not found; needs synchronization")
		return true, nil
	case err != nil:
		return false, fmt.Errorf("unable to retrieve secret from cluster: %s", err)
	}

	logger.Infof("Already synchronized")
	return false, nil
}
