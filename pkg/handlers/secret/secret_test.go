package secret

import (
	"errors"
	"github.com/nais/aivenator/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"

	aiven_nais_io_v1 "github.com/nais/liberator/pkg/apis/aiven.nais.io/v1"
	nais_io_v1 "github.com/nais/liberator/pkg/apis/nais.io/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	"github.com/nais/aivenator/constants"
)

const (
	namespace       = "ns"
	applicationName = "app"
	secretName      = "my-secret"
	correlationId   = "correlation-id"
)

func TestHandler_Apply(t *testing.T) {
	type args struct {
		application         aiven_nais_io_v1.AivenApplication
		objects             []client.Object
		secret              corev1.Secret
		assert              func(*testing.T, args)
		assertUnrecoverable bool
	}
	exampleAivenApplication := aiven_nais_io_v1.NewAivenApplicationBuilder(applicationName, namespace).
		WithSpec(aiven_nais_io_v1.AivenApplicationSpec{SecretName: secretName}).
		Build()
	tests := []struct {
		name string
		args args
	}{
		{
			name: "BaseApplication",
			args: args{
				application: exampleAivenApplication,
				objects:     nil,
				secret:      corev1.Secret{},
				assert: func(t *testing.T, a args) {
					assert.Equal(t, constants.AivenatorSecretType, a.secret.Labels[constants.SecretTypeLabel])
					assert.Equal(t, a.application.GetName(), a.secret.Labels[constants.AppLabel])
					assert.Equal(t, a.application.GetNamespace(), a.secret.Labels[constants.TeamLabel])
					assert.Equal(t, a.application.GetNamespace(), a.secret.GetNamespace())
				},
			},
		},
		{
			name: "ApplicationWithSecretAndCorrelationId",
			args: args{
				application: aiven_nais_io_v1.NewAivenApplicationBuilder(applicationName, namespace).
					WithSpec(aiven_nais_io_v1.AivenApplicationSpec{SecretName: secretName}).
					WithAnnotation(nais_io_v1.DeploymentCorrelationIDAnnotation, correlationId).
					Build(),
				objects: nil,
				secret:  corev1.Secret{},
				assert: func(t *testing.T, a args) {
					assert.Equal(t, correlationId, a.secret.GetAnnotations()[nais_io_v1.DeploymentCorrelationIDAnnotation])
					assert.Equal(t, a.application.Spec.SecretName, a.secret.GetName())
				},
			},
		},
		{
			name: "PreexistingSecret",
			args: args{
				application: exampleAivenApplication,
				secret: corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:            secretName,
						Namespace:       namespace,
						Labels:          map[string]string{"pre-existing-label": "pre-existing-label"},
						Annotations:     map[string]string{"pre-existing-annotation": "pre-existing-annotation"},
						OwnerReferences: []metav1.OwnerReference{{Name: "pre-existing-owner-reference"}},
						Finalizers:      []string{"pre-existing-finalizer"},
					},
				},
				assert: func(t *testing.T, a args) {
					assert.Contains(t, a.secret.Labels, "pre-existing-label", "existing label missing")
					assert.Contains(t, a.secret.Labels, constants.AppLabel, "new label missing")
					assert.Contains(t, a.secret.Annotations, "pre-existing-annotation", "existing annotation missing")
					assert.Contains(t, a.secret.Annotations, nais_io_v1.DeploymentCorrelationIDAnnotation, "new annotation missing")
					assert.Contains(t, a.secret.Finalizers, "pre-existing-finalizer", "existing finalizer missing")
					assert.Contains(t, a.secret.OwnerReferences, metav1.OwnerReference{Name: "pre-existing-owner-reference"}, "pre-existing ownerReference missing")

					assert.Len(t, a.secret.OwnerReferences, 1, "additional ownerReferences set")
				},
			},
		},
		{
			name: "ProtectedSecret",
			args: args{
				application: aiven_nais_io_v1.NewAivenApplicationBuilder(applicationName, namespace).
					WithSpec(aiven_nais_io_v1.AivenApplicationSpec{SecretName: secretName, Protected: true}).
					Build(),
				objects: nil,
				secret:  corev1.Secret{},
				assert: func(t *testing.T, a args) {
					assert.Equal(t, "true", a.secret.GetAnnotations()[constants.AivenatorProtectedAnnotation])
				},
			},
		},
		{
			name: "HasTimestamp",
			args: args{
				application: exampleAivenApplication,
				objects:     nil,
				secret:      corev1.Secret{},
				assert: func(t *testing.T, a args) {
					value := a.secret.StringData[AivenSecretUpdatedKey]
					timestamp, err := time.Parse(time.RFC3339, value)
					assert.NoError(t, err)
					assert.WithinDuration(t, time.Now(), timestamp, time.Second*10)
				},
			},
		},
		{
			name: "EmptySecretName",
			args: args{
				application: aiven_nais_io_v1.NewAivenApplicationBuilder(applicationName, namespace).
					Build(),
				objects:             nil,
				secret:              corev1.Secret{},
				assertUnrecoverable: true,
			},
		},
		{
			name: "InvalidSecretName",
			args: args{
				application: aiven_nais_io_v1.NewAivenApplicationBuilder(applicationName, namespace).
					WithSpec(aiven_nais_io_v1.AivenApplicationSpec{SecretName: "my_super_(c@@LS_ecE43109*23"}).
					Build(),
				objects:             nil,
				secret:              corev1.Secret{},
				assertUnrecoverable: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Handler{}
			err := s.Apply(&tt.args.application, &tt.args.secret, nil)

			if tt.args.assertUnrecoverable {
				assert.Error(t, err)
				assert.True(t, errors.Is(err, utils.UnrecoverableError))
			} else {
				assert.NoError(t, err)
			}

			if tt.args.assert != nil {
				tt.args.assert(t, tt.args)
			}
		})
	}
}
