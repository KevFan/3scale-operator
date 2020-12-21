package operator

import (
	"fmt"
	"net/url"

	"github.com/3scale/3scale-operator/pkg/3scale/amp/component"
	"github.com/3scale/3scale-operator/pkg/3scale/amp/product"
	appsv1alpha1 "github.com/3scale/3scale-operator/pkg/apis/apps/v1alpha1"
	"github.com/3scale/3scale-operator/pkg/helper"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ZyncOptionsProvider struct {
	apimanager   *appsv1alpha1.APIManager
	namespace    string
	client       client.Client
	zyncOptions  *component.ZyncOptions
	secretSource *helper.SecretSource
}

func NewZyncOptionsProvider(apimanager *appsv1alpha1.APIManager, namespace string, client client.Client) *ZyncOptionsProvider {
	return &ZyncOptionsProvider{
		apimanager:   apimanager,
		namespace:    namespace,
		client:       client,
		zyncOptions:  component.NewZyncOptions(),
		secretSource: helper.NewSecretSource(client, namespace),
	}
}

func (z *ZyncOptionsProvider) GetZyncOptions() (*component.ZyncOptions, error) {
	z.zyncOptions.ImageTag = product.ThreescaleRelease
	z.zyncOptions.DatabaseImageTag = product.ThreescaleRelease

	err := z.setSecretBasedOptions()
	if err != nil {
		return nil, fmt.Errorf("GetZyncOptions reading secret options: %w", err)
	}

	z.setResourceRequirementsOptions()
	z.setNodeAffinityAndTolerationsOptions()
	z.setReplicas()

	imageOpts, err := NewAmpImagesOptionsProvider(z.apimanager).GetAmpImagesOptions()
	if err != nil {
		return nil, fmt.Errorf("GetZyncOptions reading image options: %w", err)
	}

	z.zyncOptions.CommonLabels = z.commonLabels()
	z.zyncOptions.CommonZyncLabels = z.commonZyncLabels()
	z.zyncOptions.CommonZyncQueLabels = z.commonZyncQueLabels()
	z.zyncOptions.CommonZyncDatabaseLabels = z.commonZyncDatabaseLabels()
	z.zyncOptions.ZyncPodTemplateLabels = z.zyncPodTemplateLabels(imageOpts.ZyncImage)
	z.zyncOptions.ZyncQuePodTemplateLabels = z.zyncQuePodTemplateLabels(imageOpts.ZyncImage)
	z.zyncOptions.ZyncDatabasePodTemplateLabels = z.zyncDatabasePodTemplateLabels(imageOpts.ZyncDatabasePostgreSQLImage)

	z.zyncOptions.ZyncMetrics = true

	z.zyncOptions.ZyncQueServiceAccountImagePullSecrets = z.zyncQueServiceAccountImagePullSecrets()

	z.zyncOptions.Namespace = z.apimanager.Namespace

	err = z.zyncOptions.Validate()
	if err != nil {
		return nil, fmt.Errorf("GetZyncOptions validating: %w", err)
	}
	return z.zyncOptions, nil
}

func (z *ZyncOptionsProvider) setSecretBasedOptions() error {
	// We first get the potentially existing Zync Database password value
	// so we use for the default Zync Database URL which will have a different
	// value depending on whether the Zync Database password has been autogenerated
	// or not
	var zyncDatabasePassword string
	if z.apimanager.IsZyncExternalDatabaseEnabled() {
		var err error
		zyncDatabasePassword, err = z.secretSource.RequiredFieldValueFromRequiredSecret(component.ZyncSecretName, component.ZyncSecretDatabasePasswordFieldName)
		if err != nil {
			return err
		}
	} else {
		var err error
		defaultInternalZyncDatabasePassword := component.DefaultZyncDatabasePassword()
		zyncDatabasePassword, err = z.secretSource.FieldValue(component.ZyncSecretName, component.ZyncSecretDatabasePasswordFieldName, defaultInternalZyncDatabasePassword)
		if err != nil {
			return err
		}
	}
	z.zyncOptions.DatabasePassword = zyncDatabasePassword

	cases := []struct {
		field               *string
		secretName          string
		secretField         string
		defValue            string
		secretRequiredField bool
	}{
		{
			&z.zyncOptions.SecretKeyBase,
			component.ZyncSecretName,
			component.ZyncSecretKeyBaseFieldName,
			component.DefaultZyncSecretKeyBase(),
			false,
		},
		{
			&z.zyncOptions.AuthenticationToken,
			component.ZyncSecretName,
			component.ZyncSecretAuthenticationTokenFieldName,
			component.DefaultZyncAuthenticationToken(),
			false,
		},
		{
			&z.zyncOptions.DatabaseURL,
			component.ZyncSecretName,
			component.ZyncSecretDatabaseURLFieldName,
			component.DefaultZyncDatabaseURL(zyncDatabasePassword),
			z.apimanager.IsZyncExternalDatabaseEnabled(),
		},
	}

	for _, option := range cases {
		if option.secretRequiredField {
			val, err := z.secretSource.RequiredFieldValueFromRequiredSecret(option.secretName, option.secretField)
			if err != nil {
				return err
			}
			*option.field = val
		} else {
			val, err := z.secretSource.FieldValue(option.secretName, option.secretField, option.defValue)
			if err != nil {
				return err
			}
			*option.field = val
		}
	}

	err := z.validateZyncDatabaseURLAndPasswordFieldsConsistency()
	if err != nil {
		return err
	}

	return nil
}

// Verify that the password field and the database url fields in the zync secret
// contain the same value
func (z *ZyncOptionsProvider) validateZyncDatabaseURLAndPasswordFieldsConsistency() error {
	zyncDatabaseURL, err := url.Parse(z.zyncOptions.DatabaseURL)
	if err != nil {
		return fmt.Errorf("GetZyncOptions: error parsing provided '%s' field in '%s' secret: %w", component.ZyncSecretName, component.ZyncSecretDatabaseURLFieldName, err)
	}
	zyncDatabaseURLUserInfo := zyncDatabaseURL.User
	if zyncDatabaseURLUserInfo == nil {
		return fmt.Errorf("GetZyncOptions: '%s' field in '%s' secret doesn't have required password part", component.ZyncSecretName, component.ZyncSecretDatabaseURLFieldName)
	}
	zyncDatabaseURLPasswordPart, zyncDatabaseURLHasPassword := zyncDatabaseURL.User.Password()
	if !zyncDatabaseURLHasPassword {
		return fmt.Errorf("GetZyncOptions: '%s' field in '%s' secret doesn't have required password part", component.ZyncSecretName, component.ZyncSecretDatabaseURLFieldName)
	}
	if z.zyncOptions.DatabasePassword != zyncDatabaseURLPasswordPart {
		return fmt.Errorf("GetZyncOptions: '%s' field in secret '%s' does not match password part in field '%s'. Inconsistency detected", component.ZyncSecretDatabasePasswordFieldName, component.ZyncSecretName, component.ZyncSecretDatabaseURLFieldName)
	}
	return nil
}

func (z *ZyncOptionsProvider) setResourceRequirementsOptions() {
	if *z.apimanager.Spec.ResourceRequirementsEnabled {
		z.zyncOptions.ContainerResourceRequirements = component.DefaultZyncContainerResourceRequirements()
		z.zyncOptions.QueContainerResourceRequirements = component.DefaultZyncQueContainerResourceRequirements()
		z.zyncOptions.DatabaseContainerResourceRequirements = component.DefaultZyncDatabaseContainerResourceRequirements()
	} else {
		z.zyncOptions.ContainerResourceRequirements = v1.ResourceRequirements{}
		z.zyncOptions.QueContainerResourceRequirements = v1.ResourceRequirements{}
		z.zyncOptions.DatabaseContainerResourceRequirements = v1.ResourceRequirements{}
	}

	// DeploymentConfig-level ResourceRequirements CR fields have priority over
	// spec.resourceRequirementsEnabled, overwriting that setting when they are
	// defined
	if z.apimanager.Spec.Zync.AppSpec.Resources != nil {
		z.zyncOptions.ContainerResourceRequirements = *z.apimanager.Spec.Zync.AppSpec.Resources
	}
	if z.apimanager.Spec.Zync.QueSpec.Resources != nil {
		z.zyncOptions.QueContainerResourceRequirements = *z.apimanager.Spec.Zync.QueSpec.Resources
	}
	if z.apimanager.Spec.Zync.DatabaseResources != nil {
		z.zyncOptions.DatabaseContainerResourceRequirements = *z.apimanager.Spec.Zync.DatabaseResources
	}
}

func (z *ZyncOptionsProvider) setNodeAffinityAndTolerationsOptions() {
	z.zyncOptions.ZyncAffinity = z.apimanager.Spec.Zync.AppSpec.Affinity
	z.zyncOptions.ZyncTolerations = z.apimanager.Spec.Zync.AppSpec.Tolerations
	z.zyncOptions.ZyncQueAffinity = z.apimanager.Spec.Zync.QueSpec.Affinity
	z.zyncOptions.ZyncQueTolerations = z.apimanager.Spec.Zync.QueSpec.Tolerations
	z.zyncOptions.ZyncDatabaseAffinity = z.apimanager.Spec.Zync.DatabaseAffinity
	z.zyncOptions.ZyncDatabaseTolerations = z.apimanager.Spec.Zync.DatabaseTolerations
}

func (z *ZyncOptionsProvider) setReplicas() {
	z.zyncOptions.ZyncReplicas = int32(*z.apimanager.Spec.Zync.AppSpec.Replicas)
	z.zyncOptions.ZyncQueReplicas = int32(*z.apimanager.Spec.Zync.QueSpec.Replicas)
}

func (z *ZyncOptionsProvider) commonLabels() map[string]string {
	return map[string]string{
		"app":                  *z.apimanager.Spec.AppLabel,
		"threescale_component": "zync",
	}
}

func (z *ZyncOptionsProvider) commonZyncLabels() map[string]string {
	labels := z.commonLabels()
	labels["threescale_component_element"] = "zync"
	return labels
}

func (z *ZyncOptionsProvider) commonZyncQueLabels() map[string]string {
	labels := z.commonLabels()
	labels["threescale_component_element"] = "zync-que"
	return labels
}

func (z *ZyncOptionsProvider) commonZyncDatabaseLabels() map[string]string {
	labels := z.commonLabels()
	labels["threescale_component_element"] = "database"
	return labels
}

func (z *ZyncOptionsProvider) zyncPodTemplateLabels(image string) map[string]string {
	labels := helper.MeteringLabels("zync", helper.ParseVersion(image), helper.ApplicationType)

	for k, v := range z.commonZyncLabels() {
		labels[k] = v
	}

	labels["deploymentConfig"] = "zync"

	return labels
}

func (z *ZyncOptionsProvider) zyncQuePodTemplateLabels(image string) map[string]string {
	labels := helper.MeteringLabels("zync-que", helper.ParseVersion(image), helper.ApplicationType)

	for k, v := range z.commonZyncQueLabels() {
		labels[k] = v
	}

	labels["deploymentConfig"] = "zync-que"

	return labels
}

func (z *ZyncOptionsProvider) zyncDatabasePodTemplateLabels(image string) map[string]string {
	labels := helper.MeteringLabels("zync-database", helper.ParseVersion(image), helper.ApplicationType)

	for k, v := range z.commonZyncDatabaseLabels() {
		labels[k] = v
	}

	labels["deploymentConfig"] = "zync-database"

	return labels
}

func (z *ZyncOptionsProvider) zyncQueServiceAccountImagePullSecrets() []v1.LocalObjectReference {
	if z.apimanager.Spec.ImagePullSecrets != nil {
		return z.apimanager.Spec.ImagePullSecrets
	}

	return component.DefaultZyncQueServiceAccountImagePullSecrets()
}
