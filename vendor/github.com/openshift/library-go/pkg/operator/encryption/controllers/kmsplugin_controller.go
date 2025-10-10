package controllers

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticpod/kmsplugin"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	kmsPluginControllerDegradedCondition = "EncryptionKMSPluginControllerDegraded"

	kmsPluginPodBaseName = "%s-kms-plugin"
)

type kmsPluginPodTemplateBuilderFunc func(targetHash, targetNamespace, image, keyID, region, listen string) (string, error)

type kmsPluginController struct {
	instanceName           string
	targetNamespace        string
	controllerInstanceName string

	operatorClient   operatorv1helpers.OperatorClient
	apiserverClient  configv1client.APIServerInterface
	podLister        corev1lister.PodLister
	configMapsClient corev1client.ConfigMapsGetter

	provider                 Provider
	preconditionsFulfilledFn preconditionsFulfilled
	podTemplateBuilderFunc   kmsPluginPodTemplateBuilderFunc
	eventRecorder            events.Recorder
}

// NewKMSPluginController creates a new instance of the controller which manages
// the config map used to manage static pods for the configured kms plugin.
//
// podTemplateBuilderFunc defaults to kmsplugin.GenerateAWSProviderTemplate when nil.
func NewKMSPluginController(
	targetNamespace string,
	provider Provider,
	preconditionsFulfilledFn preconditionsFulfilled,
	podTemplateBuilderFunc kmsPluginPodTemplateBuilderFunc,
	apiserverClient configv1client.APIServerInterface,
	operatorClient operatorv1helpers.OperatorClient,
	apiServerInformer configv1informers.APIServerInformer,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	configMapsClient corev1client.ConfigMapsGetter,
	eventRecorder events.Recorder,
) factory.Controller {
	c := kmsPluginController{
		targetNamespace:        targetNamespace,
		controllerInstanceName: factory.ControllerInstanceName(targetNamespace, "KMSPlugin"),

		operatorClient:   operatorClient,
		apiserverClient:  apiserverClient,
		configMapsClient: configMapsClient,

		provider:                 provider,
		preconditionsFulfilledFn: preconditionsFulfilledFn,
		podTemplateBuilderFunc:   podTemplateBuilderFunc,
		eventRecorder:            eventRecorder,
	}

	if c.podTemplateBuilderFunc == nil {
		c.podTemplateBuilderFunc = kmsplugin.GenerateAWSProviderTemplate
	}

	return factory.New().
		WithSync(c.sync).
		WithControllerInstanceName(c.controllerInstanceName).
		ResyncEvery(time.Minute).
		WithInformers(
			apiServerInformer.Informer(), // watch for encryption configuration changes
			operatorClient.Informer(),    // watch for management state changes
		).ToController(
		c.controllerInstanceName,
		eventRecorder.WithComponentSuffix("kms-plugin-controller"),
	)
}

func (c *kmsPluginController) sync(ctx context.Context, syncCtx factory.SyncContext) (err error) {
	// TODO(fmissi):
	//  * check plugin health (Status) and update conditions accordingly
	//  * cleanup plugin resources if encryption config changes from KMS to a
	//    different encryption provider, i.e. AESCBC
	//  * provide means for key_controller to determine whether the KMS plugin pods are operational,
	//    potentially via conditions, or maybe even via direct call to plugin Status.

	// The status for this condition is intentionally omitted to ensure it's correctly set in each branch
	degradedCondition := applyoperatorv1.OperatorCondition().
		WithType(kmsPluginControllerDegradedCondition)

	defer func() {
		if degradedCondition == nil {
			return
		}
		status := applyoperatorv1.OperatorStatus().WithConditions(degradedCondition)
		if applyError := c.operatorClient.ApplyOperatorStatus(ctx, c.controllerInstanceName, status); applyError != nil {
			err = applyError
		}
	}()

	if ready, err := shouldRunEncryptionController(c.operatorClient, c.preconditionsFulfilledFn, c.provider.ShouldRunEncryptionControllers); err != nil || !ready {
		if err != nil {
			degradedCondition = nil
		} else {
			degradedCondition = degradedCondition.
				WithStatus(operatorv1.ConditionFalse)
		}
		return err // we will get re-kicked when the operator status updates
	}

	apiServer, err := c.apiserverClient.Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return err
	}
	kmsConfig := apiServer.Spec.Encryption.KMS
	if kmsConfig == nil {
		return nil
	}
	switch kmsConfig.Type {
	case configv1.AWSKMSProvider:
		name := "kms-plugin-pod"
		currentcm, err := c.configMapsClient.ConfigMaps(c.targetNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil && !kerrors.IsNotFound(err) {
			return err
		}
		desiredPodManifest, err := c.podTemplateBuilderFunc(
			"hash-todo",
			c.targetNamespace,
			"quay.io/image/todo:latest",
			kmsConfig.AWS.KeyARN,
			kmsConfig.AWS.Region,
			":8080",
		)
		if err != nil {
			return err
		}
		currentcmOutdated := false
		if currentcm.Data["pod.yaml"] != desiredPodManifest {
			currentcmOutdated = true
		}
		if currentcmOutdated {
			desiredcm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: c.targetNamespace,
					// Labels: map[string]string{}, // TODO
				},
				Data: map[string]string{
					"pod.yaml": desiredPodManifest,
				},
			}
			if _, _, err := resourceapply.ApplyConfigMap(ctx, c.configMapsClient, c.eventRecorder, desiredcm); err != nil {
				return err
			}
		}
	default:
		// error
	}

	return nil
}

func (c *kmsPluginController) getPluginBaseName(kmsProvider configv1.KMSProviderType) string {
	p := strings.ToLower(string(kmsProvider))
	return fmt.Sprintf(kmsPluginPodBaseName, p)
}

func (c *kmsPluginController) getPluginPodSelector(kmsConfig *configv1.KMSConfig) labels.Selector {
	return labels.Set{
		"app": c.getPluginBaseName(kmsConfig.Type),
	}.AsSelector()
}
